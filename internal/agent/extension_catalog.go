package agent

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

type extensionInstallFile struct {
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

type extensionInstallPayload struct {
	Core             string                 `json:"core"`
	MinecraftVersion string                 `json:"minecraft_version"`
	Files            []extensionInstallFile `json:"files"`
}

type installedExtension struct {
	VersionID string `json:"version_id"`
	Path      string `json:"path"`
	SHA1      string `json:"sha1"`
}

func extensionFileMatches(ctx context.Context, r *os.Root, name string, file extensionInstallFile) (bool, error) {
	info, err := r.Lstat(name)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != file.Size {
		return false, nil
	}
	in, err := r.Open(name)
	if err != nil {
		return false, err
	}
	h, strong := sha1.New(), sha512.New()
	_, err = io.Copy(io.MultiWriter(h, strong), contextReader{ctx: ctx, r: in})
	if closeErr := in.Close(); err == nil {
		err = closeErr
	}
	return err == nil && strings.EqualFold(hex.EncodeToString(h.Sum(nil)), file.SHA1) &&
		strings.EqualFold(hex.EncodeToString(strong.Sum(nil)), file.SHA512), err
}

func readExtensionManifest(name string) (map[string]installedExtension, error) {
	manifest := map[string]installedExtension{}
	f, err := os.Open(name)
	if os.IsNotExist(err) {
		return manifest, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("installed Modrinth manifest exceeds 4 MiB")
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	if manifest == nil {
		manifest = map[string]installedExtension{}
	}
	return manifest, nil
}

func modrinthDownloadURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil &&
		strings.EqualFold(u.Hostname(), "cdn.modrinth.com") &&
		(u.Port() == "" || u.Port() == "443") && u.Fragment == "" && strings.HasPrefix(u.Path, "/data/")
}

func (a *Agent) installExtensions(ctx context.Context, serverID, dir string, p extensionInstallPayload, logs *[]string) (any, error) {
	spec, err := a.readSpec(serverID)
	if err != nil {
		return nil, err
	}
	if p.Core != spec.Server.Core || p.MinecraftVersion != spec.Server.MinecraftVersion {
		return nil, errors.New("server profile changed; refresh the catalog installation plan")
	}
	destination := ""
	switch p.Core {
	case "fabric", "forge", "neoforge":
		destination = "mods"
	case "paper", "spigot":
		destination = "plugins"
	default:
		return nil, errors.New("this server core does not support catalog mods or plugins")
	}
	if len(p.Files) < 1 || len(p.Files) > 128 {
		return nil, errors.New("catalog installation must contain between 1 and 128 files")
	}
	if err = a.requireStopped(ctx, serverID); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	manifestDir := filepath.Join(a.cfg.DataDir, "specs", "modrinth")
	if err = os.MkdirAll(manifestDir, 0700); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(manifestDir, serverID+".json")
	manifest, err := readExtensionManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read installed Modrinth projects: %w", err)
	}
	names, projects := map[string]bool{}, map[string]bool{}
	install := make([]extensionInstallFile, 0, len(p.Files))
	skipped := []string{}
	for _, file := range p.Files {
		name, e := operationPath(file.Name, false)
		u, urlErr := url.Parse(file.URL)
		checksum, checksumErr := hex.DecodeString(file.SHA1)
		strongChecksum, strongErr := hex.DecodeString(file.SHA512)
		if e != nil || strings.Contains(name, "/") || !strings.EqualFold(path.Ext(name), ".jar") ||
			file.ProjectID == "" || file.VersionID == "" || file.Size <= 0 || file.Size > 2<<30 ||
			urlErr != nil || !modrinthDownloadURL(u) || checksumErr != nil || len(checksum) != sha1.Size || strongErr != nil || len(strongChecksum) != sha512.Size {
			return nil, errors.New("catalog file requires a safe JAR name, Modrinth CDN URL, positive size and valid SHA1/SHA512")
		}
		if names[name] || projects[file.ProjectID] {
			return nil, errors.New("catalog installation contains duplicate filenames or projects")
		}
		names[name], projects[file.ProjectID] = true, true
		target := path.Join(destination, name)
		if previous, exists := manifest[file.ProjectID]; exists {
			previousPath, e := operationPath(previous.Path, false)
			if e != nil || (path.Dir(previousPath) != "mods" && path.Dir(previousPath) != "plugins") || !strings.EqualFold(path.Ext(previousPath), ".jar") {
				return nil, errors.New("invalid installed Modrinth project record")
			}
			guard, _, e := openJail(dir, previousPath)
			if e != nil {
				return nil, e
			}
			guard.Close()
			_, e = r.Lstat(previousPath)
			if os.IsNotExist(e) {
				previousPath += ".disabled"
				guard, _, e = openJail(dir, previousPath)
				if e != nil {
					return nil, e
				}
				guard.Close()
				_, e = r.Lstat(previousPath)
			}
			if e != nil && !os.IsNotExist(e) {
				return nil, e
			}
			if e == nil {
				if previous.VersionID != file.VersionID || previous.SHA1 != strings.ToLower(file.SHA1) || path.Dir(previousPath) != destination {
					return nil, fmt.Errorf("уже установлена другая версия проекта %s (%s); удалите старый JAR через файловый менеджер перед установкой", file.Title, previousPath)
				}
				matches, e := extensionFileMatches(ctx, r, previousPath, file)
				if e != nil {
					return nil, e
				}
				if !matches {
					return nil, fmt.Errorf("existing catalog file was changed: %s", previousPath)
				}
				skipped = append(skipped, previousPath)
				*logs = append(*logs, "Already installed: "+previousPath)
				continue
			}
			// A record is stale only after both its active and disabled JAR are absent.
			delete(manifest, file.ProjectID)
		}
		guard, _, e := openJail(dir, target)
		if e != nil {
			return nil, e
		}
		guard.Close()
		info, e := r.Lstat(target)
		if os.IsNotExist(e) {
			install = append(install, file)
			manifest[file.ProjectID] = installedExtension{file.VersionID, target, strings.ToLower(file.SHA1)}
			continue
		}
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("existing file conflicts with catalog installation: %s", target)
		}
		matches, e := extensionFileMatches(ctx, r, target, file)
		if e != nil {
			return nil, e
		}
		if !matches {
			return nil, fmt.Errorf("existing file conflicts with catalog installation: %s", target)
		}
		skipped = append(skipped, target)
		manifest[file.ProjectID] = installedExtension{file.VersionID, target, strings.ToLower(file.SHA1)}
		*logs = append(*logs, "Already installed: "+target)
	}
	stageName, stage, err := newFileStage(r)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	defer r.RemoveAll(stageName)
	// Public downloads never use the panel's mTLS identity. Restrict redirects
	// as well as the initial URL so vendor links cannot change the download origin.
	client := http.Client{Timeout: 30 * time.Minute}
	if a.downloadClient != nil {
		client = *a.downloadClient
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !modrinthDownloadURL(req.URL) {
			return errors.New("Modrinth download redirects must stay on its HTTPS CDN")
		}
		return nil
	}
	downloader := &Agent{cfg: a.cfg, downloadClient: &client}
	entries := make([]stagedEntry, 0, len(install))
	installed := make([]string, 0, len(install))
	for _, file := range install {
		*logs = append(*logs, "Downloading from Modrinth: "+file.Name)
		artifact := model.Artifact{Name: file.Name, URL: file.URL, Size: file.Size, SHA1: file.SHA1}
		if err = downloader.download(ctx, filepath.Join(dir, stageName), file.Name, artifact, true); err != nil {
			return nil, fmt.Errorf("download %s: %w", file.Name, err)
		}
		matches, e := extensionFileMatches(ctx, stage, file.Name, file)
		if e != nil {
			return nil, e
		}
		if !matches {
			return nil, fmt.Errorf("Modrinth file checksum mismatch: %s", file.Name)
		}
		if err = ownManagedPath(stage, file.Name); err != nil {
			return nil, err
		}
		entries = append(entries, stagedEntry{file.Name, false})
		installed = append(installed, path.Join(destination, file.Name))
	}
	if err = a.requireStopped(ctx, serverID); err != nil {
		return nil, err
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(manifestData) > 4<<20 {
		return nil, errors.New("installed Modrinth manifest exceeds 4 MiB")
	}
	manifestTemp, err := os.CreateTemp(manifestDir, ".modrinth-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(manifestTemp.Name())
	_, err = manifestTemp.Write(manifestData)
	if err == nil {
		err = manifestTemp.Sync()
	}
	if e := manifestTemp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return nil, err
	}
	_, directoryErr := r.Lstat(destination)
	if err = commitEntries(ctx, dir, r, stageName, destination, entries, logs); err != nil {
		return nil, err
	}
	if err = os.Rename(manifestTemp.Name(), manifestPath); err != nil {
		for i := len(install) - 1; i >= 0; i-- {
			target := path.Join(destination, install[i].Name)
			if e := r.Rename(target, path.Join(stageName, install[i].Name)); e != nil {
				*logs = append(*logs, "Modrinth metadata rollback failed for "+target+": "+e.Error())
				err = errors.Join(err, e)
			}
		}
		if os.IsNotExist(directoryErr) {
			if e := r.Remove(destination); e != nil {
				*logs = append(*logs, "Retained catalog directory: "+destination)
			}
		}
		return nil, fmt.Errorf("save installed Modrinth projects: %w", err)
	}
	*logs = append(*logs, fmt.Sprintf("Installed %d Modrinth files; %d already present", len(installed), len(skipped)))
	return map[string]any{"installed": installed, "skipped": skipped}, nil
}
