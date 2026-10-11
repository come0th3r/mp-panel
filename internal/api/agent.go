package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/store"
	"github.com/jackc/pgx/v5"
)

func (a *API) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/v1/heartbeat", a.heartbeat)
	mux.HandleFunc("GET /agent/v1/commands", a.nextCommand)
	mux.HandleFunc("POST /agent/v1/results", a.commandResult)
	mux.HandleFunc("GET /agent/v1/artifacts/{id}", a.agentArtifact)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			fail(w, 403, "Client certificate required")
			return
		}
		id := r.TLS.PeerCertificates[0].Subject.CommonName
		var n model.Node
		if a.Store.Get(r.Context(), "node", id, &n) != nil {
			fail(w, 403, "Node registration revoked")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), contextKey("node"), id))
		mux.ServeHTTP(w, r)
	})
}
func nodeIdentity(r *http.Request) string {
	id, _ := r.Context().Value(contextKey("node")).(string)
	return id
}
func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	var h model.Heartbeat
	if !decodeAgent(w, r, &h) {
		return
	}
	id := nodeIdentity(r)
	if h.Node.ID != id || len(h.Servers) > 2000 || len(h.Metrics) > 2001 {
		fail(w, 400, "Node heartbeat identity or size mismatch")
		return
	}
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 503, "Database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, id).Scan(&raw); e != nil {
		fail(w, 403, "Node registration revoked")
		return
	}
	var n model.Node
	if json.Unmarshal(raw, &n) != nil {
		fail(w, 500, "Invalid node record")
		return
	}
	n.Status = "online"
	n.Error = ""
	if h.Node.Status == "error" {
		n.Status = "error"
		n.Error = h.Node.Error
	}
	n.LastSeen = now()
	n.OS = h.Node.OS
	n.Arch = h.Node.Arch
	n.CPUCount = h.Node.CPUCount
	n.MemoryTotal = h.Node.MemoryTotal
	n.DiskTotal = h.Node.DiskTotal
	n.DiskFree = h.Node.DiskFree
	if e = store.PutTx(r.Context(), tx, "node", id, n); e != nil {
		fail(w, 500, "Unable to persist heartbeat")
		return
	}
	for _, observed := range h.Servers {
		if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='server' AND id=$1 AND payload->>'node_id'=$2`, observed.ID, id).Scan(&raw); e != nil {
			continue
		}
		var s model.Server
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		var active bool
		if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM mp_commands WHERE server_id=$1 AND mutating AND state IN ('pending','delivered'))`, s.ID).Scan(&active) != nil {
			continue
		}
		if active {
			continue
		}
		switch observed.Status {
		case "running", "starting", "stopped", "error", "unknown":
			s.Status = observed.Status
			s.Error = observed.Error
		default:
			continue
		}
		if e = store.PutTx(r.Context(), tx, "server", s.ID, s); e != nil {
			fail(w, 500, "Unable to persist observed server state")
			return
		}
	}
	for _, m := range h.Metrics {
		if m.NodeID != id {
			continue
		}
		if m.ServerID != "" {
			var exists bool
			if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM mp_entities WHERE kind='server' AND id=$1 AND payload->>'node_id'=$2)`, m.ServerID, id).Scan(&exists) != nil || !exists {
				continue
			}
		}
		m.Timestamp = now()
		if _, e = tx.Exec(r.Context(), `INSERT INTO mp_metrics(node_id,server_id,payload) VALUES($1,$2,$3)`, id, m.ServerID, store.Marshal(m)); e != nil {
			fail(w, 500, "Unable to persist metrics")
			return
		}
	}
	if e = tx.Commit(r.Context()); e != nil {
		fail(w, 500, "Unable to commit heartbeat")
		return
	}
	write(w, 200, map[string]bool{"ok": true})
}
func (a *API) nextCommand(w http.ResponseWriter, r *http.Request) {
	id := nodeIdentity(r)
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var raw []byte
		e := a.Store.Pool.QueryRow(r.Context(), `UPDATE mp_commands SET state='delivered',delivered_at=now() WHERE id=(SELECT id FROM mp_commands WHERE node_id=$1 AND deadline>now() AND (state='pending' OR (state='delivered' AND delivered_at<now()-interval '30 seconds')) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING payload`, id).Scan(&raw)
		if e == nil {
			var c model.Command
			if json.Unmarshal(raw, &c) != nil {
				fail(w, 500, "Invalid command record")
				return
			}
			if c.JobID != "" {
				_, _ = a.Store.Pool.Exec(r.Context(), `UPDATE mp_entities SET payload=jsonb_set(jsonb_set(jsonb_set(payload,'{status}','"running"'),'{step}','"Агент выполняет операцию"'),'{updated_at}',to_jsonb($2::text)) WHERE kind='job' AND id=$1 AND payload->>'status'='queued'`, c.JobID, now())
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(raw)
			return
		}
		if !store.IsMissing(e) {
			fail(w, 503, "Command queue unavailable")
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			w.WriteHeader(204)
			return
		case <-ticker.C:
		}
	}
}
func (a *API) commandResult(w http.ResponseWriter, r *http.Request) {
	var result model.CommandResult
	if !decodeAgent(w, r, &result) {
		return
	}
	id := nodeIdentity(r)
	if result.NodeID != id || result.CommandID == "" {
		fail(w, 403, "Result identity mismatch")
		return
	}
	if len(result.Logs) > 1000 {
		result.Logs = result.Logs[len(result.Logs)-1000:]
	}
	for i, line := range result.Logs {
		if len(line) > 4096 {
			result.Logs[i] = line[:4096]
		}
	}
	if len(result.Error) > 8192 {
		result.Error = result.Error[:8192]
	}
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 503, "Database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var nodeRaw []byte
	if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, id).Scan(&nodeRaw); e != nil {
		fail(w, 403, "Node registration revoked")
		return
	}
	var raw, previous, late []byte
	var state string
	var commandCreated time.Time
	if e = tx.QueryRow(r.Context(), `SELECT payload,result,late_result,state,created_at FROM mp_commands WHERE id=$1 AND node_id=$2 FOR UPDATE`, result.CommandID, id).Scan(&raw, &previous, &late, &state, &commandCreated); e != nil {
		fail(w, 404, "Command not found")
		return
	}
	var cmd model.Command
	if json.Unmarshal(raw, &cmd) != nil {
		fail(w, 500, "Invalid command")
		return
	}
	if state == "expired" {
		if len(late) > 0 {
			write(w, 200, map[string]bool{"ok": true})
			return
		}
		if e = registerResultBackup(r.Context(), tx, cmd, result); e == nil {
			e = a.recordLateResult(r.Context(), tx, cmd, result, commandCreated)
		}
		if e == nil {
			e = tx.Commit(r.Context())
		}
		if e != nil {
			fail(w, 500, "Unable to reconcile late command result")
			return
		}
		write(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(previous) > 0 {
		write(w, 200, map[string]bool{"ok": true})
		return
	}
	if state != "delivered" && state != "pending" {
		write(w, 200, map[string]bool{"ok": true})
		return
	}
	if _, e = tx.Exec(r.Context(), `UPDATE mp_commands SET state='completed',result=$2 WHERE id=$1`, cmd.ID, store.Marshal(result)); e != nil {
		fail(w, 500, "Unable to save result")
		return
	}
	if cmd.JobID != "" {
		var j model.Job
		if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='job' AND id=$1`, cmd.JobID).Scan(&raw); e == nil && json.Unmarshal(raw, &j) == nil {
			j.UpdatedAt = now()
			j.Logs = append(j.Logs, result.Logs...)
			if result.Success {
				j.Status = "succeeded"
				j.Step = "Операция завершена"
				j.Error = ""
			} else {
				j.Status = "failed"
				j.Step = "Операция завершилась ошибкой"
				j.Error = result.Error
			}
			if e = store.PutTx(r.Context(), tx, "job", j.ID, j); e != nil {
				fail(w, 500, "Unable to save job")
				return
			}
		}
		var original model.Server
		if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='server' AND id=$1 AND payload->>'node_id'=$2`, cmd.ServerID, id).Scan(&raw); e == nil && json.Unmarshal(raw, &original) == nil {
			committedProfile := false
			// Installation may commit its profile and then fail during startup.
			// Preserve that installed version so subsequent actions describe the
			// runtime actually present on the node, including failed jobs.
			if cmd.Type == "create" || cmd.Type == "install" {
				var out struct {
					Server *model.Server `json:"server"`
				}
				if json.Unmarshal(result.Output, &out) == nil && out.Server != nil && out.Server.ID == original.ID && out.Server.NodeID == id {
					original = *out.Server
					committedProfile = true
				}
			}
			if result.Success {
				switch cmd.Type {
				case "delete":
					_, e = tx.Exec(r.Context(), `DELETE FROM mp_entities WHERE kind='server' AND id=$1`, original.ID)
					if e == nil {
						_, e = tx.Exec(r.Context(), `DELETE FROM mp_server_operators WHERE server_id=$1`, original.ID)
					}
					if e == nil {
						_, e = tx.Exec(r.Context(), `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','removing','updated_at',$2::text) WHERE kind='route' AND id=$1`, original.ID, now())
					}
				case "create", "install":
					if cmd.Server != nil && !committedProfile {
						original = *cmd.Server
					}
					original.Status = "running"
					original.DesiredState = "running"
					original.Error = ""
					e = store.PutTx(r.Context(), tx, "server", original.ID, original)
				case "start", "restart":
					original.Status = "running"
					original.DesiredState = "running"
					original.Error = ""
					e = store.PutTx(r.Context(), tx, "server", original.ID, original)
				case "stop":
					original.Status = "stopped"
					original.DesiredState = "stopped"
					original.Error = ""
					e = store.PutTx(r.Context(), tx, "server", original.ID, original)
				case "restore":
					var out struct {
						Server model.Server `json:"server"`
					}
					if json.Unmarshal(result.Output, &out) == nil && out.Server.ID == original.ID && out.Server.NodeID == id {
						original = out.Server
					}
					original.Status = "stopped"
					original.DesiredState = "stopped"
					original.Error = ""
					e = store.PutTx(r.Context(), tx, "server", original.ID, original)
				}
			} else if cmd.Type == "create" || cmd.Type == "install" || cmd.Type == "start" || cmd.Type == "restart" || cmd.Type == "stop" || cmd.Type == "restore" || cmd.Type == "delete" {
				original.Status = "error"
				original.Error = result.Error
				e = store.PutTx(r.Context(), tx, "server", original.ID, original)
			}
			if e != nil {
				fail(w, 500, "Unable to save server state")
				return
			}
		}
	}
	if e = registerResultBackup(r.Context(), tx, cmd, result); e != nil {
		fail(w, 500, "Unable to register backup")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		fail(w, 500, "Unable to commit result")
		return
	}
	write(w, 200, map[string]bool{"ok": true})
}

