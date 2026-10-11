package networking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// NewMCRouter uses the private REST API of mc-router. PostgreSQL is the durable
// source of desired routes; RunGateway restores mappings after router restart.
func NewMCRouter(baseURL string) *Gateway {
	g := New(baseURL)
	g.provider = "mc-router"
	return g
}

type mcRouterRoute struct {
	Backend string `json:"backend"`
}

type mcRouterHTTPError struct {
	Status int
}

func (e *mcRouterHTTPError) Error() string {
	return fmt.Sprintf("mc-router API: HTTP %d", e.Status)
}

func (g *Gateway) mcRequest(ctx context.Context, method, path string, input, output any) error {
	u, err := url.Parse(g.baseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("задайте приватный URL mc-router API без пути, например http://mc-router:8080")
	}
	var payload []byte
	if input != nil {
		payload, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("шлюз mc-router недоступен: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return err
	}
	if len(body) > 2*1024*1024 {
		return errors.New("mc-router API вернул слишком большой ответ")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &mcRouterHTTPError{Status: response.StatusCode}
	}
	if output != nil {
		if err := json.Unmarshal(body, output); err != nil {
			return fmt.Errorf("неверный JSON mc-router API: %w", err)
		}
	}
	return nil
}

func (g *Gateway) readMCRouter(ctx context.Context) (map[string]mcRouterRoute, error) {
	var routes map[string]mcRouterRoute
	if err := g.mcRequest(ctx, http.MethodGet, "/routes", nil, &routes); err != nil {
		return nil, err
	}
	if routes == nil {
		return nil, errors.New("mc-router API не предоставил объект маршрутов")
	}
	canonical := make(map[string]mcRouterRoute, len(routes))
	for domain, route := range routes {
		key := strings.ToLower(domain)
		if _, exists := canonical[key]; exists {
			return nil, fmt.Errorf("mc-router содержит повторяющийся домен %s", key)
		}
		canonical[key] = route
	}
	return canonical, nil
}

// CheckClaims checks new mc-router domains before the caller durably records
// their ownership in PostgreSQL. Committing claims before Apply allows recovery
// if the panel exits after a POST but before saving its reconciliation result.
// The mc-router API must remain private with MP as its only writer.
func (g *Gateway) CheckClaims(ctx context.Context, previousDomains []string, routes []Route) error {
	if g.provider != "mc-router" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(routes) > 2000 || len(previousDomains) > 4000 {
		return errors.New("слишком много маршрутов шлюза")
	}
	owned := make(map[string]bool, len(previousDomains))
	for _, domain := range previousDomains {
		if !hostname(domain, true) || strings.EqualFold(domain, DisabledHost) {
			return errors.New("неверный предыдущий домен MP")
		}
		owned[strings.ToLower(domain)] = true
	}
	desired := make(map[string]bool, len(routes))
	for _, route := range routes {
		if err := Validate(route.Domain, route.Backend); err != nil {
			return err
		}
		domain := strings.ToLower(route.Domain)
		if desired[domain] {
			return fmt.Errorf("домен %s назначен нескольким серверам", domain)
		}
		desired[domain] = true
	}
	current, err := g.readMCRouter(ctx)
	if err != nil {
		return err
	}
	for domain := range current {
		if desired[domain] && !owned[domain] {
			return fmt.Errorf("домен %s уже принадлежит внешнему маршруту mc-router", domain)
		}
	}
	return nil
}

// applyMCRouter replaces only exact domains owned by MP. mc-router has no
// compare-and-swap or batch transaction, so its private API must have one writer.
// A partial batch retains ownership through AppliedError and converges on retry.
func (g *Gateway) applyMCRouter(ctx context.Context, previousDomains []string, routes []Route) error {
	if len(routes) > 2000 || len(previousDomains) > 4000 {
		return errors.New("слишком много маршрутов шлюза")
	}
	owned := make(map[string]bool, len(previousDomains))
	for _, domain := range previousDomains {
		if !hostname(domain, true) || strings.EqualFold(domain, DisabledHost) {
			return errors.New("неверный предыдущий домен MP")
		}
		owned[strings.ToLower(domain)] = true
	}
	desired := make(map[string]string, len(routes))
	for _, route := range routes {
		if err := Validate(route.Domain, route.Backend); err != nil {
			return err
		}
		domain := strings.ToLower(route.Domain)
		if _, exists := desired[domain]; exists {
			return fmt.Errorf("домен %s назначен нескольким серверам", domain)
		}
		desired[domain] = route.Backend
	}
	current, err := g.readMCRouter(ctx)
	if err != nil {
		return err
	}
	// Validate the complete batch before mutating anything. Existing unrelated
	// routes are preserved even when their backend happens to match our request.
	for domain := range current {
		key := strings.ToLower(domain)
		if _, exists := desired[key]; exists && !owned[key] {
			return fmt.Errorf("домен %s уже принадлежит внешнему маршруту mc-router", domain)
		}
	}
	domains := make([]string, 0, len(desired))
	for domain := range desired {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	changed := false
	failed := func(err error, uncertain bool) error {
		if changed || uncertain {
			return &AppliedError{Message: "Синхронизация mc-router выполнена частично или её подтверждение потеряно; панель повторит попытку: " + err.Error(), Uncertain: uncertain}
		}
		return err
	}
	for _, domain := range domains {
		if active, exists := current[domain]; exists && active.Backend == desired[domain] && !g.needsPersist {
			continue
		}
		err := g.mcRequest(ctx, http.MethodPost, "/routes", map[string]string{"serverAddress": domain, "backend": desired[domain]}, nil)
		if err != nil {
			var response *mcRouterHTTPError
			return failed(err, !errors.As(err, &response))
		}
		changed = true
	}
	removed := []string{}
	for domain := range current {
		key := strings.ToLower(domain)
		if owned[key] {
			if _, keep := desired[key]; !keep {
				removed = append(removed, domain)
			}
		}
	}
	sort.Strings(removed)
	for _, domain := range removed {
		err := g.mcRequest(ctx, http.MethodDelete, "/routes/"+url.PathEscape(domain), nil, nil)
		if err != nil {
			var response *mcRouterHTTPError
			if errors.As(err, &response) && response.Status == http.StatusNotFound {
				continue
			}
			return failed(err, !errors.As(err, &response))
		}
		changed = true
	}
	if !changed {
		g.needsPersist = false
		return nil
	}
	after, err := g.readMCRouter(ctx)
	if err != nil {
		return failed(err, true)
	}
	for domain, backend := range desired {
		if after[domain].Backend != backend {
			return failed(fmt.Errorf("маршрут %s не подтверждён", domain), true)
		}
	}
	for _, domain := range removed {
		if _, exists := after[domain]; exists {
			return failed(fmt.Errorf("удаление маршрута %s не подтверждено", domain), true)
		}
	}
	g.needsPersist = false
	return nil
}
