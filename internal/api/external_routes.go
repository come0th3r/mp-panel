package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/come0th3r/mp-panel/internal/networking"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// External routes only publish a connection address. They deliberately have no
// associated server or node, so adding one cannot imply control over its files.
func (a *API) externalRoutes(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Store.Pool.Query(r.Context(), `SELECT payload FROM mp_entities WHERE kind='route' AND payload->>'external'='true' ORDER BY created_at DESC LIMIT 2000`)
	if err != nil {
		fail(w, 503, "Не удалось получить внешние маршруты")
		return
	}
	defer rows.Close()
	routes := []networking.Route{}
	for rows.Next() {
		var raw []byte
		var route networking.Route
		if rows.Scan(&raw) != nil || json.Unmarshal(raw, &route) != nil {
			fail(w, 503, "Не удалось прочитать внешний маршрут")
			return
		}
		routes = append(routes, route)
	}
	if rows.Err() != nil {
		fail(w, 503, "Не удалось получить внешние маршруты")
		return
	}
	write(w, 200, map[string]any{"enabled": a.Config.GatewayURL() != "", "provider": a.Config.GatewayProvider(), "base_domain": a.Config.GameDomain, "routes": routes})
}

func (a *API) saveExternalRoute(w http.ResponseWriter, r *http.Request) {
	if a.Config.GatewayURL() == "" {
		fail(w, 409, "Игровой шлюз не настроен")
		return
	}
	var p struct {
		Name    string `json:"name"`
		Domain  string `json:"domain"`
		Backend string `json:"backend"`
	}
	if !decodeLimit(w, r, &p, 4096) {
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Domain = strings.ToLower(strings.TrimSpace(p.Domain))
	p.Backend = strings.TrimSpace(p.Backend)
	if !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) < 1 || utf8.RuneCountInString(p.Name) > 120 || strings.IndexFunc(p.Name, unicode.IsControl) >= 0 {
		fail(w, 400, "Название должно содержать от 1 до 120 символов без управляющих символов")
		return
	}
	if err := networking.Validate(p.Domain, p.Backend); err != nil {
		fail(w, 400, err.Error())
		return
	}
	tx, err := a.Store.Pool.Begin(r.Context())
	if err != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock($1)`, gatewayLock); err != nil {
		fail(w, 503, "Шлюз занят")
		return
	}
	id := r.PathValue("id")
	action := "external-route.update"
	if id == "" {
		id = "external-" + security.ID()
		action = "external-route.create"
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM mp_entities WHERE kind='route'`).Scan(&count); err != nil {
			fail(w, 503, "Не удалось проверить число маршрутов")
			return
		}
		if count >= 2000 {
			fail(w, 409, "Достигнут предел в 2000 маршрутов игрового шлюза")
			return
		}
	} else {
		var raw []byte
		var existing networking.Route
		err = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='route' AND id=$1 FOR UPDATE`, id).Scan(&raw)
		if store.IsMissing(err) || (err == nil && (json.Unmarshal(raw, &existing) != nil || !existing.External)) {
			fail(w, 404, "Внешний маршрут не найден")
			return
		}
		if err != nil {
			fail(w, 503, "Не удалось прочитать внешний маршрут")
			return
		}
		if existing.Status == "removing" {
			fail(w, 409, "Маршрут удаляется. Дождитесь завершения синхронизации")
			return
		}
	}
	route := networking.Route{ServerID: id, Name: p.Name, External: true, Domain: p.Domain, Backend: p.Backend, Status: "pending", UpdatedAt: now()}
	if err = store.PutTx(r.Context(), tx, "route", id, route); err != nil {
		if databaseError, ok := err.(*pgconn.PgError); ok && databaseError.Code == "23505" {
			fail(w, 409, "Домен уже назначен другому серверу или маршруту")
		} else {
			fail(w, 503, "Не удалось сохранить внешний маршрут")
		}
		return
	}
	u := actor(r)
	_, err = tx.Exec(r.Context(), `INSERT INTO mp_audit(id,actor_id,actor,action,object_id,detail) VALUES($1,$2,$3,$4,$5,$6)`, security.ID(), u.ID, u.Username, action, id, string(store.Marshal(p)))
	if err != nil || tx.Commit(r.Context()) != nil {
		fail(w, 503, "Не удалось сохранить внешний маршрут")
		return
	}
	write(w, 202, route)
}

func (a *API) removeExternalRoute(w http.ResponseWriter, r *http.Request) {
	tx, err := a.Store.Pool.Begin(r.Context())
	if err != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock($1)`, gatewayLock); err != nil {
		fail(w, 503, "Шлюз занят")
		return
	}
	id := r.PathValue("id")
	var raw []byte
	var route networking.Route
	err = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='route' AND id=$1 FOR UPDATE`, id).Scan(&raw)
	if store.IsMissing(err) || (err == nil && (json.Unmarshal(raw, &route) != nil || !route.External)) {
		fail(w, 404, "Внешний маршрут не найден")
		return
	}
	if err != nil {
		fail(w, 503, "Не удалось прочитать внешний маршрут")
		return
	}
	route.Status = "removing"
	route.UpdatedAt = now()
	if err = store.PutTx(r.Context(), tx, "route", id, route); err != nil {
		fail(w, 503, "Не удалось удалить внешний маршрут")
		return
	}
	u := actor(r)
	_, err = tx.Exec(r.Context(), `INSERT INTO mp_audit(id,actor_id,actor,action,object_id,detail) VALUES($1,$2,$3,'external-route.delete',$4,$5)`, security.ID(), u.ID, u.Username, id, route.Domain)
	if err != nil || tx.Commit(r.Context()) != nil {
		fail(w, 503, "Не удалось удалить внешний маршрут")
		return
	}
	write(w, 202, map[string]bool{"ok": true})
}
