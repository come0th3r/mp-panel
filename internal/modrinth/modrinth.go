// Package modrinth builds pinned, server-compatible installation plans from the official API.
package modrinth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const apiBase = "https://api.modrinth.com/v2"
const responseLimit = 8 << 20
const cacheLimit = 128
const cacheTTL = 3 * time.Minute
const maxProjects = 64

type Project struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	IconURL     string   `json:"icon_url"`
	Author      string   `json:"author"`
	Downloads   int64    `json:"downloads"`
	Categories  []string `json:"categories"`
	ProjectType string   `json:"project_type"`
	URL         string   `json:"url"`
}

type SearchResult struct {
	Hits      []Project `json:"hits"`
	TotalHits int       `json:"total_hits"`
	Offset    int       `json:"offset"`
	Limit     int       `json:"limit"`
}

type Version struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	Name          string `json:"name"`
	VersionNumber string `json:"version_number"`
	VersionType   string `json:"version_type"`
	DatePublished string `json:"date_published"`
}

type PlanFile struct {
	ProjectID string `json:"project_id"`
	VersionID string `json:"version_id"`
	Title     string `json:"title"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	SHA1      string `json:"sha1"`
	SHA512    string `json:"sha512"`
	Required  bool   `json:"required"`
}

type Plan struct {
	ProjectID        string     `json:"project_id"`
	VersionID        string     `json:"version_id"`
	Title            string     `json:"title"`
	Core             string     `json:"core"`
	MinecraftVersion string     `json:"minecraft_version"`
	Files            []PlanFile `json:"files"`
	Warnings         []string   `json:"warnings"`
}

type cacheEntry struct {
	body []byte
	at   time.Time
}

type Catalog struct {
	client       *http.Client
	userAgent    string
	mu           sync.Mutex
	cache        map[string]cacheEntry
	cacheBytes   int
	blockedUntil time.Time
	slots        chan struct{}
}

func New(userAgent string) *Catalog {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "come0th3r/mp-panel (https://github.com/come0th3r/mp-panel)"
	}
	return &Catalog{
		client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 2 || req.URL.Scheme != "https" || req.URL.Host != "api.modrinth.com" {
				return fmt.Errorf("Modrinth вернул недопустимое перенаправление")
			}
			return nil
		}},
		userAgent: userAgent, cache: make(map[string]cacheEntry), slots: make(chan struct{}, 3),
	}
}

func (c *Catalog) fetch(ctx context.Context, endpoint string, result any) error {
	target := apiBase + endpoint
	c.mu.Lock()
	entry, ok := c.cache[target]
	blocked := c.blockedUntil
	c.mu.Unlock()
	if ok && time.Since(entry.at) < cacheTTL {
		return json.Unmarshal(entry.body, result)
	}
	if time.Now().Before(blocked) {
		return fmt.Errorf("Modrinth временно ограничил запросы; повторите через %d с", int(time.Until(blocked).Seconds())+1)
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к Modrinth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := 60 * time.Second
		if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > 0 {
			delay = time.Duration(min(seconds, 3600)) * time.Second
		}
		c.mu.Lock()
		c.blockedUntil = time.Now().Add(delay)
		c.mu.Unlock()
		return fmt.Errorf("Modrinth временно ограничил запросы; повторите через %d с", int(delay.Seconds()))
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("проект или версия Modrinth не найдены")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Modrinth недоступен: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return fmt.Errorf("не удалось прочитать ответ Modrinth: %w", err)
	}
	if len(body) > responseLimit {
		return fmt.Errorf("ответ Modrinth превышает допустимый размер")
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("Modrinth вернул некорректные данные: %w", err)
	}
	c.mu.Lock()
	if old, exists := c.cache[target]; exists {
		c.cacheBytes -= len(old.body)
		delete(c.cache, target)
	}
	for len(c.cache) >= cacheLimit || c.cacheBytes+len(body) > 32<<20 {
		var oldest string
		var at time.Time
		for key, item := range c.cache {
			if oldest == "" || item.at.Before(at) {
				oldest, at = key, item.at
			}
		}
		if oldest == "" {
			break
		}
		c.cacheBytes -= len(c.cache[oldest].body)
		delete(c.cache, oldest)
	}
	c.cache[target] = cacheEntry{body: body, at: time.Now()}
	c.cacheBytes += len(body)
	c.mu.Unlock()
	return nil
}

func loaders(core string) ([]string, error) {
	switch core {
	case "fabric", "forge", "neoforge":
		return []string{core}, nil
	case "paper":
		return []string{"paper", "spigot", "bukkit"}, nil
	case "spigot":
		return []string{"spigot", "bukkit"}, nil
	default:
		return nil, fmt.Errorf("каталог Modrinth доступен для Fabric, Forge, NeoForge, Paper и Spigot")
	}
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
func kind(core string) string {
	if core == "paper" || core == "spigot" {
		return "plugin"
	}
	return "mod"
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_!@()+.,'\-]{1,128}$`)
var gameVersion = regexp.MustCompile(`^[A-Za-z0-9_.+\-]{1,64}$`)
var sha1Pattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var sha512Pattern = regexp.MustCompile(`^[a-fA-F0-9]{128}$`)

