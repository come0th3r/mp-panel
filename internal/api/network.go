package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/networking"
	"github.com/come0th3r/mp-panel/internal/store"
)

const gatewayLock = 628121

type gatewayState struct {
	ManagedDomains []string `json:"managed_domains"`
	NeedsPersist   bool     `json:"needs_persist"`
}

func (a *API) getRoute(w http.ResponseWriter, r *http.Request) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	out := map[string]any{"enabled": a.Config.GatewayURL() != "", "provider": a.Config.GatewayProvider(), "base_domain": a.Config.GameDomain}
	if a.Config.GameDomain != "" && len(s.ID) >= 12 {
		domain := s.ID[:12] + "." + a.Config.GameDomain
		if networking.Validate(domain, "localhost:25565") == nil {
			out["default_domain"] = domain
		}
	}
	var route networking.Route
	e := a.Store.Get(r.Context(), "route", s.ID, &route)
	if e == nil {
		out["route"] = route
	} else if !store.IsMissing(e) {
		fail(w, 503, "Не удалось получить маршрут")
		return
	}
	write(w, 200, out)
}

func (a *API) putRoute(w http.ResponseWriter, r *http.Request) {
	if a.Config.GatewayURL() == "" {
		fail(w, 409, "Игровой шлюз не настроен")
		return
	}
	var p struct {
		Domain  string `json:"domain"`
		Backend string `json:"backend"`
	}
	if !decode(w, r, &p) {
		return
	}
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	operator := actor(r).Role == "operator"
	if p.Backend == "" || operator {
		var n model.Node
		if a.Store.Get(r.Context(), "node", s.NodeID, &n) != nil {
			fail(w, 404, "Хост не найден")
			return
		}
		if p.Backend == "" {
			host := n.Address
			if ip := net.ParseIP(n.GameBindAddress); ip != nil && !ip.IsUnspecified() {
				host = ip.String()
			}
			p.Backend = net.JoinHostPort(host, strconv.Itoa(s.Port))
		}
		if operator && !operatorRouteBackend(n, s.Port, strings.TrimSpace(p.Backend)) {
			fail(w, 403, "Маршрут оператора должен вести на адрес хоста и порт назначенного сервера")
			return
		}
	}
	p.Domain = strings.ToLower(strings.TrimSpace(p.Domain))
	p.Backend = strings.TrimSpace(p.Backend)
	if e := networking.Validate(p.Domain, p.Backend); e != nil {
		fail(w, 400, e.Error())
		return
	}
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, s.NodeID).Scan(&raw); e != nil {
		fail(w, 404, "Хост не найден")
		return
	}
	if operator {
		var n model.Node
		if json.Unmarshal(raw, &n) != nil || !operatorRouteBackend(n, s.Port, p.Backend) {
			fail(w, 403, "Адрес хоста изменился. Обновите маршрут назначенного сервера")
			return
		}
	}
	var exists bool
	if e = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM mp_entities WHERE kind='server' AND id=$1)`, s.ID).Scan(&exists); e != nil || !exists {
		fail(w, 404, "Сервер не найден")
		return
	}
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock($1)`, gatewayLock); e != nil {
		fail(w, 503, "Шлюз занят")
		return
	}
	var count int
	var existing bool
	if e = tx.QueryRow(r.Context(), `SELECT count(*), COALESCE(bool_or(id=$1),false) FROM mp_entities WHERE kind='route'`, s.ID).Scan(&count, &existing); e != nil {
		fail(w, 503, "Не удалось проверить число маршрутов")
		return
	}
	if !existing && count >= 2000 {
		fail(w, 409, "Достигнут предел в 2000 маршрутов игрового шлюза")
		return
	}
	route := networking.Route{ServerID: s.ID, Domain: p.Domain, Backend: p.Backend, Status: "pending", UpdatedAt: now()}
	if e = store.PutTx(r.Context(), tx, "route", s.ID, route); e != nil {
		fail(w, 409, "Домен уже назначен другому серверу")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		fail(w, 503, "Не удалось сохранить маршрут")
		return
	}
	a.audit(r.Context(), actor(r), "route.put", s.ID, p.Domain)
	write(w, 202, route)
}

func operatorRouteBackend(n model.Node, serverPort int, backend string) bool {
	host, port, err := net.SplitHostPort(backend)
	if err != nil {
		return false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number != serverPort {
		return false
	}
	match := func(expected string) bool {
		if actualIP, expectedIP := net.ParseIP(host), net.ParseIP(expected); actualIP != nil && expectedIP != nil {
			return actualIP.Equal(expectedIP)
		}
		return strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(expected, "."))
	}
	if match(n.Address) {
		return true
	}
	ip := net.ParseIP(n.GameBindAddress)
	return ip != nil && !ip.IsUnspecified() && match(n.GameBindAddress)
}

