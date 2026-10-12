package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/bootstrap"
	"github.com/come0th3r/mp-panel/internal/catalog"
	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
)

func now() string { return time.Now().UTC().Format(time.RFC3339) }
func (a *API) list(w http.ResponseWriter, r *http.Request, kind string) {
	var out []json.RawMessage
	if e := a.accessibleList(r, kind, &out); e != nil {
		fail(w, 500, "Не удалось получить данные")
		return
	}
	write(w, 200, out)
}
func (a *API) jobs(w http.ResponseWriter, r *http.Request) { a.list(w, r, "job") }
func (a *API) job(w http.ResponseWriter, r *http.Request) {
	var j model.Job
	if a.Store.Get(r.Context(), "job", r.PathValue("id"), &j) != nil {
		fail(w, 404, "Задание не найдено")
		return
	}
	if !a.checkServerAccess(w, r, actor(r), j.ServerID) {
		return
	}
	write(w, 200, j)
}
func visibleNode(n model.Node) model.Node {
	if n.LastSeen != "" {
		t, e := time.Parse(time.RFC3339, n.LastSeen)
		if e != nil || time.Since(t) > 45*time.Second {
			if n.Status != "error" {
				n.Status = "offline"
			}
		}
	}
	return n
}
func (a *API) nodes(w http.ResponseWriter, r *http.Request) {
	out := []model.Node{}
	if a.accessibleList(r, "node", &out) != nil {
		fail(w, 500, "Не удалось получить хосты")
		return
	}
	for i := range out {
		out[i] = visibleNode(out[i])
		if actor(r).Role == "operator" {
			out[i] = operatorVisibleNode(out[i])
		}
	}
	write(w, 200, out)
}
func (a *API) node(w http.ResponseWriter, r *http.Request) {
	var n model.Node
	if a.Store.Get(r.Context(), "node", r.PathValue("id"), &n) != nil {
		fail(w, 404, "Хост не найден")
		return
	}
	n = visibleNode(n)
	if actor(r).Role == "operator" {
		n = operatorVisibleNode(n)
	}
	write(w, 200, n)
}
func validAddress(address string) bool {
	if len(address) == 0 || len(address) > 253 || strings.ContainsAny(address, "\n\r\x00 /@\\") {
		return false
	}
	return net.ParseIP(address) != nil || regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`).MatchString(address)
}
func (a *API) fingerprint(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Address string `json:"address"`
		Port    int    `json:"ssh_port"`
		User    string `json:"ssh_user"`
	}
	if !decode(w, r, &p) {
		return
	}
	if p.Port == 0 {
		p.Port = 22
	}
	if !validAddress(p.Address) || p.Port < 1 || p.Port > 65535 {
		fail(w, 400, "Укажите корректный SSH-адрес и порт")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	fp, e := bootstrap.Fingerprint(ctx, p.Address, p.Port)
	if e != nil {
		fail(w, 502, "Не удалось получить SSH-ключ хоста: "+e.Error())
		return
	}
	write(w, 200, map[string]string{"fingerprint": fp})
}

type bootstrapRequest struct {
	Name            string  `json:"name"`
	Address         string  `json:"address"`
	Port            int     `json:"ssh_port"`
	User            string  `json:"ssh_user"`
	Password        string  `json:"password,omitempty"`
	PrivateKey      string  `json:"private_key,omitempty"`
	Fingerprint     string  `json:"host_fingerprint"`
	SudoPassword    string  `json:"sudo_password,omitempty"`
	GameBindAddress *string `json:"game_bind_address,omitempty"`
}

func (a *API) bootstrapNode(w http.ResponseWriter, r *http.Request) {
	var p bootstrapRequest
	if !decode(w, r, &p) {
		return
	}
	originalRequest := p
	if p.Port == 0 {
		p.Port = 22
	}
	if p.GameBindAddress != nil {
		ip := net.ParseIP(strings.TrimSpace(*p.GameBindAddress))
		if ip == nil {
			fail(w, 400, "Укажите IP-адрес игрового интерфейса этого хоста")
			return
		}
		canonical := ip.String()
		p.GameBindAddress = &canonical
	}
	if len(p.Name) < 1 || len(p.Name) > 80 || !validAddress(p.Address) || p.Port < 1 || p.Port > 65535 || p.User == "" || len(p.User) > 80 || p.Fingerprint == "" || (p.Password == "" && p.PrivateKey == "") {
		fail(w, 400, "Заполните имя, SSH-доступ и подтвержденный отпечаток хоста")
		return
	}
	if _, e := os.Stat(a.Config.AgentBinary); e != nil {
		fail(w, 503, "Linux-бинарник агента отсутствует в установке панели")
		return
	}
	if a.replay(w, r, originalRequest) {
		return
	}
	n := model.Node{ID: security.ID(), Name: p.Name, Address: p.Address, SSHPort: p.Port, SSHUser: p.User, Status: "connecting", CreatedAt: now()}
	j := newJob(n.ID, "", "Подключение хоста по SSH")
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	defer tx.Rollback(r.Context())
	if id := r.PathValue("id"); id != "" {
		var raw []byte
		if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, id).Scan(&raw); e != nil || json.Unmarshal(raw, &n) != nil {
			fail(w, 404, "Хост не найден")
			return
		}
		var active bool
		if e = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM mp_work WHERE node_id=$1 AND state IN ('queued','running'))`, id).Scan(&active); e != nil {
			fail(w, 503, "Не удалось проверить подключение")
			return
		}
		if active {
			fail(w, 409, "Подключение этого хоста уже выполняется")
			return
		}
		if e = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM mp_commands WHERE node_id=$1 AND mutating AND state IN ('pending','delivered'))`, id).Scan(&active); e != nil {
			fail(w, 503, "Не удалось проверить задания игровых серверов")
			return
		}
		if active {
			fail(w, 409, "Дождитесь завершения заданий игровых серверов перед обновлением агента")
			return
		}
		n.Name, n.Address, n.SSHPort, n.SSHUser, n.Status, n.Error = p.Name, p.Address, p.Port, p.User, "connecting", ""
		j.NodeID = id
	}
	if p.GameBindAddress != nil {
		n.GameBindAddress = *p.GameBindAddress
	} else if n.GameBindAddress == "" {
		n.GameBindAddress = "0.0.0.0"
	}
	p.GameBindAddress = &n.GameBindAddress
	secret, e := a.Vault.Encrypt(store.Marshal(p))
	if e != nil {
		fail(w, 500, "Не удалось защитить SSH-данные")
		return
	}
	if e = store.PutTx(r.Context(), tx, "node", n.ID, n); e == nil {
		e = store.PutTx(r.Context(), tx, "job", j.ID, j)
	}
	if e == nil {
		_, e = tx.Exec(r.Context(), `INSERT INTO mp_work(id,kind,node_id,secret) VALUES($1,'bootstrap',$2,$3)`, j.ID, n.ID, secret)
	}
	if e == nil {
		e = a.saveIdempotency(r, tx, originalRequest, j)
	}
	if e != nil {
		fail(w, 409, "Хост уже подключен или задание не удалось сохранить")
		return
	}
	if tx.Commit(r.Context()) != nil {
		fail(w, 500, "Не удалось сохранить подключение")
		return
	}
	a.audit(r.Context(), actor(r), "node.bootstrap", n.ID, "")
	write(w, 202, j)
}
func (a *API) deleteNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 500, "Ошибка базы данных")
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, id).Scan(&raw); e != nil {
		fail(w, 404, "Хост не найден")
		return
	}
	var count int
	if e = tx.QueryRow(r.Context(), `SELECT count(*) FROM mp_entities WHERE kind='server' AND payload->>'node_id'=$1`, id).Scan(&count); e != nil || count > 0 {
		fail(w, 409, "Сначала удалите игровые серверы этого хоста")
		return
	}
	var work int
	if e = tx.QueryRow(r.Context(), `SELECT count(*) FROM mp_work WHERE node_id=$1 AND state IN ('queued','running')`, id).Scan(&work); e != nil {
		fail(w, 503, "Не удалось проверить задания хоста")
		return
	}
	if work > 0 {
		fail(w, 409, "Дождитесь завершения подключения хоста")
		return
	}
	if _, e = tx.Exec(r.Context(), `DELETE FROM mp_entities WHERE kind='node' AND id=$1`, id); e != nil || tx.Commit(r.Context()) != nil {
		fail(w, 500, "Не удалось отключить хост")
		return
	}
	a.audit(r.Context(), actor(r), "node.delete", id, "")
	write(w, 200, map[string]bool{"ok": true})
}
func (a *API) versions(w http.ResponseWriter, r *http.Request) {
	out, e := a.Catalog.Versions(r.Context(), r.PathValue("core"))
	if e != nil {
		fail(w, 502, e.Error())
		return
	}
	write(w, 200, out)
}
func (a *API) builds(w http.ResponseWriter, r *http.Request) {
	out, e := a.Catalog.Builds(r.Context(), r.PathValue("core"), r.PathValue("version"))
	if e != nil {
		fail(w, 502, e.Error())
		return
	}
	write(w, 200, out)
}
func (a *API) resolve(w http.ResponseWriter, r *http.Request) {
	var p catalog.ResolveRequest
	if !decode(w, r, &p) {
		return
	}
	out, e := a.Catalog.Resolve(r.Context(), p)
	if e != nil {
		fail(w, 422, e.Error())
		return
	}
	write(w, 200, out)
}
func (a *API) servers(w http.ResponseWriter, r *http.Request) {
	out := []model.Server{}
	if a.accessibleList(r, "server", &out) != nil {
		fail(w, 500, "Не удалось получить серверы")
		return
	}
	for i := range out {
		out[i] = a.visibleServer(r.Context(), out[i])
	}
	write(w, 200, out)
}
func (a *API) visibleServer(ctx context.Context, s model.Server) model.Server {
	var n model.Node
	if a.Store.Get(ctx, "node", s.NodeID, &n) != nil || visibleNode(n).Status != "online" {
		if s.Status != "creating" {
			s.Status = "unknown"
		}
	}
	return s
}
func (a *API) server(w http.ResponseWriter, r *http.Request) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	write(w, 200, a.visibleServer(r.Context(), s))
}
func (a *API) getServer(w http.ResponseWriter, r *http.Request) (model.Server, bool) {
	var s model.Server
	if a.Store.Get(r.Context(), "server", r.PathValue("id"), &s) != nil {
		fail(w, 404, "Сервер не найден")
		return s, false
	}
	return s, true
}
func (a *API) readyNode(ctx context.Context, id string) error {
	var n model.Node
	if a.Store.Get(ctx, "node", id, &n) != nil {
		return fmt.Errorf("Хост не найден")
	}
	if visibleNode(n).Status != "online" {
		return fmt.Errorf("Агент хоста не на связи. Дождитесь подключения")
	}
	return nil
}

type createRequest struct {
	Name             string   `json:"name"`
	NodeID           string   `json:"node_id"`
	Core             string   `json:"core"`
	MinecraftVersion string   `json:"minecraft_version"`
	LoaderVersion    string   `json:"loader_version,omitempty"`
	Build            string   `json:"build,omitempty"`
	JavaVersion      int      `json:"java_version,omitempty"`
	MemoryMB         int      `json:"memory_mb"`
	CPULimit         float64  `json:"cpu_limit"`
	Port             int      `json:"port"`
	EULAAccepted     bool     `json:"eula_accepted"`
	JarUploadID      string   `json:"jar_upload_id,omitempty"`
	ExtraUploadIDs   []string `json:"extra_upload_ids,omitempty"`
}

func (a *API) resolveCreate(r *http.Request, p createRequest) (model.InstallSpec, error) {
	spec, e := a.Catalog.Resolve(r.Context(), catalog.ResolveRequest{Core: p.Core, MinecraftVersion: p.MinecraftVersion, LoaderVersion: p.LoaderVersion, Build: p.Build, JavaVersion: p.JavaVersion})
	if e != nil {
		return spec, e
	}
	if p.Core == "custom" {
		if p.JarUploadID == "" {
			return spec, fmt.Errorf("Выберите JAR собственного ядра")
		}
		asset, e := a.artifact(r.Context(), p.JarUploadID)
		if e != nil {
			return spec, e
		}
		if !strings.HasSuffix(strings.ToLower(asset.Name), ".jar") {
			return spec, fmt.Errorf("Ядро должно быть JAR-файлом")
		}
		asset.Target = "server.jar"
		spec.Artifacts = []model.Artifact{asset}
	}
	if len(p.ExtraUploadIDs) > 100 {
		return spec, fmt.Errorf("Не более 100 дополнительных JAR за одну установку")
	}
	dir := extensionDir(p.Core)
	if len(p.ExtraUploadIDs) > 0 && dir == "" {
		return spec, fmt.Errorf("Это ядро не поддерживает установку модов/плагинов")
	}
	seen := map[string]bool{}
	for _, id := range p.ExtraUploadIDs {
		asset, e := a.artifact(r.Context(), id)
		if e != nil {
			return spec, e
		}
		if !strings.HasSuffix(strings.ToLower(asset.Name), ".jar") {
			return spec, fmt.Errorf("Дополнительные файлы должны быть JAR")
		}
		if seen[asset.Name] {
			return spec, fmt.Errorf("Названия дополнительных JAR не должны повторяться")
		}
		seen[asset.Name] = true
		asset.Target = dir + "/" + asset.Name
		spec.ExtraJars = append(spec.ExtraJars, asset)
	}
	return spec, nil
}
func (a *API) createServer(w http.ResponseWriter, r *http.Request) {
	var p createRequest
	if !decode(w, r, &p) {
		return
	}
	if a.replay(w, r, p) {
		return
	}
	if len(p.Name) < 1 || len(p.Name) > 80 || p.MemoryMB < 512 || p.MemoryMB > 1048576 || p.CPULimit <= 0 || p.CPULimit > 1024 || p.Port < 1 || p.Port > 65535 || !p.EULAAccepted {
		fail(w, 400, "Проверьте имя, RAM от 512 МиБ, CPU, порт и согласие с EULA")
		return
	}
	if e := a.readyNode(r.Context(), p.NodeID); e != nil {
		fail(w, 409, e.Error())
		return
	}
	spec, e := a.resolveCreate(r, p)
	if e != nil {
		fail(w, 422, e.Error())
		return
	}
	s := model.Server{ID: security.ID(), NodeID: p.NodeID, Name: p.Name, Core: p.Core, MinecraftVersion: spec.MinecraftVersion, LoaderVersion: spec.LoaderVersion, Build: spec.Build, JavaVersion: spec.JavaVersion, MemoryMB: p.MemoryMB, CPULimit: p.CPULimit, Port: p.Port, Status: "creating", DesiredState: "running", EULAAccepted: true, CreatedAt: now()}
	j := newJob(s.NodeID, s.ID, "Создание сервера")
	cmd := model.Command{ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, Type: "create", Server: &s, Install: &spec, JobID: j.ID, Deadline: time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)}
	if e = a.enqueue(r, cmd, j, &s, true, p); e != nil {
		fail(w, 409, e.Error())
		return
	}
	a.audit(r.Context(), actor(r), "server.create", s.ID, "EULA accepted; https://www.minecraft.net/en-us/eula")
	write(w, 202, j)
}
func newJob(nodeID, serverID, kind string) model.Job {
	return model.Job{ID: security.ID(), NodeID: nodeID, ServerID: serverID, Type: kind, Status: "queued", Step: "Ожидает агента", Logs: []string{}, CreatedAt: now(), UpdatedAt: now()}
}
func (a *API) enqueue(r *http.Request, cmd model.Command, j model.Job, s *model.Server, mutating bool, request any) error {
	// Resolving an installer can take time. Revalidate the grant before queuing
	// work rather than retaining the permission from the start of the request.
	if cmd.ServerID != "" {
		if ok, err := a.serverAccess(r.Context(), actor(r), cmd.ServerID); err != nil || !ok {
			return fmt.Errorf("Нет доступа к серверу")
		}
	}
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	if e = a.lockUploads(r.Context(), tx, cmd); e != nil {
		return e
	}
	var n []byte
	if e = tx.QueryRow(r.Context(), `SELECT payload FROM mp_entities WHERE kind='node' AND id=$1 FOR UPDATE`, cmd.NodeID).Scan(&n); e != nil {
		return fmt.Errorf("Хост не найден")
	}
	if s != nil {
		if e = store.PutTx(r.Context(), tx, "server", s.ID, s); e != nil {
			return fmt.Errorf("Порт на хосте уже занят или сервер не удалось сохранить")
		}
	}
	if e = store.PutTx(r.Context(), tx, "job", j.ID, j); e == nil {
		deadline, parseErr := time.Parse(time.RFC3339, cmd.Deadline)
		if parseErr != nil {
			return parseErr
		}
		_, e = tx.Exec(r.Context(), `INSERT INTO mp_commands(id,node_id,server_id,job_id,payload,deadline,mutating) VALUES($1,$2,$3,$4,$5,$6,$7)`, cmd.ID, cmd.NodeID, cmd.ServerID, j.ID, store.Marshal(cmd), deadline, mutating)
	}
	if e == nil {
		e = a.saveIdempotency(r, tx, request, j)
	}
	if e != nil {
		var pe *pgconn.PgError
		if errorsAs(e, &pe) && pe.Code == "23505" {
			return errBusy
		}
		return e
	}
	return tx.Commit(r.Context())
}
func errorsAs(e error, out **pgconn.PgError) bool {
	return errors.As(e, out)
}
func (a *API) serverAction(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Action string `json:"action"`
	}
	if !decode(w, r, &p) {
		return
	}
	if a.replay(w, r, p) {
		return
	}
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	if p.Action != "start" && p.Action != "stop" && p.Action != "restart" && p.Action != "delete" {
		fail(w, 400, "Неизвестное действие")
		return
	}
	if e := a.readyNode(r.Context(), s.NodeID); e != nil {
		fail(w, 409, e.Error())
		return
	}
	if p.Action == "delete" && s.Status != "stopped" && s.Status != "error" {
		fail(w, 409, "Сначала остановите сервер")
		return
	}
	names := map[string]string{"start": "Запуск сервера", "stop": "Остановка сервера", "restart": "Перезапуск сервера", "delete": "Удаление сервера"}
	j := newJob(s.NodeID, s.ID, names[p.Action])
	switch p.Action {
	case "start", "restart":
		s.Status = "starting"
		s.DesiredState = "running"
	case "stop", "delete":
		s.Status = "stopping"
		s.DesiredState = "stopped"
	}
	s.Error = ""
	cmd := model.Command{ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, JobID: j.ID, Type: p.Action, Server: &s, Deadline: time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)}
	if e := a.enqueue(r, cmd, j, &s, true, p); e != nil {
		fail(w, 409, e.Error())
		return
	}
	a.audit(r.Context(), actor(r), "server."+p.Action, s.ID, "")
	write(w, 202, j)
}
func (a *API) installServer(w http.ResponseWriter, r *http.Request) {
	var p createRequest
	if !decode(w, r, &p) {
		return
	}
	if a.replay(w, r, p) {
		return
	}
	originalRequest := p
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	if !p.EULAAccepted {
		fail(w, 400, "Подтвердите согласие с EULA")
		return
	}
	if e := a.readyNode(r.Context(), s.NodeID); e != nil {
		fail(w, 409, e.Error())
		return
	}
	if p.Core == "" {
		p.Core = s.Core
	}
	if p.JavaVersion == 0 && p.Core == "custom" {
		p.JavaVersion = s.JavaVersion
	}
	spec, e := a.resolveCreate(r, p)
	if e != nil {
		fail(w, 422, e.Error())
		return
	}
	target := s
	target.Core = spec.Core
	target.MinecraftVersion = spec.MinecraftVersion
	target.LoaderVersion = spec.LoaderVersion
	target.Build = spec.Build
	target.JavaVersion = spec.JavaVersion
	target.Status = "updating"
	target.DesiredState = "running"
	target.Error = ""
	backupID := security.ID()
	j := newJob(s.NodeID, s.ID, "Обновление ядра и резервная копия")
	cmd := model.Command{ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, JobID: j.ID, Type: "install", Server: &target, Install: &spec, Payload: store.Marshal(map[string]string{"backup_id": backupID, "backup_name": "Перед обновлением ядра"}), Deadline: time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)}
	s.Status = "updating"
	s.Error = ""
	if e = a.enqueue(r, cmd, j, &s, true, originalRequest); e != nil {
		fail(w, 409, e.Error())
		return
	}
	a.audit(r.Context(), actor(r), "server.install", s.ID, p.Core+" "+p.MinecraftVersion+"; EULA accepted")
	write(w, 202, j)
}
func extensionDir(core string) string {
	switch core {
	case "paper", "spigot":
		return "plugins"
	case "fabric", "forge", "neoforge":
		return "mods"
	}
	return ""
}

func (a *API) replay(w http.ResponseWriter, r *http.Request, request any) bool {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return false
	}
	if len(key) > 128 {
		fail(w, 400, "Слишком длинный Idempotency-Key")
		return true
	}
	var hash string
	var raw []byte
	e := a.Store.Pool.QueryRow(r.Context(), `SELECT request_hash,response FROM mp_idempotency WHERE actor_id=$1 AND key=$2`, actor(r).ID, key).Scan(&hash, &raw)
	if store.IsMissing(e) {
		return false
	}
	if e != nil {
		fail(w, 503, "Не удалось проверить повтор операции")
		return true
	}
	expected := security.Hash(r.Method + " " + r.URL.Path + " " + string(store.Marshal(request)))
	if hash != expected {
		fail(w, 409, "Idempotency-Key уже использован для другого запроса")
		return true
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		fail(w, 500, "Ошибка сохраненного ответа")
		return true
	}
	write(w, 202, out)
	return true
}
func (a *API) saveIdempotency(r *http.Request, tx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, request, response any) error {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil
	}
	hash := security.Hash(r.Method + " " + r.URL.Path + " " + string(store.Marshal(request)))
	_, e := tx.Exec(r.Context(), `INSERT INTO mp_idempotency(actor_id,key,request_hash,response) VALUES($1,$2,$3,$4)`, actor(r).ID, key, hash, store.Marshal(response))
	return e
}
