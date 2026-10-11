// Package catalog resolves official Minecraft distributions into pinned installation plans.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

type Core struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}
type Version struct {
	ID        string `json:"id"`
	Stable    bool   `json:"stable"`
	Supported bool   `json:"supported"`
	Note      string `json:"note,omitempty"`
}
type Build struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Stable           bool   `json:"stable"`
	Recommended      bool   `json:"recommended"`
	JavaVersion      int    `json:"java_version"`
	InstallerVersion string `json:"installer_version,omitempty"`
}
type ResolveRequest struct {
	Core             string `json:"core"`
	MinecraftVersion string `json:"minecraft_version"`
	LoaderVersion    string `json:"loader_version,omitempty"`
	Build            string `json:"build,omitempty"`
	JavaVersion      int    `json:"java_version,omitempty"`
}

type cacheEntry struct {
	body []byte
	at   time.Time
}
type Catalog struct {
	client    *http.Client
	userAgent string
	mu        sync.RWMutex
	cache     map[string]cacheEntry
	limits    chan struct{}
}

func New(userAgent string) *Catalog {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "MP-Panel/0.1 (https://github.com/come0th3r/mp-panel)"
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || !officialHost(req.URL.Hostname()) {
			return fmt.Errorf("недопустимое перенаправление официального источника")
		}
		return nil
	}}
	return &Catalog{client: client, userAgent: userAgent, cache: make(map[string]cacheEntry), limits: make(chan struct{}, 6)}
}

// Refresh expires entries, retaining last known metadata for temporary upstream outages.
func (c *Catalog) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.cache {
		entry.at = time.Time{}
		c.cache[key] = entry
	}
}

func (c *Catalog) Cores() []Core {
	return []Core{
		{"vanilla", "Vanilla", "Официальный сервер Minecraft без модификаций", "vanilla"},
		{"paper", "Paper", "Сервер с поддержкой Bukkit / Spigot плагинов", "plugins"},
		{"fabric", "Fabric", "Лёгкий загрузчик модов Fabric", "mods"},
		{"forge", "Forge", "Загрузчик модов Minecraft Forge", "mods"},
		{"neoforge", "NeoForge", "Современный загрузчик модов NeoForge", "mods"},
		{"spigot", "Spigot", "Сервер для плагинов; сборка официальным BuildTools", "plugins"},
		{"custom", "Свой JAR", "Загрузка своего серверного JAR и выбор Java", "custom"},
	}
}

func (c *Catalog) fetch(ctx context.Context, target string) ([]byte, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || !officialHost(u.Hostname()) {
		return nil, fmt.Errorf("недопустимый URL источника каталога")
	}
	c.mu.RLock()
	cached, ok := c.cache[target]
	c.mu.RUnlock()
	if ok && time.Since(cached.at) < 15*time.Minute {
		return cached.body, nil
	}
	select {
	case c.limits <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.limits }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json, application/xml, text/plain, text/html")
	resp, err := c.client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("источник %s вернул HTTP %d", u.Hostname(), resp.StatusCode)
		}
	}
	var body []byte
	if err == nil {
		body, err = io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
		if len(body) > 8*1024*1024 {
			err = fmt.Errorf("ответ источника каталога слишком большой")
		}
	}
	if err != nil {
		if ok && len(cached.body) > 0 {
			return cached.body, nil
		}
		return nil, fmt.Errorf("каталог %s недоступен: %w", u.Hostname(), err)
	}
	c.mu.Lock()
	if len(c.cache) >= 256 {
		oldestKey := ""
		oldestTime := time.Now()
		for k, v := range c.cache {
			if v.at.Before(oldestTime) {
				oldestKey, oldestTime = k, v.at
			}
		}
		delete(c.cache, oldestKey)
	}
	c.cache[target] = cacheEntry{body: body, at: time.Now()}
	c.mu.Unlock()
	return body, nil
}
func officialHost(host string) bool {
	switch host {
	case "piston-meta.mojang.com", "piston-data.mojang.com", "launchermeta.mojang.com", "launcher.mojang.com", "fill.papermc.io", "fill-data.papermc.io", "meta.fabricmc.net", "maven.fabricmc.net", "maven.minecraftforge.net", "files.minecraftforge.net", "maven.neoforged.net", "hub.spigotmc.org":
		return true
	}
	return false
}
func (c *Catalog) json(ctx context.Context, target string, out any) error {
	body, err := c.fetch(ctx, target)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("неверный ответ каталога %s: %w", target, err)
	}
	return nil
}
func validID(value string) bool {
	return len(value) > 0 && len(value) <= 100 && safeID.MatchString(value)
}