func (a *API) removeRoute(w http.ResponseWriter, r *http.Request) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	defer tx.Rollback(r.Context())
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock($1)`, gatewayLock); e == nil {
		_, e = tx.Exec(r.Context(), `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','removing','updated_at',$2::text) WHERE kind='route' AND id=$1`, s.ID, now())
	}
	if e != nil || tx.Commit(r.Context()) != nil {
		fail(w, 503, "Не удалось удалить маршрут")
		return
	}
	a.audit(r.Context(), actor(r), "route.delete", s.ID, "")
	write(w, 200, map[string]bool{"ok": true})
}

func (a *API) RunGateway(ctx context.Context) {
	if a.Config.GatewayURL() == "" {
		return
	}
	g := networking.New(a.Config.GateURL)
	if a.Config.GatewayProvider() == "mc-router" {
		g = networking.NewMCRouter(a.Config.MCRouterURL)
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		runCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		if e := a.reconcileGateway(runCtx, g); e != nil && ctx.Err() == nil {
			log.Printf("gateway reconciliation: %v", e)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *API) reconcileGateway(ctx context.Context, g *networking.Gateway) error {
	tx, e := a.Store.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var locked bool
	if e = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, gatewayLock).Scan(&locked); e != nil || !locked {
		return e
	}
	var state gatewayState
	var raw []byte
	e = tx.QueryRow(ctx, `SELECT payload FROM mp_entities WHERE kind='gateway_state' AND id='state'`).Scan(&raw)
	if e != nil && !store.IsMissing(e) {
		return e
	}
	if e == nil {
		if e = json.Unmarshal(raw, &state); e != nil {
			return e
		}
	}
	rows, e := tx.Query(ctx, `SELECT payload FROM mp_entities WHERE kind='route' ORDER BY id`)
	if e != nil {
		return e
	}
	all := []networking.Route{}
	desired := []networking.Route{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		var route networking.Route
		if e = json.Unmarshal(b, &route); e != nil {
			rows.Close()
			return e
		}
		all = append(all, route)
		if route.Status != "removing" {
			desired = append(desired, route)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(all) == 0 && len(state.ManagedDomains) == 0 {
		return nil
	}
	if state.NeedsPersist {
		g.RequirePersistence()
	}
	var applyErr error
	if a.Config.GatewayProvider() == "mc-router" {
		claimed := append([]string{}, state.ManagedDomains...)
		seen := make(map[string]bool, len(claimed))
		for _, domain := range claimed {
			seen[domain] = true
		}
		for _, route := range desired {
			if !seen[route.Domain] {
				claimed = append(claimed, route.Domain)
				seen[route.Domain] = true
			}
		}
		if len(claimed) > len(state.ManagedDomains) {
			if len(claimed) > 4000 {
				applyErr = errors.New("Достигнут предел в 4000 доменов, закреплённых за шлюзом панели")
			} else {
				// Reserve only domains absent from the private router. Committing intent
				// before REST mutations preserves ownership across a panel crash.
				applyErr = g.CheckClaims(ctx, state.ManagedDomains, desired)
			}
			if applyErr == nil {
				state.ManagedDomains = claimed
				if e = store.PutTx(ctx, tx, "gateway_state", "state", state); e != nil {
					return e
				}
				if e = tx.Commit(ctx); e != nil {
					return e
				}
				return a.reconcileGateway(ctx, g)
			}
		}
	}
	if applyErr == nil {
		applyErr = g.Apply(ctx, state.ManagedDomains, desired)
	}
	var appliedWarning *networking.AppliedError
	if applyErr == nil || errors.As(applyErr, &appliedWarning) {
		domains := []string{}
		seen := map[string]bool{}
		if applyErr != nil || a.Config.GatewayProvider() == "mc-router" {
			for _, domain := range state.ManagedDomains {
				domains = append(domains, domain)
				seen[domain] = true
			}
		}
		for _, route := range desired {
			if !seen[route.Domain] {
				domains = append(domains, route.Domain)
				seen[route.Domain] = true
			}
		}
		if e = store.PutTx(ctx, tx, "gateway_state", "state", gatewayState{ManagedDomains: domains, NeedsPersist: applyErr != nil}); e != nil {
			return e
		}
	}
	for _, route := range all {
		if applyErr == nil && route.Status == "removing" {
			_, e = tx.Exec(ctx, `DELETE FROM mp_entities WHERE kind='route' AND id=$1`, route.ServerID)
		} else {
			if applyErr == nil {
				route.Status = "active"
				route.Error = ""
			} else {
				if route.Status != "removing" {
					route.Status = "error"
				}
				route.Error = applyErr.Error()
			}
			route.UpdatedAt = now()
			e = store.PutTx(ctx, tx, "route", route.ServerID, route)
		}
		if e != nil {
			return e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return applyErr
}
