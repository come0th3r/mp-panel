package api

import (
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
)

const fileBatchLimit = 100

// Batch operations require canonical relative paths so validation and the
// agent address exactly the same entries. Only a destination may be root.
func validateFilePath(value string, rootAllowed bool) error {
	if value == "" && rootAllowed {
		return nil
	}
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\:") || path.Clean(value) != value || value == "." {
		return fmt.Errorf("Укажите относительный путь внутри сервера длиной до 4096 байт")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." || part == "" {
			return fmt.Errorf("Путь не должен содержать переходы в родительский каталог")
		}
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return fmt.Errorf("Путь содержит недопустимый управляющий символ")
		}
	}
	return nil
}

func validateFileSelection(paths []string) error {
	if len(paths) == 0 || len(paths) > fileBatchLimit {
		return fmt.Errorf("Выберите от 1 до %d файлов или каталогов", fileBatchLimit)
	}
	for i, value := range paths {
		if err := validateFilePath(value, false); err != nil {
			return err
		}
		for _, previous := range paths[:i] {
			if value == previous || strings.HasPrefix(value, previous+"/") || strings.HasPrefix(previous, value+"/") {
				return fmt.Errorf("Не выбирайте один путь дважды или каталог вместе с его содержимым")
			}
		}
	}
	return nil
}

func (a *API) fileMutationServer(w http.ResponseWriter, r *http.Request) (model.Server, bool) {
	s, ok := a.getServer(w, r)
	if !ok {
		return s, false
	}
	if s.Status != "stopped" && s.Status != "error" {
		fail(w, http.StatusConflict, "Перед изменением файлов остановите сервер")
		return s, false
	}
	if err := a.readyNode(r.Context(), s.NodeID); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return s, false
	}
	return s, true
}

func (a *API) queueFileOperation(w http.ResponseWriter, r *http.Request, s model.Server, kind, title string, request, payload any) {
	j := newJob(s.NodeID, s.ID, title)
	cmd := model.Command{
		ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, JobID: j.ID,
		Type: kind, Payload: store.Marshal(payload),
		Deadline: time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339),
	}
	if err := a.enqueue(r, cmd, j, nil, true, request); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	a.audit(r.Context(), actor(r), kind, s.ID, title)
	write(w, http.StatusAccepted, j)
}

func (a *API) transferFiles(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Paths       []string `json:"paths"`
		Destination string   `json:"destination"`
		Mode        string   `json:"mode"`
	}
	if !decode(w, r, &p) || a.replay(w, r, p) {
		return
	}
	if p.Mode != "copy" && p.Mode != "move" {
		fail(w, http.StatusBadRequest, "Выберите копирование или перемещение")
		return
	}
	if err := validateFileSelection(p.Paths); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateFilePath(p.Destination, true); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	seen := make(map[string]bool, len(p.Paths))
	for _, source := range p.Paths {
		if source == p.Destination || strings.HasPrefix(p.Destination, source+"/") {
			fail(w, http.StatusBadRequest, "Нельзя копировать или перемещать каталог внутрь самого себя")
			return
		}
		target := path.Join(p.Destination, path.Base(source))
		if err := validateFilePath(target, false); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if p.Mode == "move" && target == source {
			fail(w, http.StatusBadRequest, "Каталог назначения совпадает с исходным")
			return
		}
		if seen[target] {
			fail(w, http.StatusBadRequest, "В каталоге назначения совпадают имена выбранных файлов")
			return
		}
		seen[target] = true
	}
	s, ok := a.fileMutationServer(w, r)
	if !ok {
		return
	}
	title := "Копирование файлов"
	if p.Mode == "move" {
		title = "Перемещение файлов"
	}
	a.queueFileOperation(w, r, s, "file.transfer", title, p, p)
}

func (a *API) extractFiles(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Path        string `json:"path"`
		Destination string `json:"destination"`
	}
	if !decode(w, r, &p) || a.replay(w, r, p) {
		return
	}
	if err := validateFilePath(p.Path, false); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateFilePath(p.Destination, true); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	extension := strings.ToLower(path.Ext(p.Path))
	if extension != ".zip" && extension != ".rar" {
		fail(w, http.StatusBadRequest, "Поддерживаются архивы ZIP и RAR")
		return
	}
	if p.Destination == p.Path || strings.HasPrefix(p.Destination, p.Path+"/") {
		fail(w, http.StatusBadRequest, "Каталог назначения не может совпадать с архивом")
		return
	}
	s, ok := a.fileMutationServer(w, r)
	if !ok {
		return
	}
	a.queueFileOperation(w, r, s, "file.extract", "Распаковка архива", p, p)
}

func (a *API) uploadFiles(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Files []struct {
			Path     string `json:"path"`
			UploadID string `json:"upload_id"`
		} `json:"files"`
	}
	if !decode(w, r, &p) || a.replay(w, r, p) {
		return
	}
	paths := make([]string, len(p.Files))
	for i, file := range p.Files {
		paths[i] = file.Path
		if file.UploadID == "" || len(file.UploadID) > 128 {
			fail(w, http.StatusBadRequest, "Для каждого файла нужен идентификатор завершённой загрузки")
			return
		}
	}
	if err := validateFileSelection(paths); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s, ok := a.fileMutationServer(w, r)
	if !ok {
		return
	}
	type uploadEntry struct {
		Path     string         `json:"path"`
		Artifact model.Artifact `json:"artifact"`
	}
	files := make([]uploadEntry, 0, len(p.Files))
	for _, file := range p.Files {
		asset, err := a.artifact(r.Context(), file.UploadID)
		if err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		files = append(files, uploadEntry{Path: file.Path, Artifact: asset})
	}
	a.queueFileOperation(w, r, s, "file.upload_batch", "Загрузка файлов на игровой хост", p, struct {
		Files []uploadEntry `json:"files"`
	}{Files: files})
}

func (a *API) deleteFiles(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Paths []string `json:"paths"`
	}
	if !decode(w, r, &p) || a.replay(w, r, p) {
		return
	}
	if err := validateFileSelection(p.Paths); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s, ok := a.fileMutationServer(w, r)
	if !ok {
		return
	}
	a.queueFileOperation(w, r, s, "file.delete_batch", "Удаление выбранных файлов", p, p)
}