var numericVersion = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`)
var safeID = regexp.MustCompile(`^[A-Za-z0-9._+ -]+$`)
var digitParts = regexp.MustCompile(`[0-9]+`)
var sha1Digest = regexp.MustCompile(`^[0-9a-f]{40}$`)

func stable(value string) bool { return numericVersion.MatchString(value) }
func numbers(value string) []int {
	parts := digitParts.FindAllString(value, -1)
	out := make([]int, 0, len(parts))
	for _, v := range parts {
		n, _ := strconv.Atoi(v)
		out = append(out, n)
	}
	return out
}
func compare(a, b string) int {
	aa, bb := numbers(a), numbers(b)
	for i := 0; i < len(aa) || i < len(bb); i++ {
		x, y := 0, 0
		if i < len(aa) {
			x = aa[i]
		}
		if i < len(bb) {
			y = bb[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	if stable(a) != stable(b) {
		if stable(a) {
			return 1
		}
		return -1
	}
	return strings.Compare(a, b)
}
func supportedMC(mc string) bool { return stable(mc) && compare(mc, "1.12.2") >= 0 }
func sortedVersions(values map[string]bool) []Version {
	out := []Version{}
	for id, s := range values {
		if !validID(id) {
			continue
		}
		if !s || supportedMC(id) {
			note := ""
			if !s {
				note = "Предварительная версия Minecraft"
			}
			out = append(out, Version{ID: id, Stable: s, Supported: true, Note: note})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stable != out[j].Stable {
			return out[i].Stable
		}
		return compare(out[i].ID, out[j].ID) > 0
	})
	return out
}
func markRecommended(builds []Build) {
	for i := range builds {
		if builds[i].Stable {
			builds[i].Recommended = true
			return
		}
	}
}

const mojangManifest = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"

type manifest struct {
	Versions []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
		Time string `json:"releaseTime"`
	} `json:"versions"`
}
type mojangVersion struct {
	Downloads map[string]struct {
		URL  string `json:"url"`
		SHA1 string `json:"sha1"`
		Size int64  `json:"size"`
	} `json:"downloads"`
	Java struct {
		Major int `json:"majorVersion"`
	} `json:"javaVersion"`
}

func (c *Catalog) minecraft(ctx context.Context, mc string) (mojangVersion, error) {
	var list manifest
	var detail mojangVersion
	if err := c.json(ctx, mojangManifest, &list); err != nil {
		return detail, err
	}
	for _, v := range list.Versions {
		if v.ID == mc {
			if err := c.json(ctx, v.URL, &detail); err != nil {
				return detail, err
			}
			if detail.Java.Major == 0 {
				detail.Java.Major = 8
			}
			return detail, nil
		}
	}
	return detail, fmt.Errorf("версия Minecraft %q отсутствует в официальном каталоге", mc)
}

func (c *Catalog) Versions(ctx context.Context, core string) ([]Version, error) {
	values := map[string]bool{}
	switch core {
	case "custom":
		return []Version{}, nil
	case "vanilla":
		var list manifest
		if err := c.json(ctx, mojangManifest, &list); err != nil {
			return nil, err
		}
		out := []Version{}
		for _, v := range list.Versions {
			if v.Type == "release" && supportedMC(v.ID) {
				out = append(out, Version{ID: v.ID, Stable: true, Supported: true})
			}
		}
		return out, nil
	case "paper":
		var project struct {
			Versions map[string][]string `json:"versions"`
		}
		if err := c.json(ctx, "https://fill.papermc.io/v3/projects/paper", &project); err != nil {
			return nil, err
		}
		for _, group := range project.Versions {
			for _, v := range group {
				values[v] = stable(v)
			}
		}
	case "fabric":
		var list []struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		}
		if err := c.json(ctx, "https://meta.fabricmc.net/v2/versions/game", &list); err != nil {
			return nil, err
		}
		for _, v := range list {
			values[v.Version] = v.Stable
		}
	case "forge", "neoforge":
		list, err := c.maven(ctx, core)
		if err != nil {
			return nil, err
		}
		for _, v := range list {
			mc := minecraftForMaven(core, v)
			if mc != "" {
				values[mc] = stable(mc)
			}
		}
	case "spigot":
		body, err := c.fetch(ctx, "https://hub.spigotmc.org/versions/")
		if err != nil {
			return nil, err
		}
		for _, match := range regexp.MustCompile(`href="([A-Za-z0-9._-]+)\.json"`).FindAllStringSubmatch(string(body), -1) {
			id := match[1]
			if id != "latest" {
				values[id] = stable(id)
			}
		}
	default:
		return nil, fmt.Errorf("неизвестное ядро %q", core)
	}
	out := sortedVersions(values)
	if len(out) == 0 {
		return nil, fmt.Errorf("официальный источник %s не вернул поддерживаемых версий", core)
	}
	return out, nil
}

type paperBuild struct {
	ID        int    `json:"id"`
	Channel   string `json:"channel"`
	Downloads map[string]struct {
		Name      string `json:"name"`
		URL       string `json:"url"`
		Size      int64  `json:"size"`
		Checksums struct {
			SHA256 string `json:"sha256"`
		} `json:"checksums"`
	} `json:"downloads"`
}

func (c *Catalog) paper(ctx context.Context, mc string) ([]paperBuild, error) {
	var out []paperBuild
	err := c.json(ctx, "https://fill.papermc.io/v3/projects/paper/versions/"+url.PathEscape(mc)+"/builds", &out)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, err
}
func paperJava(mc string, base int) int {
	if !stable(mc) {
		return base
	}
	switch {
	case compare(mc, "26.1") >= 0:
		return max(base, 25)
	case compare(mc, "1.20") >= 0:
		return max(base, 21)
	case compare(mc, "1.17") >= 0:
		return max(base, 17)
	case mc == "1.16.5":
		return max(base, 16)
	default:
		return max(base, 11)
	}
}
func (c *Catalog) fabricInstaller(ctx context.Context) (string, error) {
	var list []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := c.json(ctx, "https://meta.fabricmc.net/v2/versions/installer", &list); err != nil {
		return "", err
	}
	for _, v := range list {
		if v.Stable {
			return v.Version, nil
		}
	}
	return "", fmt.Errorf("официальный каталог Fabric не содержит стабильного установщика")
}
func (c *Catalog) maven(ctx context.Context, core string) ([]string, error) {
	target := "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml"
	if core == "neoforge" {
		target = "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml"
	}
	body, err := c.fetch(ctx, target)
	if err != nil {
		return nil, err
	}
	var meta struct {
		Versions []string `xml:"versioning>versions>version"`
	}
	if err = xml.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("ошибка формата Maven: %w", err)
	}
	if len(meta.Versions) == 0 {
		return nil, fmt.Errorf("пустой Maven каталог %s", core)
	}
	return meta.Versions, nil
}

// NeoForge changed numbering in 26.1; reject unknown schemes rather than guess mappings.
func minecraftForMaven(core, version string) string {
	if core == "forge" {
		mc, _, ok := strings.Cut(version, "-")
		if ok && supportedMC(mc) {
			return mc
		}
		return ""
	}
	base := strings.Split(version, "-")[0]
	p := strings.Split(base, ".")
	if !numericVersion.MatchString(base) {
		return ""
	}
	n := numbers(base)
	if len(p) == 3 && n[0] >= 20 && n[0] <= 21 {
		mc := fmt.Sprintf("1.%d", n[0])
		if n[1] > 0 {
			mc += fmt.Sprintf(".%d", n[1])
		}
		if mc == "1.20" || mc == "1.20.1" {
			return ""
		}
		return mc
	}
	if len(p) == 4 && n[0] >= 26 && n[0] <= 99 {
		mc := fmt.Sprintf("%d.%d", n[0], n[1])
		if n[2] > 0 {
			mc += fmt.Sprintf(".%d", n[2])
		}
		return mc
	}
	return ""
}

type spigotRevision struct {
	Name string `json:"name"`
	Java []int  `json:"javaVersions"`
}

func (c *Catalog) spigot(ctx context.Context, mc string) (spigotRevision, error) {
	var r spigotRevision
	err := c.json(ctx, "https://hub.spigotmc.org/versions/"+url.PathEscape(mc)+".json", &r)
	if err == nil && r.Name == "" {
		err = fmt.Errorf("неверная ревизия Spigot")
	}
	return r, err
}

func (c *Catalog) Builds(ctx context.Context, core, mc string) ([]Build, error) {
	if core == "custom" {
		return []Build{}, nil
	}
	if !validID(mc) {
		return nil, fmt.Errorf("неверная версия Minecraft")
	}
	versions, err := c.Versions(ctx, core)
	if err != nil {
		return nil, err
	}
	found := false
	for _, v := range versions {
		if v.ID == mc && v.Supported {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%s не поддерживает Minecraft %s", core, mc)
	}
	meta, err := c.minecraft(ctx, mc)
	if err != nil {
		return nil, err
	}
	java := meta.Java.Major
	out := []Build{}
	switch core {
	case "vanilla":
		if meta.Downloads["server"].URL == "" {
			return nil, fmt.Errorf("у Minecraft %s нет серверного JAR", mc)
		}
		out = append(out, Build{ID: mc, Name: mc, Stable: true, Recommended: true, JavaVersion: java})
	case "paper":
		list, err := c.paper(ctx, mc)
		if err != nil {
			return nil, err
		}
		for _, b := range list {
			if b.Downloads["server:default"].URL != "" {
				out = append(out, Build{ID: strconv.Itoa(b.ID), Name: fmt.Sprintf("Build %d · %s", b.ID, b.Channel), Stable: b.Channel == "STABLE", JavaVersion: paperJava(mc, java)})
			}
		}
		markRecommended(out)
	case "fabric":
		installer, err := c.fabricInstaller(ctx)
		if err != nil {
			return nil, err
		}
		var list []struct {
			Loader struct {
				Version string `json:"version"`
				Stable  bool   `json:"stable"`
			} `json:"loader"`
		}
		if err = c.json(ctx, "https://meta.fabricmc.net/v2/versions/loader/"+url.PathEscape(mc), &list); err != nil {
			return nil, err
		}
		for _, v := range list {
			// The executable launcher endpoint requires Fabric Loader 0.12.5+.
			if compare(v.Loader.Version, "0.12.5") < 0 {
				continue
			}
			out = append(out, Build{ID: v.Loader.Version, Name: v.Loader.Version, Stable: v.Loader.Stable, JavaVersion: java, InstallerVersion: installer})
		}
		markRecommended(out)
	case "forge", "neoforge":
		list, err := c.maven(ctx, core)
		if err != nil {
			return nil, err
		}
		sort.Slice(list, func(i, j int) bool { return compare(list[i], list[j]) > 0 })
		recommended := ""
		if core == "forge" {
			var promo struct {
				Promos map[string]string `json:"promos"`
			}
			if err = c.json(ctx, "https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json", &promo); err != nil {
				return nil, err
			}
			recommended = promo.Promos[mc+"-recommended"]
		}
		for _, v := range list {
			if minecraftForMaven(core, v) != mc {
				continue
			}
			id := v
			if core == "forge" {
				id = strings.TrimPrefix(v, mc+"-")
			}
			out = append(out, Build{ID: id, Name: id, Stable: !strings.Contains(strings.ToLower(v), "beta") && !strings.Contains(strings.ToLower(v), "alpha"), Recommended: core == "forge" && id == recommended, JavaVersion: java})
		}
		if core == "neoforge" {
			markRecommended(out)
		}
	case "spigot":
		r, err := c.spigot(ctx, mc)
		if err != nil {
			return nil, err
		}
		if len(r.Java) > 0 {
			java = max(java, r.Java[0]-44)
		}
		out = append(out, Build{ID: r.Name, Name: "Revision " + r.Name, Stable: stable(mc), Recommended: stable(mc), JavaVersion: java})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("нет доступных сборок %s для Minecraft %s", core, mc)
	}
	return out, nil
}

func (c *Catalog) Resolve(ctx context.Context, req ResolveRequest) (model.InstallSpec, error) {
	spec := model.InstallSpec{Core: req.Core, MinecraftVersion: req.MinecraftVersion, InstallMode: "direct", LaunchJar: "server.jar", Artifacts: []model.Artifact{}}
	if req.Core == "custom" {
		switch req.JavaVersion {
		case 8, 11, 16, 17, 21, 25:
			spec.JavaVersion = req.JavaVersion
			return spec, nil
		default:
			return spec, fmt.Errorf("для своего JAR выберите Java 8, 11, 16, 17, 21 или 25")
		}
	}
	builds, err := c.Builds(ctx, req.Core, req.MinecraftVersion)
	if err != nil {
		return spec, err
	}
	requested := req.Build
	if req.LoaderVersion != "" {
		if requested != "" && requested != req.LoaderVersion {
			return spec, fmt.Errorf("loader_version и build должны обозначать одну сборку")
		}
		requested = req.LoaderVersion
	}
	var choice *Build
	for i := range builds {
		if requested != "" {
			if builds[i].ID == requested {
				choice = &builds[i]
				break
			}
		} else if builds[i].Recommended {
			choice = &builds[i]
			break
		}
	}
	if choice == nil {
		if requested != "" {
			return spec, fmt.Errorf("сборка %q отсутствует в официальном каталоге", requested)
		}
		return spec, fmt.Errorf("рекомендованная сборка отсутствует; выберите доступную сборку явно")
	}
	spec.Build = choice.ID
	spec.JavaVersion = choice.JavaVersion
	spec.InstallerVersion = choice.InstallerVersion
	if req.JavaVersion != 0 && req.JavaVersion != choice.JavaVersion {
		return spec, fmt.Errorf("эта сборка использует Java %d; выбранная Java %d не подтверждена профилем", choice.JavaVersion, req.JavaVersion)
	}
	switch req.Core {
	case "vanilla":
		meta, err := c.minecraft(ctx, req.MinecraftVersion)
		if err != nil {
			return spec, err
		}
		d := meta.Downloads["server"]
		spec.Artifacts = append(spec.Artifacts, model.Artifact{Name: "server.jar", URL: d.URL, SHA1: d.SHA1, Size: d.Size, Target: "server.jar"})
	case "paper":
		list, err := c.paper(ctx, req.MinecraftVersion)
		if err != nil {
			return spec, err
		}
		for _, b := range list {
			if strconv.Itoa(b.ID) == choice.ID {
				d := b.Downloads["server:default"]
				spec.Artifacts = append(spec.Artifacts, model.Artifact{Name: d.Name, URL: d.URL, SHA256: d.Checksums.SHA256, Size: d.Size, Target: "server.jar"})
				break
			}
		}
	case "fabric":
		spec.LoaderVersion = choice.ID
		spec.LaunchJar = "fabric-server-launch.jar"
		target := "https://meta.fabricmc.net/v2/versions/loader/" + url.PathEscape(req.MinecraftVersion) + "/" + url.PathEscape(choice.ID) + "/" + url.PathEscape(choice.InstallerVersion) + "/server/jar"
		spec.Artifacts = append(spec.Artifacts, model.Artifact{Name: spec.LaunchJar, URL: target, Target: spec.LaunchJar})
	case "forge", "neoforge":
		spec.LoaderVersion = choice.ID
		spec.InstallMode = req.Core
		spec.LaunchJar = ""
		version := choice.ID
		base := "https://maven.neoforged.net/releases/net/neoforged/neoforge/"
		prefix := "neoforge-"
		if req.Core == "forge" {
			version = req.MinecraftVersion + "-" + choice.ID
			base = "https://maven.minecraftforge.net/net/minecraftforge/forge/"
			prefix = "forge-"
		}
		target := base + url.PathEscape(version) + "/" + prefix + url.PathEscape(version) + "-installer.jar"
		digest, err := c.checksum(ctx, target, "sha1")
		if err != nil {
			return spec, err
		}
		spec.Artifacts = append(spec.Artifacts, model.Artifact{Name: "installer.jar", URL: target, SHA1: digest, Target: "installer.jar"})
	case "spigot":
		spec.InstallMode = "spigot"
		var job struct {
			Build struct {
				Number int `json:"number"`
			} `json:"lastSuccessfulBuild"`
		}
		if err := c.json(ctx, "https://hub.spigotmc.org/jenkins/job/BuildTools/api/json?tree=lastSuccessfulBuild%5Bnumber%5D", &job); err != nil {
			return spec, err
		}
		if job.Build.Number <= 0 {
			return spec, fmt.Errorf("BuildTools не опубликовал успешную сборку")
		}
		spec.InstallerVersion = strconv.Itoa(job.Build.Number)
		target := fmt.Sprintf("https://hub.spigotmc.org/jenkins/job/BuildTools/%d/artifact/target/BuildTools.jar", job.Build.Number)
		spec.Artifacts = append(spec.Artifacts, model.Artifact{Name: "BuildTools.jar", URL: target, Target: "BuildTools.jar"})
	}
	// Generated Fabric launchers and Jenkins artifacts have no checksum manifest;
	// compute one now so installation cannot silently consume a different binary.
	if req.Core == "fabric" || req.Core == "spigot" {
		for i := range spec.Artifacts {
			digest, size, err := c.pinArtifact(ctx, spec.Artifacts[i].URL)
			if err != nil {
				return spec, err
			}
			spec.Artifacts[i].SHA256, spec.Artifacts[i].Size = digest, size
		}
	}
	if len(spec.Artifacts) == 0 {
		return spec, fmt.Errorf("не удалось зафиксировать артефакт установки")
	}
	return spec, nil
}

func (c *Catalog) pinArtifact(ctx context.Context, target string) (string, int64, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || !officialHost(u.Hostname()) {
		return "", 0, fmt.Errorf("недопустимый URL артефакта")
	}
	select {
	case c.limits <- struct{}{}:
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	defer func() { <-c.limits }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("не удалось зафиксировать артефакт: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("артефакт вернул HTTP %d", resp.StatusCode)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(resp.Body, 32*1024*1024+1))
	if err != nil {
		return "", 0, err
	}
	if size == 0 || size > 32*1024*1024 {
		return "", 0, fmt.Errorf("неверный размер установщика")
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), size, nil
}
func (c *Catalog) checksum(ctx context.Context, target, kind string) (string, error) {
	body, err := c.fetch(ctx, target+"."+kind)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("пустая контрольная сумма %s", target)
	}
	digest := strings.ToLower(fields[0])
	if !sha1Digest.MatchString(digest) {
		return "", fmt.Errorf("неверная контрольная сумма %s", target)
	}
	return digest, nil
}