func validateProfile(core, mc string) ([]string, error) {
	allowed, err := loaders(core)
	if err != nil {
		return nil, err
	}
	if !gameVersion.MatchString(mc) {
		return nil, fmt.Errorf("укажите корректную версию Minecraft")
	}
	return allowed, nil
}

func (c *Catalog) Search(ctx context.Context, core, mcVersion, query, sortBy string, offset int) (SearchResult, error) {
	allowed, err := validateProfile(core, mcVersion)
	if err != nil {
		return SearchResult{}, err
	}
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > 200 || offset < 0 || offset > 10000 {
		return SearchResult{}, fmt.Errorf("некорректные параметры поиска Modrinth")
	}
	if sortBy == "" {
		if strings.TrimSpace(query) == "" {
			sortBy = "downloads"
		} else {
			sortBy = "relevance"
		}
	}
	if !contains([]string{"relevance", "downloads", "follows", "newest", "updated"}, sortBy) {
		return SearchResult{}, fmt.Errorf("неизвестная сортировка Modrinth")
	}
	loaderFacets := make([]string, 0, len(allowed))
	for _, loader := range allowed {
		loaderFacets = append(loaderFacets, "categories:"+loader)
	}
	facets := [][]string{{"all_project_types:" + kind(core)}, loaderFacets, {"versions:" + mcVersion}, {"server_side:required", "server_side:optional"}}
	params := url.Values{"query": {strings.TrimSpace(query)}, "index": {sortBy}, "offset": {strconv.Itoa(offset)}, "limit": {"20"}, "facets": {jsonString(facets)}}
	var raw struct {
		Hits []struct {
			Project
			ProjectID  string `json:"project_id"`
			ServerSide string `json:"server_side"`
		} `json:"hits"`
		TotalHits int `json:"total_hits"`
		Offset    int `json:"offset"`
		Limit     int `json:"limit"`
	}
	if err := c.fetch(ctx, "/search?"+params.Encode(), &raw); err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{Hits: []Project{}, TotalHits: raw.TotalHits, Offset: raw.Offset, Limit: raw.Limit}
	for _, hit := range raw.Hits {
		if !identifier.MatchString(hit.ProjectID) || (hit.ServerSide != "required" && hit.ServerSide != "optional") {
			continue
		}
		p := hit.Project
		p.ID = hit.ProjectID
		p.ProjectType = kind(core)
		p.URL = "https://modrinth.com/project/" + url.PathEscape(p.ID)
		if !safeImageURL(p.IconURL) {
			p.IconURL = ""
		}
		if p.Categories == nil {
			p.Categories = []string{}
		}
		result.Hits = append(result.Hits, p)
	}
	return result, nil
}

