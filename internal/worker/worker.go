package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/come0th3r/mp-panel/internal/bootstrap"
	"github.com/come0th3r/mp-panel/internal/config"
	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
)

type credentials struct {
	Name            string `json:"name"`
	Address         string `json:"address"`
	SSHPort         int    `json:"ssh_port"`
	SSHUser         string `json:"ssh_user"`
	GameBindAddress string `json:"game_bind_address"`
	Password        string `json:"password"`
	PrivateKey      string `json:"private_key"`
	HostFingerprint string `json:"host_fingerprint"`
	SudoPassword    string `json:"sudo_password"`
}

func Run(ctx context.Context, s *store.Store, c config.Config) error {
	vault, err := security.OpenVault(c.DataDir)
	if err != nil {
		return err
	}
	ca, err := security.OpenCA(c.DataDir)
	if err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := expire(ctx, s); err != nil {
			log.Printf("worker expiry cleanup: %v", err)
		}
		var id, nodeID string
		var secret []byte
		var expires time.Time
		err = s.Pool.QueryRow(ctx, `UPDATE mp_work SET state='running',claimed_at=now() WHERE id=(SELECT id FROM mp_work WHERE kind='bootstrap' AND state='queued' AND expires_at>now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING id,node_id,secret,expires_at`).Scan(&id, &nodeID, &secret, &expires)
		if store.IsMissing(err) {
			if !wait(ctx, 2*time.Second) {
				return nil
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("claim bootstrap: %w", err)
		}
		process(ctx, s, c, vault, ca, id, nodeID, secret, expires)
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func expire(ctx context.Context, s *store.Store) error {
	rows, err := s.Pool.Query(ctx, `SELECT id,node_id FROM mp_work WHERE kind='bootstrap' AND state IN ('queued','running') AND expires_at<=now() ORDER BY created_at LIMIT 500`)
	if err != nil {
		return err
	}
	type item struct{ id, node string }
	items := []item{}
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.node); err != nil {
			rows.Close()
			return err
		}
		items = append(items, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range items {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		var lockedNode string
		if err = tx.QueryRow(ctx, `SELECT id FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, x.node).Scan(&lockedNode); err != nil && !store.IsMissing(err) {
			tx.Rollback(ctx)
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE mp_work SET state='failed',secret=NULL WHERE id=$1 AND kind='bootstrap' AND state IN ('queued','running') AND expires_at<=now()`, x.id)
		if err == nil && tag.RowsAffected() > 0 {
			_, err = tx.Exec(ctx, `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','failed','step','Превышено время подключения','error','Срок задания подключения истёк. Повторите подключение хоста','updated_at',$2::text) WHERE kind='job' AND id=$1 AND payload->>'status' IN ('queued','running')`, x.id, time.Now().UTC().Format(time.RFC3339))
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','error','error','Срок задания подключения истёк. Повторите подключение хоста') WHERE kind='node' AND id=$1 AND payload->>'status'<>'online' AND NOT EXISTS(SELECT 1 FROM mp_work WHERE node_id=$1 AND id<>$2 AND state IN ('queued','running') AND expires_at>now())`, x.node, x.id)
			}
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			tx.Rollback(ctx)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func process(parent context.Context, s *store.Store, c config.Config, vault *security.Vault, ca *security.CA, id, nodeID string, encrypted []byte, expires time.Time) {
	ctx, cancel := context.WithDeadline(parent, expires)
	defer cancel()
	job := model.Job{ID: id, NodeID: nodeID, Type: "bootstrap", Status: "running", Logs: []string{}, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := s.Get(ctx, "job", id, &job); err != nil {
		log.Printf("bootstrap %s job load: %v", id, err)
	}
	job.Status = "running"
	job.Step = "Подготовка подключения"
	var mu sync.Mutex
	appendLog := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if len(job.Logs) >= 200 {
			job.Logs = job.Logs[len(job.Logs)-199:]
		}
		job.Logs = append(job.Logs, line)
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		payload, err := json.Marshal(job)
		if err == nil {
			_, err = s.Pool.Exec(ctx, `UPDATE mp_entities SET payload=$2 WHERE kind='job' AND id=$1 AND payload->>'status' IN ('queued','running') AND EXISTS(SELECT 1 FROM mp_work WHERE id=$1 AND state='running' AND expires_at>now())`, id, payload)
		}
		if err != nil {
			log.Printf("bootstrap %s log persistence: %v", id, err)
		}
	}
	var result error
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fmt.Errorf("внутренняя ошибка задания подключения")
			log.Printf("bootstrap %s recovered an internal panic", id)
		}
		cleanCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		status := "succeeded"
		if result != nil {
			status = "failed"
			job.Error = "Не удалось подключить хост: " + result.Error()
			job.Step = "Подключение не завершено"
		} else {
			job.Step = "Хост подключён"
		}
		job.Status = status
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := finish(cleanCtx, s, nodeID, job); err != nil {
			log.Printf("bootstrap %s completion: %v", id, err)
		}
		for i := range encrypted {
			encrypted[i] = 0
		}
	}()
	plain, err := vault.Decrypt(encrypted)
	if err != nil {
		result = fmt.Errorf("не удалось расшифровать временные данные доступа")
		return
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	var creds credentials
	if err = json.Unmarshal(plain, &creds); err != nil {
		result = fmt.Errorf("неверный формат временных данных доступа")
		return
	}
	var node model.Node
	if err = s.Get(ctx, "node", nodeID, &node); err != nil {
		result = fmt.Errorf("запись хоста отсутствует")
		return
	}
	cert, key, err := ca.Issue(nodeID, false)
	if err != nil {
		result = err
		return
	}
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()
	job.Step = "Установка агента"
	appendLog("Подключение к хосту с проверкой SSH fingerprint")
	installStarted := time.Now().UTC().Truncate(time.Second)
	bindAddress := creds.GameBindAddress
	if bindAddress == "" {
		bindAddress = node.GameBindAddress
	}
	if bindAddress == "" {
		bindAddress = "0.0.0.0"
	}
	result = bootstrap.Install(ctx, bootstrap.Config{Address: creds.Address, Port: creds.SSHPort, User: creds.SSHUser, Password: creds.Password, PrivateKey: creds.PrivateKey, HostFingerprint: creds.HostFingerprint, SudoPassword: creds.SudoPassword, NodeID: nodeID, AgentURL: c.AgentURL, AgentBinaryPath: c.AgentBinary, Certificate: cert, PrivateKeyPEM: key, CA: ca.PEM, BindAddress: bindAddress, DownloadUserAgent: c.UserAgent}, appendLog)
	creds.Password = ""
	creds.PrivateKey = ""
	creds.SudoPassword = ""
	if result != nil {
		return
	}
	job.Step = "Ожидание защищённого соединения"
	appendLog("Агент установлен. Ожидаем первый heartbeat")
	heartbeatCtx, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	for {
		if err = s.Get(heartbeatCtx, "node", nodeID, &node); err == nil && node.Status == "online" {
			seen, parseErr := time.Parse(time.RFC3339Nano, node.LastSeen)
			if parseErr == nil && !seen.Before(installStarted) {
				appendLog("Хост подтвердил подключение к панели")
				return
			}
		}
		if !wait(heartbeatCtx, 2*time.Second) {
			result = fmt.Errorf("агент не вышел на связь за 60 секунд; проверьте доступ к %s", c.AgentURL)
			return
		}
	}
}

// finish follows the same node -> work -> job lock order as retry and expiry.
// Expiry is terminal: a late installer result cannot replace its public failure.
func finish(ctx context.Context, s *store.Store, nodeID string, job model.Job) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var lockedNode string
	if err = tx.QueryRow(ctx, `SELECT id FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, nodeID).Scan(&lockedNode); err != nil && !store.IsMissing(err) {
		return err
	}
	var state string
	var unexpired bool
	if err = tx.QueryRow(ctx, `SELECT state,expires_at>now() FROM mp_work WHERE id=$1 AND kind='bootstrap' FOR UPDATE`, job.ID).Scan(&state, &unexpired); err != nil {
		return err
	}
	if state != "running" && state != "queued" {
		if _, err = tx.Exec(ctx, `UPDATE mp_work SET secret=NULL WHERE id=$1`, job.ID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if !unexpired {
		job.Status = "failed"
		job.Step = "Превышено время подключения"
		job.Error = "Срок задания подключения истёк. Повторите подключение хоста"
	}
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT payload FROM mp_entities WHERE kind='job' AND id=$1 FOR UPDATE`, job.ID).Scan(&raw); err != nil {
		return err
	}
	var current model.Job
	if err = json.Unmarshal(raw, &current); err != nil {
		return err
	}
	if current.Status == "queued" || current.Status == "running" {
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err = store.PutTx(ctx, tx, "job", job.ID, job); err != nil {
			return err
		}
	} else {
		job = current
	}
	if _, err = tx.Exec(ctx, `UPDATE mp_work SET state=$2,secret=NULL WHERE id=$1`, job.ID, job.Status); err != nil {
		return err
	}
	if job.Status == "failed" {
		if _, err = tx.Exec(ctx, `UPDATE mp_entities SET payload=payload||jsonb_build_object('status','error','error',$3::text) WHERE kind='node' AND id=$1 AND payload->>'status'<>'online' AND NOT EXISTS(SELECT 1 FROM mp_work WHERE node_id=$1 AND id<>$2 AND state IN ('queued','running') AND expires_at>now())`, nodeID, job.ID, job.Error); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
