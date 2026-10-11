package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
)

type storedUpload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Path      string `json:"path"`
	OwnerID   string `json:"owner_id"`
	CreatedAt string `json:"created_at"`
}

func (a *API) upload(w http.ResponseWriter, r *http.Request) {
	reader, e := r.MultipartReader()
	if e != nil {
		fail(w, 400, "Не удалось прочитать загрузку")
		return
	}
	var src io.ReadCloser
	var filename string
	for {
		part, err := reader.NextPart()
		if err != nil {
			fail(w, 400, "Выберите файл для загрузки")
			return
		}
		if part.FormName() == "file" && part.FileName() != "" {
			src, filename = part, part.FileName()
			break
		}
		if _, err = io.Copy(io.Discard, part); err != nil {
			part.Close()
			fail(w, 400, "Загрузка не завершена")
			return
		}
		part.Close()
	}
	defer src.Close()
	name := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if name == "." || name == "/" || len(name) > 180 || strings.ContainsAny(name, "\x00\r\n") {
		fail(w, 400, "Недопустимое имя файла")
		return
	}
	dir := filepath.Join(a.Config.DataDir, "uploads")
	if os.MkdirAll(dir, 0700) != nil {
		fail(w, 500, "Хранилище загрузок недоступно")
		return
	}
	id := security.ID()
	dstPath := filepath.Join(dir, id)
	dst, e := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		fail(w, 500, "Не удалось сохранить файл")
		return
	}
	h := sha256.New()
	size, e := io.Copy(io.MultiWriter(dst, h), src)
	// Verify the complete multipart envelope before registering the file.
	for e == nil {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			e = err
			break
		}
		if part.FormName() == "file" {
			e = fmt.Errorf("one file per upload request")
			part.Close()
			break
		}
		_, e = io.Copy(io.Discard, part)
		part.Close()
	}
	if e == nil {
		e = dst.Sync()
	}
	closeErr := dst.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		os.Remove(dstPath)
		fail(w, 400, "Загрузка не завершена. Проверьте соединение и свободное место на диске")
		return
	}
	u := storedUpload{ID: id, Name: name, Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), Path: dstPath, OwnerID: actor(r).ID, CreatedAt: now()}
	if a.Store.Put(r.Context(), "upload", id, u) != nil {
		os.Remove(dstPath)
		fail(w, 500, "Не удалось зарегистрировать файл")
		return
	}
	write(w, 201, map[string]any{"id": id, "name": name, "size": size, "sha256": u.SHA256})
}
func (a *API) artifact(ctx context.Context, id string) (model.Artifact, error) {
	var u storedUpload
	if a.Store.Get(ctx, "upload", id, &u) != nil {
		return model.Artifact{}, fmt.Errorf("Загруженный файл не найден")
	}
	if user, ok := ctx.Value(userKey).(model.User); !ok || user.ID == "" || (user.Role == "operator" && u.OwnerID != user.ID) {
		return model.Artifact{}, fmt.Errorf("Загруженный файл не найден")
	}
	return model.Artifact{ID: u.ID, Name: u.Name, URL: strings.TrimRight(a.Config.AgentURL, "/") + "/agent/v1/artifacts/" + u.ID, SHA256: u.SHA256, Size: u.Size}, nil
}
func (a *API) syncCommand(r *http.Request, s model.Server, kind string, payload any, mutating bool) (json.RawMessage, error) {
	if ok, err := a.serverAccess(r.Context(), actor(r), s.ID); err != nil || !ok {
		return nil, fmt.Errorf("Нет доступа к серверу")
	}
	if e := a.readyNode(r.Context(), s.NodeID); e != nil {
		return nil, e
	}
	id := security.ID()
	cmd := model.Command{ID: id, NodeID: s.NodeID, ServerID: s.ID, Type: kind, Payload: store.Marshal(payload), Deadline: time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339)}
	deadline, e := time.Parse(time.RFC3339, cmd.Deadline)
	if e != nil {
		return nil, e
	}
	_, e = a.Store.Pool.Exec(r.Context(), `INSERT INTO mp_commands(id,node_id,server_id,payload,deadline,mutating) VALUES($1,$2,$3,$4,$5,$6)`, id, s.NodeID, s.ID, store.Marshal(cmd), deadline, mutating)
	if e != nil {
		return nil, errBusy
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(46 * time.Second)
	defer timer.Stop()
	for {
		var raw []byte
		e = a.Store.Pool.QueryRow(r.Context(), `SELECT result FROM mp_commands WHERE id=$1`, id).Scan(&raw)
		if e != nil {
			return nil, e
		}
		if len(raw) > 0 {
			if ok, err := a.serverAccess(r.Context(), actor(r), s.ID); err != nil || !ok {
				return nil, fmt.Errorf("Доступ к серверу отозван")
			}
			var result model.CommandResult
			if e = json.Unmarshal(raw, &result); e != nil {
				return nil, e
			}
			if !result.Success {
				return result.Output, fmt.Errorf("%s", result.Error)
			}
			if len(result.Output) == 0 {
				return json.RawMessage(`{"ok":true}`), nil
			}
			return result.Output, nil
		}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-timer.C:
			return nil, fmt.Errorf("Агент не ответил вовремя. Проверьте состояние хоста")
		case <-ticker.C:
		}
	}
}
func (a *API) remote(w http.ResponseWriter, r *http.Request, kind string, payload any, mutating bool) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	if mutating && s.Status != "stopped" && s.Status != "error" {
		fail(w, 409, "Перед изменением файлов остановите сервер")
		return
	}
	out, e := a.syncCommand(r, s, kind, payload, mutating)
	if e != nil {
		fail(w, 409, e.Error())
		return
	}
	if mutating {
		a.audit(r.Context(), actor(r), kind, s.ID, "")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	w.Write(out)
}
func (a *API) files(w http.ResponseWriter, r *http.Request) {
	a.remote(w, r, "file.list", map[string]string{"path": r.URL.Query().Get("path")}, false)
}
func (a *API) fileContent(w http.ResponseWriter, r *http.Request) {
	a.remote(w, r, "file.read", map[string]string{"path": r.URL.Query().Get("path")}, false)
}
func (a *API) writeFile(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Revision string `json:"revision"`
	}
	if !decode(w, r, &p) {
		return
	}
	a.remote(w, r, "file.write", p, true)
}
func (a *API) mkdir(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &p) {
		return
	}
	a.remote(w, r, "file.mkdir", p, true)
}
func (a *API) rename(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Path    string `json:"path"`
		NewPath string `json:"new_path"`
	}
	if !decode(w, r, &p) {
		return
	}
	a.remote(w, r, "file.rename", p, true)
}
func (a *API) deleteFile(w http.ResponseWriter, r *http.Request) {
	a.remote(w, r, "file.delete", map[string]string{"path": r.URL.Query().Get("path")}, true)
}
func (a *API) properties(w http.ResponseWriter, r *http.Request) {
	a.remote(w, r, "properties.read", map[string]string{}, false)
}
func (a *API) writeProperties(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Properties map[string]string `json:"properties"`
		Revision   string            `json:"revision"`
	}
	if !decode(w, r, &p) {
		return
	}
	a.remote(w, r, "properties.write", p, true)
}
func (a *API) extensions(w http.ResponseWriter, r *http.Request) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	dir := extensionDir(s.Core)
	if dir == "" {
		write(w, 200, []model.FileEntry{})
		return
	}
	raw, e := a.syncCommand(r, s, "file.list", map[string]string{"path": dir}, false)
	if e != nil {
		fail(w, 409, e.Error())
		return
	}
	var entries []model.FileEntry
	if json.Unmarshal(raw, &entries) != nil {
		fail(w, 502, "Неверный ответ агента")
		return
	}
	out := []model.FileEntry{}
	for _, f := range entries {
		if !f.IsDir && (strings.HasSuffix(strings.ToLower(f.Name), ".jar") || strings.HasSuffix(strings.ToLower(f.Name), ".jar.disabled")) {
			out = append(out, f)
		}
	}
	write(w, 200, out)
}
func (a *API) toggleExtension(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Path    string `json:"path"`
		Enabled bool   `json:"enabled"`
	}
	if !decode(w, r, &p) {
		return
	}
	a.remote(w, r, "extension.toggle", p, true)
}
func (a *API) fileUpload(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Path     string `json:"path"`
		UploadID string `json:"upload_id"`
	}
	if !decode(w, r, &p) {
		return
	}
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	a.queueUpload(w, r, s, p.Path, p.UploadID)
}
func (a *API) addExtension(w http.ResponseWriter, r *http.Request) {
	var p struct {
		UploadID string `json:"upload_id"`
	}
	if !decode(w, r, &p) {
		return
	}
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	dir := extensionDir(s.Core)
	if dir == "" {
		fail(w, 422, "Выбранное ядро не поддерживает моды/плагины")
		return
	}
	asset, e := a.artifact(r.Context(), p.UploadID)
	if e != nil || !strings.HasSuffix(strings.ToLower(asset.Name), ".jar") {
		fail(w, 400, "Выберите загруженный JAR")
		return
	}
	a.queueUpload(w, r, s, dir+"/"+asset.Name, p.UploadID)
}
func (a *API) queueUpload(w http.ResponseWriter, r *http.Request, s model.Server, target, id string) {
	if s.Status != "stopped" && s.Status != "error" {
		fail(w, 409, "Сначала остановите сервер")
		return
	}
	if e := a.readyNode(r.Context(), s.NodeID); e != nil {
		fail(w, 409, e.Error())
		return
	}
	asset, e := a.artifact(r.Context(), id)
	if e != nil {
		fail(w, 404, e.Error())
		return
	}
	request := map[string]string{"path": target, "upload_id": id}
	if a.replay(w, r, request) {
		return
	}
	j := newJob(s.NodeID, s.ID, "Загрузка файла на игровой хост")
	cmd := model.Command{ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, JobID: j.ID, Type: "file.upload", Payload: store.Marshal(map[string]any{"path": target, "artifact": asset}), Deadline: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}
	if e = a.enqueue(r, cmd, j, nil, true, request); e != nil {
		fail(w, 409, e.Error())
		return
	}
	a.audit(r.Context(), actor(r), "file.upload", s.ID, target)
	write(w, 202, j)
}
func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	a.remote(w, r, "logs", map[string]string{}, false)
}
func (a *API) console(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Command string `json:"command"`
	}
	if !decode(w, r, &p) {
		return
	}
	if p.Command == "" || len(p.Command) > 4096 || strings.ContainsAny(p.Command, "\r\n\x00") {
		fail(w, 400, "Введите одну команду длиной до 4096 символов")
		return
	}
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	_, e := a.syncCommand(r, s, "console", p, false)
	if e != nil {
		fail(w, 409, e.Error())
		return
	}
	a.audit(r.Context(), actor(r), "console.write", s.ID, "Команда отправлена; содержимое не сохраняется в аудите")
	write(w, 200, map[string]bool{"ok": true})
}
func (a *API) backups(w http.ResponseWriter, r *http.Request) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	all := []model.Backup{}
	if a.Store.List(r.Context(), "backup", &all) != nil {
		fail(w, 500, "Не удалось получить копии")
		return
	}
	out := []model.Backup{}
	for _, b := range all {
		if b.ServerID == s.ID {
			out = append(out, b)
		}
	}
	write(w, 200, out)
}
func (a *API) backup(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name string `json:"name"`
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
	if s.Status != "stopped" {
		fail(w, 409, "Остановите сервер перед резервным копированием")
		return
	}
	if e := a.readyNode(r.Context(), s.NodeID); e != nil {
		fail(w, 409, e.Error())
		return
	}
	if len(p.Name) > 120 {
		fail(w, 400, "Имя копии слишком длинное")
		return
	}
	originalRequest := p
	if p.Name == "" {
		p.Name = "Копия " + time.Now().UTC().Format("2006-01-02 15:04 UTC")
	}
	j := newJob(s.NodeID, s.ID, "Резервная копия")
	cmd := model.Command{ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, JobID: j.ID, Type: "backup", Payload: store.Marshal(map[string]string{"backup_id": security.ID(), "name": p.Name}), Deadline: time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339)}
	if e := a.enqueue(r, cmd, j, nil, true, originalRequest); e != nil {
		fail(w, 409, e.Error())
		return
	}
	a.audit(r.Context(), actor(r), "backup.create", s.ID, "")
	write(w, 202, j)
}
func (a *API) restore(w http.ResponseWriter, r *http.Request) {
	var p struct {
		BackupID string `json:"backup_id"`
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
	var b model.Backup
	if a.Store.Get(r.Context(), "backup", p.BackupID, &b) != nil || b.ServerID != s.ID || b.NodeID != s.NodeID {
		fail(w, 404, "Копия этого сервера не найдена")
		return
	}
	if s.Status != "stopped" && s.Status != "error" {
		fail(w, 409, "Остановите сервер перед восстановлением")
		return
	}
	if e := a.readyNode(r.Context(), s.NodeID); e != nil {
		fail(w, 409, e.Error())
		return
	}
	j := newJob(s.NodeID, s.ID, "Восстановление резервной копии")
	cmd := model.Command{ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, JobID: j.ID, Type: "restore", Payload: store.Marshal(p), Deadline: time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339)}
	s.Status = "restoring"
	s.DesiredState = "stopped"
	if e := a.enqueue(r, cmd, j, &s, true, p); e != nil {
		fail(w, 409, e.Error())
		return
	}
	a.audit(r.Context(), actor(r), "backup.restore", s.ID, p.BackupID)
	write(w, 202, j)
}
func (a *API) nodeMetrics(w http.ResponseWriter, r *http.Request) {
	if actor(r).Role == "operator" {
		fail(w, 403, "Оператору доступны только метрики назначенных серверов")
		return
	}
	a.metrics(w, r, r.PathValue("id"), "")
}
func (a *API) serverMetrics(w http.ResponseWriter, r *http.Request) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	a.metrics(w, r, s.NodeID, s.ID)
}
func (a *API) metrics(w http.ResponseWriter, r *http.Request, nodeID, serverID string) {
	rows, e := a.Store.Pool.Query(r.Context(), `SELECT payload FROM (SELECT payload,measured_at FROM mp_metrics WHERE node_id=$1 AND server_id=$2 ORDER BY measured_at DESC LIMIT 240) samples ORDER BY measured_at`, nodeID, serverID)
	if e != nil {
		fail(w, 500, "Не удалось получить метрики")
		return
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			fail(w, 500, "Ошибка истории метрик")
			return
		}
		out = append(out, b)
	}
	if rows.Err() != nil {
		fail(w, 503, "Не удалось завершить загрузку данных")
		return
	}
	write(w, 200, out)
}
func (a *API) auditList(w http.ResponseWriter, r *http.Request) {
	rows, e := a.Store.Pool.Query(r.Context(), `SELECT id,actor,action,object_id,created_at,detail FROM mp_audit ORDER BY created_at DESC LIMIT 500`)
	if e != nil {
		fail(w, 500, "Не удалось получить аудит")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, actor, action, object, detail string
		var at time.Time
		if rows.Scan(&id, &actor, &action, &object, &at, &detail) != nil {
			fail(w, 500, "Ошибка журнала аудита")
			return
		}
		out = append(out, map[string]any{"id": id, "actor": actor, "action": action, "object_id": object, "created_at": at.UTC().Format(time.RFC3339), "detail": detail})
	}
	if rows.Err() != nil {
		fail(w, 503, "Не удалось завершить загрузку аудита")
		return
	}
	write(w, 200, out)
}