type projectData struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ServerSide string `json:"server_side"`
	ClientSide string `json:"client_side"`
}
type dependency struct {
	ProjectID string `json:"project_id"`
	VersionID string `json:"version_id"`
	FileName  string `json:"file_name"`
	Type      string `json:"dependency_type"`
}
type fileData struct {
	Name     string            `json:"filename"`
	URL      string            `json:"url"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
	Hashes   map[string]string `json:"hashes"`
	FileType string            `json:"file_type"`
}
type versionData struct {
	Version
	GameVersions []string     `json:"game_versions"`
	Loaders      []string     `json:"loaders"`
	Environment  string       `json:"environment"`
	Status       string       `json:"status"`
	Files        []fileData   `json:"files"`
	Dependencies []dependency `json:"dependencies"`
}

func (c *Catalog) project(ctx context.Context, id string) (projectData, error) {
	var p projectData
	if !identifier.MatchString(id) {
		return p, fmt.Errorf("некорректный идентификатор проекта Modrinth")
	}
	if err := c.fetch(ctx, "/project/"+url.PathEscape(id), &p); err != nil {
		return p, err
	}
	if !identifier.MatchString(p.ID) {
		return p, fmt.Errorf("Modrinth вернул некорректный проект")
	}
	return p, nil
}

func serverCompatible(v versionData, p projectData) bool {
	switch v.Environment {
	case "client_and_server", "server_only", "server_only_client_optional", "dedicated_server_only", "client_or_server", "client_or_server_prefers_both", "client_only_server_optional":
		return true
	case "", "unknown":
		return p.ServerSide == "required" || p.ServerSide == "optional"
	default:
		return false
	}
}

func compatible(v versionData, p projectData, allowed []string, mc string) bool {
	if v.ProjectID != p.ID || !identifier.MatchString(v.ID) || !contains(v.GameVersions, mc) || !serverCompatible(v, p) {
		return false
	}
	if v.Status != "" && v.Status != "listed" && v.Status != "archived" && v.Status != "unlisted" {
		return false
	}
	if !contains([]string{"release", "beta", "alpha"}, v.VersionType) {
		return false
	}
	for _, loader := range allowed {
		if contains(v.Loaders, loader) {
			return true
		}
	}
	return false
}

func safeImageURL(target string) bool {
	u, err := url.Parse(target)
	return err == nil && u.Scheme == "https" && u.Host == "cdn.modrinth.com" && u.User == nil
}

func validFile(f fileData) bool {
	if f.Name == "" || len(f.Name) > 255 || !strings.HasSuffix(strings.ToLower(f.Name), ".jar") || f.Name == "." || strings.ContainsAny(f.Name, "/\\:\x00\r\n") || strings.HasPrefix(f.Name, ".") || strings.TrimSpace(f.Name) != f.Name {
		return false
	}
	for _, r := range f.Name {
		if r < 32 || r == 127 {
			return false
		}
	}
	u, err := url.Parse(f.URL)
	if err != nil || u.Scheme != "https" || u.Host != "cdn.modrinth.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/data/") || path.Base(u.Path) != f.Name {
		return false
	}
	return f.Size > 0 && sha1Pattern.MatchString(f.Hashes["sha1"]) && sha512Pattern.MatchString(f.Hashes["sha512"]) && f.FileType == ""
}

func primaryFile(v versionData) (fileData, error) {
	var candidates []fileData
	var primary []fileData
	for _, f := range v.Files {
		if f.Primary && !validFile(f) {
			return fileData{}, fmt.Errorf("основной файл версии %s не является проверяемым серверным JAR", v.VersionNumber)
		}
		if validFile(f) {
			candidates = append(candidates, f)
			if f.Primary {
				primary = append(primary, f)
			}
		}
	}
	if len(primary) == 1 {
		return primary[0], nil
	}
	if len(primary) == 0 && len(candidates) == 1 {
		return candidates[0], nil
	}
	return fileData{}, fmt.Errorf("для версии %s нет единственного проверяемого серверного JAR", v.VersionNumber)
}

func (c *Catalog) versions(ctx context.Context, core, mc string, p projectData) ([]versionData, error) {
	allowed, err := validateProfile(core, mc)
	if err != nil {
		return nil, err
	}
	params := url.Values{"loaders": {jsonString(allowed)}, "game_versions": {jsonString([]string{mc})}, "include_changelog": {"false"}}
	var raw []versionData
	if err := c.fetch(ctx, "/project/"+url.PathEscape(p.ID)+"/version?"+params.Encode(), &raw); err != nil {
		return nil, err
	}
	result := []versionData{}
	for _, v := range raw {
		if compatible(v, p, allowed, mc) {
			if _, err := primaryFile(v); err == nil {
				result = append(result, v)
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].DatePublished > result[j].DatePublished })
	return result, nil
}

func (c *Catalog) Versions(ctx context.Context, core, mcVersion, project string) ([]Version, error) {
	if _, err := validateProfile(core, mcVersion); err != nil {
		return nil, err
	}
	p, err := c.project(ctx, project)
	if err != nil {
		return nil, err
	}
	versions, err := c.versions(ctx, core, mcVersion, p)
	if err != nil {
		return nil, err
	}
	result := []Version{}
	for _, v := range versions {
		result = append(result, v.Version)
	}
	return result, nil
}

type resolver struct {
	c        *Catalog
	ctx      context.Context
	core, mc string
	allowed  []string
	channel  string
	projects map[string]projectData
	chosen   map[string]versionData
	active   map[string]bool
	order    []string
	warnings []string
}

func (r *resolver) project(id string) (projectData, error) {
	if p, ok := r.projects[id]; ok {
		return p, nil
	}
	p, err := r.c.project(r.ctx, id)
	if err != nil {
		return p, err
	}
	r.projects[id], r.projects[p.ID] = p, p
	return p, nil
}

func (r *resolver) version(projectID, versionID string) (versionData, projectData, error) {
	var v versionData
	var p projectData
	var err error
	if versionID != "" {
		if !identifier.MatchString(versionID) {
			return v, p, fmt.Errorf("некорректный идентификатор версии Modrinth")
		}
		if err = r.c.fetch(r.ctx, "/version/"+url.PathEscape(versionID), &v); err != nil {
			return v, p, err
		}
		if v.ID != versionID {
			return v, p, fmt.Errorf("Modrinth вернул другой идентификатор версии")
		}
		p, err = r.project(v.ProjectID)
		if err != nil {
			return v, p, err
		}
		if projectID != "" {
			expected, e := r.project(projectID)
			if e != nil {
				return v, p, e
			}
			if expected.ID != p.ID {
				return v, p, fmt.Errorf("версия Modrinth относится к другому проекту")
			}
		}
	} else {
		p, err = r.project(projectID)
		if err != nil {
			return v, p, err
		}
		if existing, ok := r.chosen[p.ID]; ok {
			return existing, p, nil
		}
		versions, e := r.c.versions(r.ctx, r.core, r.mc, p)
		if e != nil {
			return v, p, e
		}
		for _, channel := range []string{"release", "beta", "alpha"} {
			if channel == "beta" && r.channel != "beta" && r.channel != "alpha" {
				continue
			}
			if channel == "alpha" && r.channel != "alpha" {
				continue
			}
			for _, candidate := range versions {
				if candidate.VersionType == channel {
					v = candidate
					break
				}
			}
			if v.ID != "" {
				break
			}
		}
		if v.ID == "" {
			return v, p, fmt.Errorf("для обязательной зависимости «%s» нет совместимой версии подходящего канала", p.Title)
		}
	}
	if !compatible(v, p, r.allowed, r.mc) {
		return v, p, fmt.Errorf("«%s», версия %s, не подходит для сервера %s / Minecraft %s", p.Title, v.VersionNumber, r.core, r.mc)
	}
	if _, err := primaryFile(v); err != nil {
		return v, p, err
	}
	return v, p, nil
}

func (r *resolver) visit(projectID, versionID string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("слишком глубокая цепочка зависимостей Modrinth")
	}
	v, p, err := r.version(projectID, versionID)
	if err != nil {
		return err
	}
	if r.active[p.ID] {
		return fmt.Errorf("циклическая зависимость Modrinth: «%s»", p.Title)
	}
	if old, ok := r.chosen[p.ID]; ok {
		if versionID != "" && old.ID != v.ID {
			return fmt.Errorf("обязательные зависимости требуют разные версии «%s»", p.Title)
		}
		return nil
	}
	if len(r.chosen) >= maxProjects {
		return fmt.Errorf("план Modrinth содержит слишком много зависимостей")
	}
	r.chosen[p.ID] = v
	r.active[p.ID] = true
	if v.Environment == "client_and_server" || v.Environment == "client_only_server_optional" || (v.Environment == "unknown" || v.Environment == "") && p.ClientSide == "required" {
		r.warnings = append(r.warnings, fmt.Sprintf("«%s» также нужно установить игрокам на клиент.", p.Title))
	}
	if v.VersionType != "release" {
		r.warnings = append(r.warnings, fmt.Sprintf("«%s»: выбрана предварительная версия (%s).", p.Title, v.VersionType))
	}
	// Exact requirements take precedence over a project's automatically selected latest release.
	dependencies := append([]dependency(nil), v.Dependencies...)
	sort.SliceStable(dependencies, func(i, j int) bool {
		return dependencies[i].Type == "required" && dependencies[i].VersionID != "" && !(dependencies[j].Type == "required" && dependencies[j].VersionID != "")
	})
	for _, d := range dependencies {
		switch d.Type {
		case "required":
			if d.ProjectID == "" && d.VersionID == "" {
				return fmt.Errorf("«%s» требует внешний файл %s; автоматическая установка невозможна", p.Title, d.FileName)
			}
			if err := r.visit(d.ProjectID, d.VersionID, depth+1); err != nil {
				return fmt.Errorf("зависимость «%s»: %w", p.Title, err)
			}
		case "optional":
			r.warnings = append(r.warnings, fmt.Sprintf("Необязательная зависимость «%s» (%s) автоматически не устанавливается.", p.Title, first(d.ProjectID, d.VersionID, d.FileName)))
		case "embedded", "incompatible":
		default:
			return fmt.Errorf("неизвестный тип зависимости Modrinth у «%s»", p.Title)
		}
	}
	r.active[p.ID] = false
	r.order = append(r.order, p.ID)
	return nil
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "без идентификатора"
}

func (c *Catalog) Resolve(ctx context.Context, core, mcVersion, project, version string) (Plan, error) {
	allowed, err := validateProfile(core, mcVersion)
	if err != nil {
		return Plan{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	r := resolver{c: c, ctx: ctx, core: core, mc: mcVersion, allowed: allowed, channel: "release", projects: map[string]projectData{}, chosen: map[string]versionData{}, active: map[string]bool{}, warnings: []string{}}
	main, p, err := r.version(project, version)
	if err != nil {
		return Plan{}, err
	}
	r.channel = main.VersionType
	if err := r.visit(p.ID, main.ID, 0); err != nil {
		return Plan{}, err
	}
	for id, v := range r.chosen {
		for _, d := range v.Dependencies {
			if d.Type != "incompatible" {
				continue
			}
			for selectedID, selected := range r.chosen {
				file, _ := primaryFile(selected)
				if (d.VersionID != "" && d.VersionID == selected.ID) || (d.VersionID == "" && d.ProjectID != "" && d.ProjectID == selectedID) || (d.VersionID == "" && d.ProjectID == "" && d.FileName != "" && strings.EqualFold(d.FileName, file.Name)) {
					return Plan{}, fmt.Errorf("«%s» несовместим с «%s» в выбранном плане", r.projects[id].Title, r.projects[selectedID].Title)
				}
			}
		}
	}
	plan := Plan{ProjectID: p.ID, VersionID: main.ID, Title: p.Title, Core: core, MinecraftVersion: mcVersion, Files: []PlanFile{}, Warnings: r.warnings}
	names := map[string]bool{}
	for _, id := range r.order {
		v := r.chosen[id]
		f, err := primaryFile(v)
		if err != nil {
			return Plan{}, err
		}
		if names[strings.ToLower(f.Name)] {
			return Plan{}, fmt.Errorf("разные проекты Modrinth используют одинаковое имя файла %s", f.Name)
		}
		names[strings.ToLower(f.Name)] = true
		plan.Files = append(plan.Files, PlanFile{ProjectID: id, VersionID: v.ID, Title: r.projects[id].Title, Name: f.Name, URL: f.URL, Size: f.Size, SHA1: strings.ToLower(f.Hashes["sha1"]), SHA512: strings.ToLower(f.Hashes["sha512"]), Required: id != p.ID})
	}
	return plan, nil
}
