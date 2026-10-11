package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
)

// Assignments grant operators full control of individual servers. They never
// grant global host, user, or server-creation permissions.
func (a *API) serverAccess(ctx context.Context, u model.User, id string) (bool, error) {
	if u.Role != "operator" {
		return u.ID != "", nil
	}
	var ok bool
	err := a.Store.Pool.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM mp_server_operators o
 JOIN mp_entities s ON s.kind='server' AND s.id=o.server_id
 JOIN mp_users u ON u.id=o.user_id AND u.role='operator'
 WHERE o.user_id=$1 AND o.server_id=$2)`, u.ID, id).Scan(&ok)
	return ok, err
}

func (a *API) checkServerAccess(w http.ResponseWriter, r *http.Request, u model.User, id string) bool {
	ok, err := a.serverAccess(r.Context(), u, id)
	if err != nil {
		fail(w, 503, "Не удалось проверить доступ к серверу")
		return false
	}
	if !ok {
		fail(w, 404, "Сервер не найден")
	}
	return ok
}

func (a *API) operatorNodeAccess(ctx context.Context, userID, nodeID string) (bool, error) {
	var ok bool
	err := a.Store.Pool.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM mp_server_operators o
 JOIN mp_entities s ON s.kind='server' AND s.id=o.server_id
 JOIN mp_entities n ON n.kind='node' AND n.id=s.payload->>'node_id'
 WHERE o.user_id=$1 AND n.id=$2)`, userID, nodeID).Scan(&ok)
	return ok, err
}

// Filter in SQL before applying the list limit, so unrelated entities never
// consume the operator's result window. All joins require a live server.
func (a *API) accessibleList(r *http.Request, kind string, dest any) error {
	if actor(r).Role != "operator" {
		return a.Store.List(r.Context(), kind, dest)
	}
	var scope string
	switch kind {
	case "server":
		scope = `s.id=e.id`
	case "job":
		scope = `s.id=e.payload->>'server_id'`
	case "node":
		scope = `s.payload->>'node_id'=e.id`
	default:
		return json.Unmarshal([]byte(`[]`), dest)
	}
	rows, err := a.Store.Pool.Query(r.Context(), `SELECT e.payload FROM mp_entities e
 WHERE e.kind=$1 AND EXISTS(SELECT 1 FROM mp_server_operators o
 JOIN mp_entities s ON s.kind='server' AND s.id=o.server_id
 WHERE o.user_id=$2 AND `+scope+`) ORDER BY e.created_at DESC LIMIT 2000`, kind, actor(r).ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		out = append(out, raw)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return json.Unmarshal(store.Marshal(out), dest)
}

func operatorVisibleNode(n model.Node) model.Node {
	// Only the identity and address needed for assigned server navigation and
	// game connections are exposed; host-wide resources and SSH details are not.
	return model.Node{ID: n.ID, Name: n.Name, Address: n.Address, Status: n.Status, LastSeen: n.LastSeen, CreatedAt: n.CreatedAt}
}

type serverOperatorAssignments struct {
	UserIDs []string `json:"user_ids"`
}

func (a *API) serverOperators(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.getServer(w, r); !ok {
		return
	}
	rows, err := a.Store.Pool.Query(r.Context(), `SELECT o.user_id FROM mp_server_operators o
 JOIN mp_users u ON u.id=o.user_id AND u.role='operator'
 WHERE o.server_id=$1 ORDER BY o.user_id`, r.PathValue("id"))
	if err != nil {
		fail(w, 503, "Не удалось получить операторов сервера")
		return
	}
	defer rows.Close()
	out := serverOperatorAssignments{UserIDs: []string{}}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			fail(w, 503, "Не удалось получить операторов сервера")
			return
		}
		out.UserIDs = append(out.UserIDs, id)
	}
	if rows.Err() != nil {
		fail(w, 503, "Не удалось получить операторов сервера")
		return
	}
	write(w, 200, out)
}

func (a *API) setServerOperators(w http.ResponseWriter, r *http.Request) {
	var p serverOperatorAssignments
	if !decode(w, r, &p) {
		return
	}
	if p.UserIDs == nil || len(p.UserIDs) > 128 {
		fail(w, 400, "Укажите список user_ids: не более 128 операторов")
		return
	}
	seen := map[string]bool{}
	for _, id := range p.UserIDs {
		if id == "" || len(id) > 128 || seen[id] {
			fail(w, 400, "Список операторов содержит неверный или повторяющийся ID")
			return
		}
		seen[id] = true
	}
	sort.Strings(p.UserIDs)
	tx, err := a.Store.Pool.Begin(r.Context())
	if err != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	defer tx.Rollback(r.Context())
	var serverID string
	if err = tx.QueryRow(r.Context(), `SELECT id FROM mp_entities WHERE kind='server' AND id=$1 FOR UPDATE`, r.PathValue("id")).Scan(&serverID); err != nil {
		if store.IsMissing(err) {
			fail(w, 404, "Сервер не найден")
		} else {
			fail(w, 503, "Не удалось проверить сервер")
		}
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM mp_users WHERE id=ANY($1::text[]) AND role='operator'`, p.UserIDs).Scan(&count); err != nil {
		fail(w, 503, "Не удалось проверить операторов")
		return
	}
	if count != len(p.UserIDs) {
		fail(w, 400, "Назначать можно только существующих пользователей с ролью operator")
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM mp_server_operators WHERE server_id=$1`, serverID); err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO mp_server_operators(server_id,user_id) SELECT $1,unnest($2::text[])`, serverID, p.UserIDs)
	}
	if err == nil {
		u := actor(r)
		_, err = tx.Exec(r.Context(), `INSERT INTO mp_audit(id,actor_id,actor,action,object_id,detail) VALUES($1,$2,$3,'server.operators',$4,$5)`, security.ID(), u.ID, u.Username, serverID, string(store.Marshal(p)))
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		fail(w, 503, "Не удалось сохранить операторов сервера")
		return
	}
	write(w, 200, p)
}