func registerResultBackup(ctx context.Context, tx pgx.Tx, cmd model.Command, result model.CommandResult) error {
	if len(result.Output) == 0 || (cmd.Type != "install" && cmd.Type != "backup") {
		return nil
	}
	var backup model.Backup
	if cmd.Type == "backup" {
		if json.Unmarshal(result.Output, &backup) != nil {
			return nil
		}
	} else {
		var out struct {
			Backup model.Backup `json:"backup"`
		}
		if json.Unmarshal(result.Output, &out) != nil {
			return nil
		}
		backup = out.Backup
	}
	if backup.ID == "" {
		return nil
	}
	var request struct {
		BackupID string `json:"backup_id"`
	}
	if json.Unmarshal(cmd.Payload, &request) != nil {
		return fmt.Errorf("invalid backup assignment")
	}
	digest, err := hex.DecodeString(backup.SHA256)
	if request.BackupID == "" || backup.ID != request.BackupID || backup.ServerID != cmd.ServerID || backup.NodeID != cmd.NodeID || backup.Size <= 0 || backup.Path == "" || len(backup.Path) > 8192 || err != nil || len(digest) != 32 {
		return fmt.Errorf("invalid backup result identity or metadata")
	}
	if _, err = time.Parse(time.RFC3339, backup.CreatedAt); err != nil {
		return fmt.Errorf("invalid backup result timestamp")
	}
	// Backup identity is assigned by the panel. A replay or delayed command may
	// register it, but must never replace an already registered snapshot.
	if _, err = tx.Exec(ctx, `INSERT INTO mp_entities(kind,id,payload) VALUES('backup',$1,$2) ON CONFLICT(kind,id) DO NOTHING`, backup.ID, store.Marshal(backup)); err != nil {
		return err
	}
	var existingRaw []byte
	if err = tx.QueryRow(ctx, `SELECT payload FROM mp_entities WHERE kind='backup' AND id=$1`, backup.ID).Scan(&existingRaw); err != nil {
		return err
	}
	var existing model.Backup
	if err = json.Unmarshal(existingRaw, &existing); err != nil {
		return err
	}
	if existing != backup {
		return fmt.Errorf("backup metadata is immutable")
	}
	return nil
}

