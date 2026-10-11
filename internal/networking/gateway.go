// Package networking manages explicit Minecraft hostname routes through game gateways.
package networking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const DisabledHost = "mp-disabled.invalid"

type Route struct {
	ServerID  string `json:"server_id"`
	Name      string `json:"name,omitempty"`
	External  bool   `json:"external,omitempty"`
	Domain    string `json:"domain"`
	Backend   string `json:"backend"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updated_at"`
}
type Gateway struct {
	baseURL      string
	provider     string
	client       *http.Client
	mu           sync.Mutex
	needsPersist bool
}

// AppliedError indicates an applied or partially applied batch, or a mutation
// whose acknowledgement is uncertain. Callers must record ownership of desired
// domains, retain the warning, and retry synchronization.
type AppliedError struct {
	Message   string
	Uncertain bool
}

func (e *AppliedError) Error() string { return e.Message }

func New(baseURL string) *Gateway {
	return &Gateway{baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("gateway API redirects are not permitted")
	}}}
}

// RequirePersistence restores an outstanding disk-save retry after API restart.
func (g *Gateway) RequirePersistence() {
	g.mu.Lock()
	g.needsPersist = true
	g.mu.Unlock()
}

type rpcError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"-"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("Gate API: %s (%s)", e.Message, e.Code) }

func (g *Gateway) rpc(ctx context.Context, method string, input, output any) error {
	u, err := url.Parse(g.baseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("задайте полный приватный URL Gate API без пути, например http://gate:8080")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/minekube.gate.v1.GateService/"+method, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Timeout-Ms", "9000")
	response, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("шлюз Gate недоступен: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return err
	}
	if len(body) > 2*1024*1024 {
		return errors.New("Gate API вернул слишком большой ответ")
	}
	if response.StatusCode != http.StatusOK {
		failure := rpcError{HTTPStatus: response.StatusCode}
		if json.Unmarshal(body, &failure) != nil || failure.Message == "" {
			failure.Message = fmt.Sprintf("HTTP %d", response.StatusCode)
		}
		return &failure
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("неверный JSON Gate API: %w", err)
	}
	return nil
}

type snapshot struct {
	Payload string `json:"payload"`
	Version string `json:"version"`
}
type effective struct {
	Config struct {
		Lite struct {
			Enabled bool             `yaml:"enabled"`
			Routes  []map[string]any `yaml:"routes"`
		} `yaml:"lite"`
	} `yaml:"config"`
}

func (g *Gateway) read(ctx context.Context) (snapshot, effective, error) {
	var raw snapshot
	var cfg effective
	if err := g.rpc(ctx, "GetConfig", struct{}{}, &raw); err != nil {
		return raw, cfg, err
	}
	if raw.Version == "" || raw.Payload == "" {
		return raw, cfg, errors.New("Gate не предоставил версию конфигурации; требуется Gate v0.74.27 или совместимая версия")
	}
	if err := yaml.Unmarshal([]byte(raw.Payload), &cfg); err != nil {
		return raw, cfg, fmt.Errorf("не удалось прочитать YAML Gate: %w", err)
	}
	if !cfg.Config.Lite.Enabled {
		return raw, cfg, errors.New("Gate Lite должен быть включён до настройки маршрутов")
	}
	return raw, cfg, nil
}
func (g *Gateway) Check(ctx context.Context) error {
	if g.provider == "mc-router" {
		_, err := g.readMCRouter(ctx)
		return err
	}
	_, _, err := g.read(ctx)
	return err
}

var hostLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func hostname(value string, requireDot bool) bool {
	if len(value) == 0 || len(value) > 253 || strings.HasSuffix(value, ".") {
		return false
	}
	labels := strings.Split(value, ".")
	if requireDot && len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// Validate rejects wildcard routes and accepts explicit DNS names and host:port backends.
func Validate(domain, backend string) error {
	if !hostname(domain, true) || net.ParseIP(domain) != nil || strings.EqualFold(domain, DisabledHost) {
		return errors.New("укажите явный DNS-домен без wildcard, например survival.example.com")
	}
	host, port, err := net.SplitHostPort(backend)
	if err != nil {
		return errors.New("backend должен быть адресом host:port или [IPv6]:port")
	}
	if net.ParseIP(host) == nil && !hostname(host, false) {
		return errors.New("неверный адрес backend")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("порт backend должен быть от 1 до 65535")
	}
	return nil
}

func hosts(value any) ([]string, error) {
	switch x := value.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := []string{}
		for _, item := range x {
			v, ok := item.(string)
			if !ok {
				return nil, errors.New("Gate route host содержит не строковое значение")
			}
			out = append(out, v)
		}
		return out, nil
	case []string:
		return x, nil
	default:
		return nil, errors.New("неизвестный формат host в существующем маршруте Gate")
	}
}
func normalize(value any) (any, error) {
	switch x := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, nested := range x {
			v, err := normalize(nested)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for key, nested := range x {
			k, ok := key.(string)
			if !ok {
				return nil, errors.New("в маршруте Gate есть нестроковый YAML ключ")
			}
			v, err := normalize(nested)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, nested := range x {
			v, err := normalize(nested)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	default:
		return value, nil
	}
}

// Apply replaces only exact hostnames previously owned by MP, preserving unrelated
// routes and their backend/options. The caller must retain previousDomains until
// synchronization succeeds, including when deleting or renaming a route.
func (g *Gateway) Apply(ctx context.Context, previousDomains []string, routes []Route) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.provider == "mc-router" {
		return g.applyMCRouter(ctx, previousDomains, routes)
	}
	if len(routes) > 2000 || len(previousDomains) > 4000 {
		return errors.New("слишком много маршрутов шлюза")
	}
	owned := map[string]bool{}
	for _, domain := range previousDomains {
		if !hostname(domain, true) || strings.EqualFold(domain, DisabledHost) {
			return errors.New("неверный предыдущий домен MP")
		}
		owned[strings.ToLower(domain)] = true
	}
	desired := map[string]bool{}
	managed := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		if err := Validate(route.Domain, route.Backend); err != nil {
			return err
		}
		domain := strings.ToLower(route.Domain)
		if desired[domain] {
			return fmt.Errorf("домен %s назначен нескольким серверам", domain)
		}
		desired[domain] = true
		managed = append(managed, map[string]any{"host": domain, "backend": route.Backend})
	}
	for attempt := 0; attempt < 3; attempt++ {
		raw, cfg, err := g.read(ctx)
		if err != nil {
			return err
		}
		combined := append([]map[string]any{}, managed...)
		for _, existing := range cfg.Config.Lite.Routes {
			values, err := hosts(existing["host"])
			if err != nil {
				return err
			}
			remaining := []string{}
			for _, host := range values {
				key := strings.ToLower(host)
				if owned[key] {
					continue
				}
				if desired[key] {
					return fmt.Errorf("домен %s уже принадлежит внешнему маршруту Gate", host)
				}
				remaining = append(remaining, host)
			}
			if len(remaining) == 0 {
				continue
			}
			canonical, err := normalize(existing)
			if err != nil {
				return err
			}
			route := canonical.(map[string]any)
			if len(remaining) == 1 {
				route["host"] = remaining[0]
			} else {
				route["host"] = remaining
			}
			combined = append(combined, route)
		}
		// Gate v0.74.27 rejects empty Lite configurations. This explicit inert
		// route allows the last real route to be deleted without accepting wildcards.
		if len(combined) == 0 {
			combined = append(combined, map[string]any{"host": DisabledHost, "backend": "127.0.0.1:1"})
		}
		currentJSON, err := json.Marshal(cfg.Config.Lite.Routes)
		if err != nil {
			return err
		}
		candidateJSON, err := json.Marshal(combined)
		if err != nil {
			return err
		}
		if bytes.Equal(currentJSON, candidateJSON) && !g.needsPersist {
			return nil
		}
		patch, err := json.Marshal(map[string]any{"config": map[string]any{"lite": map[string]any{"routes": combined}}})
		if err != nil {
			return err
		}
		var result struct {
			Warnings []string `json:"warnings"`
			Version  string   `json:"version"`
		}
		err = g.rpc(ctx, "ApplyConfig", map[string]any{"mergePatch": string(patch), "ifMatch": raw.Version, "persist": true}, &result)
		if err != nil {
			var failure *rpcError
			if errors.As(err, &failure) && failure.Code == "failed_precondition" && strings.Contains(failure.Message, "version does not match") {
				continue
			}
			// A broken connection may occur after Gate applied the patch. Do not
			// infer ownership from an identical external route before a change:
			// require the effective version to differ from our original snapshot.
			if !errors.As(err, &failure) {
				verifyCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				after, active, verifyErr := g.read(verifyCtx)
				stop()
				if verifyErr == nil && after.Version != raw.Version {
					observedJSON, encodeErr := json.Marshal(active.Config.Lite.Routes)
					if encodeErr == nil && bytes.Equal(candidateJSON, observedJSON) {
						g.needsPersist = true
						return &AppliedError{Message: "Маршруты применены в Gate, но подтверждение сохранения потеряно. Повторите синхронизацию", Uncertain: true}
					}
				}
			}
			return err
		}
		if result.Version == "" {
			verifyCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_, active, verifyErr := g.read(verifyCtx)
			stop()
			if verifyErr == nil {
				observedJSON, encodeErr := json.Marshal(active.Config.Lite.Routes)
				if encodeErr == nil && bytes.Equal(candidateJSON, observedJSON) {
					g.needsPersist = true
					return &AppliedError{Message: "Маршруты применены в Gate, но новая версия сохранённой конфигурации не подтверждена. Повторите синхронизацию", Uncertain: true}
				}
			}
			return errors.New("Gate не подтвердил новую версию конфигурации; не удалось подтвердить активные маршруты")
		}
		for _, warning := range result.Warnings {
			if strings.Contains(strings.ToLower(warning), "failed to persist") {
				g.needsPersist = true
				return &AppliedError{Message: fmt.Sprintf("маршруты применены в памяти Gate, но не сохранены на диск: %s", warning)}
			}
		}
		g.needsPersist = false
		return nil
	}
	return errors.New("конфигурация Gate менялась одновременно; повторите синхронизацию")
}
