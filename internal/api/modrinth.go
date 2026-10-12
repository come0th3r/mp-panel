package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/modrinth"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
)

var modrinthIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

type modrinthRequest struct {
	ProjectID string `json:"project_id"`
	VersionID string `json:"version_id"`
	PlanHash  string `json:"plan_hash,omitempty"`
}

type modrinthPreview struct {
	modrinth.Plan
	PlanHash string `json:"plan_hash"`
}

func (a *API) modrinthServer(w http.ResponseWriter, r *http.Request) (model.Server, bool) {
	s, ok := a.getServer(w, r)
	if !ok {
		return s, false
	}
	if extensionDir(s.Core) == "" {
		fail(w, http.StatusUnprocessableEntity, "Каталог доступен для Fabric, Forge, NeoForge, Paper и Spigot")
		return s, false
	}
	return s, true
}

func (a *API) modrinthSearch(w http.ResponseWriter, r *http.Request) {
	s, ok := a.modrinthServer(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > 200 {
		fail(w, http.StatusBadRequest, "Поисковый запрос должен содержать не более 200 символов")
		return
	}
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "downloads"
	}
	if sort != "downloads" && sort != "relevance" && sort != "updated" && sort != "newest" {
		fail(w, http.StatusBadRequest, "Выберите допустимую сортировку каталога")
		return
	}
	offset := 0
	if value := r.URL.Query().Get("offset"); value != "" {
		var err error
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > 10000 {
			fail(w, http.StatusBadRequest, "Недопустимая страница каталога")
			return
		}
	}
	result, err := a.Modrinth.Search(r.Context(), s.Core, s.MinecraftVersion, query, sort, offset)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	write(w, http.StatusOK, result)
}

func (a *API) modrinthVersions(w http.ResponseWriter, r *http.Request) {
	s, ok := a.modrinthServer(w, r)
	if !ok {
		return
	}
	project := r.PathValue("project")
	if !modrinthIdentifier.MatchString(project) {
		fail(w, http.StatusBadRequest, "Недопустимый проект Modrinth")
		return
	}
	result, err := a.Modrinth.Versions(r.Context(), s.Core, s.MinecraftVersion, project)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	write(w, http.StatusOK, result)
}

func decodeModrinthRequest(w http.ResponseWriter, r *http.Request, p *modrinthRequest) bool {
	if !decodeLimit(w, r, p, 4096) {
		return false
	}
	if !modrinthIdentifier.MatchString(p.ProjectID) || !modrinthIdentifier.MatchString(p.VersionID) {
		fail(w, http.StatusBadRequest, "Выберите проект и версию Modrinth")
		return false
	}
	return true
}

func (a *API) modrinthPlan(w http.ResponseWriter, r *http.Request) {
	var p modrinthRequest
	if !decodeModrinthRequest(w, r, &p) {
		return
	}
	s, ok := a.modrinthServer(w, r)
	if !ok {
		return
	}
	plan, err := a.Modrinth.Resolve(r.Context(), s.Core, s.MinecraftVersion, p.ProjectID, p.VersionID)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	write(w, http.StatusOK, modrinthPreview{Plan: plan, PlanHash: security.Hash(string(store.Marshal(plan)))})
}

func supportsModrinthInstall(value string) bool {
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return false
	}
	patch, err := strconv.Atoi(parts[2])
	return err == nil && patch >= 0 && (major > 0 || minor >= 4)
}

func (a *API) modrinthInstall(w http.ResponseWriter, r *http.Request) {
	var p modrinthRequest
	if !decodeModrinthRequest(w, r, &p) || a.replay(w, r, p) {
		return
	}
	s, ok := a.modrinthServer(w, r)
	if !ok {
		return
	}
	if s.Status != "stopped" && s.Status != "error" {
		fail(w, http.StatusConflict, "Перед установкой модов или плагинов остановите сервер")
		return
	}
	if err := a.readyNode(r.Context(), s.NodeID); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	var node model.Node
	if err := a.Store.Get(r.Context(), "node", s.NodeID, &node); err != nil || !supportsModrinthInstall(node.AgentVersion) {
		fail(w, http.StatusConflict, "Для установки из Modrinth обновите агент игрового хоста до v0.4.0 или новее: «Хосты» → нужный хост → «Обновить агент»")
		return
	}
	plan, err := a.Modrinth.Resolve(r.Context(), s.Core, s.MinecraftVersion, p.ProjectID, p.VersionID)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if p.PlanHash == "" || p.PlanHash != security.Hash(string(store.Marshal(plan))) {
		fail(w, http.StatusConflict, "Состав установки изменился. Откройте предварительный список файлов заново и подтвердите установку")
		return
	}
	j := newJob(s.NodeID, s.ID, "Установка из Modrinth: "+plan.Title)
	cmd := model.Command{
		ID: security.ID(), NodeID: s.NodeID, ServerID: s.ID, JobID: j.ID,
		Type: "extension.install", Payload: store.Marshal(plan),
		Deadline: time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339),
	}
	if err := a.enqueue(r, cmd, j, nil, true, p); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	a.audit(r.Context(), actor(r), "extension.install", s.ID, fmt.Sprintf("Modrinth %s/%s; %d files", plan.ProjectID, plan.VersionID, len(plan.Files)))
	write(w, http.StatusAccepted, j)
}