func (a *API) recordLateResult(ctx context.Context, tx pgx.Tx, cmd model.Command, result model.CommandResult, created time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE mp_commands SET late_result=$2 WHERE id=$1 AND state='expired' AND late_result IS NULL`, cmd.ID, store.Marshal(result)); err != nil {
		return err
	}
	if cmd.JobID != "" {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT payload FROM mp_entities WHERE kind='job' AND id=$1`, cmd.JobID).Scan(&raw)
		if err != nil && !store.IsMissing(err) {
			return err
		}
		if err == nil {
			var job model.Job
			if err = json.Unmarshal(raw, &job); err != nil {
				return err
			}
			job.Status = "failed"
			if job.Error == "" {
				job.Error = "Операция превысила срок ожидания"
			}
			job.UpdatedAt = now()
			job.Logs = append(job.Logs, result.Logs...)
			message := "Поздний результат: агент завершил операцию после срока ожидания; проверьте текущее состояние сервера"
			if !result.Success {
				message = "Поздний результат: агент сообщил ошибку после срока ожидания: " + result.Error
			}
			job.Logs = append(job.Logs, message)
			if len(job.Logs) > 2000 {
				job.Logs = job.Logs[len(job.Logs)-2000:]
			}
			if err = store.PutTx(ctx, tx, "job", job.ID, job); err != nil {
				return err
			}
		}
	}
	if cmd.ServerID == "" {
		return nil
	}
	var newer bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mp_commands WHERE server_id=$1 AND mutating AND created_at>$2 AND id<>$3)`, cmd.ServerID, created, cmd.ID).Scan(&newer); err != nil {
		return err
	}
	// Enqueue and results hold the same node lock. Once a newer mutation exists,
	// a delayed older result cannot change its version, status, or intent.
	if newer {
		return nil
	}
	if cmd.Type == "delete" && result.Success {
		if _, err := tx.Exec(ctx, `DELETE FROM mp_entities WHERE kind='server' AND id=$1 AND payload->>'node_id'=$2`, cmd.ServerID, cmd.NodeID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mp_server_operators WHERE server_id=$1`, cmd.ServerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','removing','updated_at',$2::text) WHERE kind='route' AND id=$1`, cmd.ServerID, now())
		return err
	}
	if cmd.Type != "create" && cmd.Type != "install" && cmd.Type != "restore" {
		return nil
	}
	var out struct {
		Server *model.Server `json:"server"`
	}
	if json.Unmarshal(result.Output, &out) != nil || out.Server == nil || out.Server.ID != cmd.ServerID || out.Server.NodeID != cmd.NodeID {
		return nil
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM mp_entities WHERE kind='server' AND id=$1 AND payload->>'node_id'=$2`, cmd.ServerID, cmd.NodeID).Scan(&raw)
	if store.IsMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var current model.Server
	if err = json.Unmarshal(raw, &current); err != nil {
		return err
	}
	profile := *out.Server
	profile.DesiredState = current.DesiredState
	profile.Status = "unknown"
	profile.Error = current.Error
	return store.PutTx(ctx, tx, "server", profile.ID, profile)
}
func (a *API) agentArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 32 || strings.ContainsAny(id, "/\\") {
		fail(w, 400, "Invalid artifact ID")
		return
	}
	var authorized bool
	e := a.Store.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM mp_commands c WHERE node_id=$1 AND state IN ('pending','delivered') AND (
 c.payload->'payload'->'artifact'->>'id'=$2 OR EXISTS(
 SELECT 1 FROM jsonb_array_elements(COALESCE(c.payload->'install'->'artifacts','[]'::jsonb)||COALESCE(c.payload->'install'->'extra_jars','[]'::jsonb)) asset WHERE asset->>'id'=$2)))`, nodeIdentity(r), id).Scan(&authorized)
	if e != nil || !authorized {
		fail(w, 403, "Artifact not assigned to this node")
		return
	}
	var u storedUpload
	if a.Store.Get(r.Context(), "upload", id, &u) != nil {
		fail(w, 404, "Artifact not found")
		return
	}
	f, e := os.Open(u.Path)
	if e != nil {
		fail(w, 404, "Artifact file not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(u.Size))
	w.Header().Set("X-Artifact-SHA256", u.SHA256)
	io.Copy(w, f)
}
func (a *API) RunMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if e := a.expireCommands(ctx); e != nil {
				log.Printf("expire commands: %v", e)
			}
			if e := a.expireWork(ctx); e != nil {
				log.Printf("expire bootstrap work: %v", e)
			}
			for _, sql := range []string{`DELETE FROM mp_sessions WHERE expires_at<now()`, `DELETE FROM mp_metrics WHERE measured_at<now()-interval '7 days'`, `DELETE FROM mp_idempotency WHERE created_at<now()-interval '24 hours'`, `DELETE FROM mp_commands WHERE state IN ('completed','expired') AND created_at<now()-interval '7 days'`} {
				if _, e := a.Store.Pool.Exec(ctx, sql); e != nil {
					log.Printf("maintenance: %v", e)
				}
			}
		}
	}
}
func (a *API) expireCommands(ctx context.Context) error {
	rows, e := a.Store.Pool.Query(ctx, `SELECT id,node_id,payload FROM mp_commands WHERE state IN ('pending','delivered') AND deadline<now() LIMIT 500`)
	if e != nil {
		return e
	}
	type expired struct {
		id, node string
		cmd      model.Command
	}
	items := []expired{}
	for rows.Next() {
		var x expired
		var raw []byte
		if e = rows.Scan(&x.id, &x.node, &raw); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(raw, &x.cmd); e != nil {
			rows.Close()
			return e
		}
		items = append(items, x)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, x := range items {
		tx, e := a.Store.Pool.Begin(ctx)
		if e != nil {
			return e
		}
		// Keep the lock order shared by enqueue, heartbeat, and results:
		// node -> command -> public entities. The command's partial unique
		// index must never be locked before a node/server enqueue transaction.
		var lockedNode string
		if e = tx.QueryRow(ctx, `SELECT id FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, x.node).Scan(&lockedNode); e != nil && !store.IsMissing(e) {
			tx.Rollback(ctx)
			return e
		}
		tag, e := tx.Exec(ctx, `UPDATE mp_commands SET state='expired',result=$2 WHERE id=$1 AND state IN ('pending','delivered')`, x.id, store.Marshal(model.CommandResult{CommandID: x.id, NodeID: x.node, Error: "Время операции истекло; фактическое состояние будет уточнено агентом"}))
		if e == nil && tag.RowsAffected() > 0 && x.cmd.JobID != "" {
			var raw []byte
			var j model.Job
			if tx.QueryRow(ctx, `SELECT payload FROM mp_entities WHERE kind='job' AND id=$1`, x.cmd.JobID).Scan(&raw) == nil && json.Unmarshal(raw, &j) == nil {
				j.Status = "failed"
				j.Step = "Превышено время ожидания"
				j.Error = "Проверьте состояние хоста и сервера перед повтором"
				j.UpdatedAt = now()
				e = store.PutTx(ctx, tx, "job", j.ID, j)
			}
		}
		lifecycle := false
		switch x.cmd.Type {
		case "create", "install", "start", "restart", "stop", "restore", "delete":
			lifecycle = true
		}
		if e == nil && tag.RowsAffected() > 0 && x.cmd.ServerID != "" && lifecycle {
			_, e = tx.Exec(ctx, `UPDATE mp_entities SET payload=jsonb_set(payload,'{status}','"unknown"') WHERE kind='server' AND id=$1`, x.cmd.ServerID)
		}
		if e == nil {
			e = tx.Commit(ctx)
		} else {
			tx.Rollback(ctx)
		}
		if e != nil {
			return e
		}
	}
	return nil
}

func (a *API) expireWork(ctx context.Context) error {
	rows, err := a.Store.Pool.Query(ctx, `SELECT id,node_id FROM mp_work WHERE kind='bootstrap' AND state IN ('queued','running') AND expires_at<=now() ORDER BY created_at LIMIT 500`)
	if err != nil {
		return err
	}
	type expired struct{ id, node string }
	items := []expired{}
	for rows.Next() {
		var item expired
		if err = rows.Scan(&item.id, &item.node); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range items {
		tx, e := a.Store.Pool.Begin(ctx)
		if e != nil {
			return e
		}
		var node string
		if e = tx.QueryRow(ctx, `SELECT id FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, item.node).Scan(&node); e != nil && !store.IsMissing(e) {
			tx.Rollback(ctx)
			return e
		}
		tag, e := tx.Exec(ctx, `UPDATE mp_work SET state='failed',secret=NULL WHERE id=$1 AND kind='bootstrap' AND state IN ('queued','running') AND expires_at<=now()`, item.id)
		if e == nil && tag.RowsAffected() > 0 {
			_, e = tx.Exec(ctx, `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','failed','step','Превышено время подключения','error','Срок задания подключения истёк. Повторите подключение хоста','updated_at',$2::text) WHERE kind='job' AND id=$1 AND payload->>'status' IN ('queued','running')`, item.id, now())
			if e == nil {
				_, e = tx.Exec(ctx, `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','error','error','Срок задания подключения истёк. Повторите подключение хоста') WHERE kind='node' AND id=$1 AND payload->>'status'<>'online' AND NOT EXISTS(SELECT 1 FROM mp_work WHERE node_id=$1 AND id<>$2 AND state IN ('queued','running') AND expires_at>now())`, item.node, item.id)
			}
		}
		if e == nil {
			e = tx.Commit(ctx)
		} else {
			tx.Rollback(ctx)
		}
		if e != nil {
			return e
		}
	}
	return nil
}
