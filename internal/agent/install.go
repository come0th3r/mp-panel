package agent

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

func (a *Agent) download(ctx context.Context, dir, target string, artifact model.Artifact, exclusive bool) error {
	u, err := url.Parse(artifact.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("artifact must have a valid HTTPS URL")
	}
	client := a.downloadClient
	limit := int64(2 << 30)
	upload := artifact.ID != ""
	if upload {
		panel, _ := url.Parse(a.cfg.PanelURL)
		if !identifier.MatchString(artifact.ID) || u.Host != panel.Host || u.Path != "/agent/v1/artifacts/"+artifact.ID || u.RawQuery != "" {
			return errors.New("upload artifact must use the authenticated panel artifact endpoint")
		}
		// Authenticated uploads are streamed to disk. Their recorded size and
		// checksum define the accepted body; command cancellation sets lifetime.
		client = &http.Client{Transport: a.client.Transport, CheckRedirect: a.client.CheckRedirect}
		limit = artifact.Size
	}
	if artifact.Size < 0 || artifact.Size > limit {
		return errors.New("artifact size exceeds download limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", a.cfg.DownloadUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact download HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return errors.New("artifact content exceeds download limit")
	}
	r, p, err := openJail(dir, target)
	if err != nil {
		return err
	}
	defer r.Close()
	if p == "." {
		return errors.New("invalid artifact destination")
	}
	if exclusive {
		if _, err = r.Lstat(p); err == nil {
			return errors.New("destination already exists")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.CreateTemp(dir, ".mp-download-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	sha256hash := sha256.New()
	sha1hash := sha1.New()
	writer := io.MultiWriter(f, sha256hash, sha1hash)
	var n int64
	if upload {
		// CopyN avoids size+1 overflow for int64 metadata, and the trailing
		// probe rejects a body larger than the verified upload without storing it.
		n, err = io.CopyN(writer, resp.Body, artifact.Size)
		if errors.Is(err, io.EOF) {
			err = errors.New("artifact size mismatch")
		}
		if err == nil {
			var probe [1]byte
			var extra int
			extra, err = io.ReadFull(resp.Body, probe[:])
			if extra != 0 {
				err = errors.New("artifact size mismatch")
			} else if errors.Is(err, io.EOF) {
				err = nil
			}
		}
	} else {
		n, err = io.Copy(writer, io.LimitReader(resp.Body, limit+1))
		if err == nil && n > limit {
			err = errors.New("artifact exceeds size limit")
		}
	}
	if err == nil && artifact.Size > 0 && n != artifact.Size {
		err = errors.New("artifact size mismatch")
	}
	if err == nil && artifact.SHA256 != "" && !strings.EqualFold(hex.EncodeToString(sha256hash.Sum(nil)), artifact.SHA256) {
		err = errors.New("artifact SHA256 mismatch")
	}
	if err == nil && artifact.SHA1 != "" && !strings.EqualFold(hex.EncodeToString(sha1hash.Sum(nil)), artifact.SHA1) {
		err = errors.New("artifact SHA1 mismatch")
	}
	if err == nil {
		err = f.Chmod(0640)
	}
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("empty artifact")
	}
	return r.Rename(filepath.Base(tmp), p)
}

func (a *Agent) install(ctx context.Context, server model.Server, install model.InstallSpec, logs *[]string) error {
	if server.ID == "" || server.Core != install.Core || server.MinecraftVersion != install.MinecraftVersion || server.JavaVersion != install.JavaVersion {
		return errors.New("server and install profile do not match")
	}
	if server.MemoryMB < 512 || server.CPULimit <= 0 || server.Port < 1 || server.Port > 65535 {
		return errors.New("invalid server resources")
	}
	image, err := javaImage(install.JavaVersion)
	if err != nil {
		return err
	}
	if len(install.Artifacts) == 0 {
		return errors.New("install profile has no artifacts")
	}
	dir, err := a.serverDir(server.ID)
	if err != nil {
		return err
	}
	if err = a.requireStopped(ctx, server.ID); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(a.cfg.DataDir, "servers"), ".staging-"+server.ID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if _, err = os.Stat(dir); err == nil {
		if err = copyTree(ctx, dir, stage); err != nil {
			return err
		}
		if old, e := a.readSpec(server.ID); e == nil {
			for _, artifact := range old.Install.Artifacts {
				target := artifact.Target
				if target == "" {
					target = artifact.Name
				}
				if target != "" {
					if clean, e := cleanPath(target); e == nil && clean != "." {
						if e = removeIfExists(stage, clean); e != nil {
							return e
						}
					}
				}
			}
			if old.Install.LaunchJar != "" {
				if e = removeIfExists(stage, old.Install.LaunchJar); e != nil {
					return e
				}
			}
			for _, target := range []string{"libraries", "user_jvm_args.txt", "run.sh", "run.bat"} {
				if e = removeIfExists(stage, target); e != nil {
					return e
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, artifact := range install.Artifacts {
		target := artifact.Target
		if target == "" {
			target = artifact.Name
		}
		clean, e := cleanPath(target)
		if e != nil || clean == "." {
			return errors.New("invalid install artifact target")
		}
		if !strings.HasSuffix(strings.ToLower(clean), ".jar") {
			return errors.New("installer artifacts must be JAR files")
		}
		if e = makeDir(stage, filepath.ToSlash(filepath.Dir(clean))); e != nil {
			return e
		}
		*logs = append(*logs, "Downloading "+artifact.Name)
		if e = a.download(ctx, stage, clean, artifact, false); e != nil {
			return fmt.Errorf("download %s: %w", artifact.Name, e)
		}
	}
	launch := []string{"java", fmt.Sprintf("-Xms%dM", min(512, server.MemoryMB/2)), fmt.Sprintf("-Xmx%dM", server.MemoryMB*75/100)}
	mode := install.InstallMode
	switch mode {
	case "direct", "jar", "custom", "":
		jar := install.LaunchJar
		if jar == "" {
			jar = install.Artifacts[0].Target
			if jar == "" {
				jar = install.Artifacts[0].Name
			}
		}
		if err = checkLaunchJar(stage, jar); err != nil {
			return err
		}
		install.LaunchJar = jar
		launch = append(launch, "-jar", jar, "nogui")
	case "forge-installer", "neoforge-installer", "installer", "forge", "neoforge":
		installer := install.Artifacts[0].Target
		if installer == "" {
			installer = install.Artifacts[0].Name
		}
		if err = prepareOwnership(stage); err != nil {
			return err
		}
		*logs = append(*logs, "Installing loader and its server libraries")
		if err = a.runInstaller(ctx, server.ID, stage, image, []string{"java", "-jar", installer, "--installServer"}, logs); err != nil {
			return err
		}
		var argsFiles []string
		err = filepath.WalkDir(filepath.Join(stage, "libraries"), func(p string, entry os.DirEntry, e error) error {
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("installer created symbolic link")
			}
			if entry.Name() == "unix_args.txt" {
				relative, e := filepath.Rel(stage, p)
				if e != nil {
					return e
				}
				argsFiles = append(argsFiles, filepath.ToSlash(relative))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(argsFiles) == 1 {
			launch = append(launch, "@"+argsFiles[0], "nogui")
		} else if len(argsFiles) > 1 {
			return errors.New("installer produced multiple launch profiles")
		} else {
			entries, e := os.ReadDir(stage)
			if e != nil {
				return e
			}
			candidates := []string{}
			for _, entry := range entries {
				n := entry.Name()
				if strings.HasPrefix(n, "forge-") && strings.HasSuffix(n, ".jar") && n != installer && !strings.Contains(n, "installer") {
					candidates = append(candidates, n)
				}
			}
			if len(candidates) != 1 {
				return errors.New("loader installer did not produce a supported launch profile")
			}
			install.LaunchJar = candidates[0]
			launch = append(launch, "-jar", candidates[0], "nogui")
		}
	case "spigot-buildtools", "buildtools", "spigot":
		builder := "mp-buildtools-java:" + strconv.Itoa(install.JavaVersion)
		*logs = append(*logs, "Preparing BuildTools runtime with Git")
		dockerfile := "FROM " + image + "\nRUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*\n"
		out, e := docker(ctx, []byte(dockerfile), "build", "--label", "mp.managed=true", "--tag", builder, "-")
		*logs = append(*logs, splitLines(out)...)
		if e != nil {
			return e
		}
		if err = prepareOwnership(stage); err != nil {
			return err
		}
		tool := install.Artifacts[0].Target
		if tool == "" {
			tool = install.Artifacts[0].Name
		}
		if err = a.runInstaller(ctx, server.ID, stage, builder, []string{"java", "-jar", tool, "--rev", install.MinecraftVersion, "--compile", "SPIGOT"}, logs); err != nil {
			return err
		}
		jar := "spigot-" + install.MinecraftVersion + ".jar"
		if err = checkLaunchJar(stage, jar); err != nil {
			return err
		}
		install.LaunchJar = jar
		launch = append(launch, "-jar", jar, "nogui")
	default:
		return fmt.Errorf("unsupported install mode %q", mode)
	}
	for _, extra := range install.ExtraJars {
		target := extra.Target
		if target == "" {
			folder := "mods"
			if server.Core == "paper" || server.Core == "spigot" {
				folder = "plugins"
			}
			target = folder + "/" + extra.Name
		}
		clean, e := cleanPath(target)
		if e != nil {
			return e
		}
		parts := strings.Split(clean, "/")
		if len(parts) != 2 || (parts[0] != "mods" && parts[0] != "plugins") || !strings.HasSuffix(strings.ToLower(parts[1]), ".jar") {
			return errors.New("extra JAR must be installed in mods or plugins")
		}
		if e = makeDir(stage, parts[0]); e != nil {
			return e
		}
		if e = a.download(ctx, stage, clean, extra, false); e != nil {
			return e
		}
	}
	// The file manager and extension list must work before Minecraft has
	// generated its directories on the first successful launch.
	if server.Core == "paper" || server.Core == "spigot" {
		if err = makeDir(stage, "plugins"); err != nil {
			return err
		}
	} else if server.Core == "fabric" || server.Core == "forge" || server.Core == "neoforge" {
		if err = makeDir(stage, "mods"); err != nil {
			return err
		}
	}
	if err = writeText(stage, "eula.txt", "eula=true\n", ""); err != nil {
		return err
	}
	properties := map[string]string{"motd": server.Name, "online-mode": "true", "difficulty": "normal", "gamemode": "survival", "max-players": "20"}
	if f, e := readText(stage, "server.properties"); e == nil {
		properties, e = parseProperties(f.Content)
		if e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	properties["server-port"] = "25565"
	properties["server-ip"] = ""
	properties["enable-rcon"] = "false"
	properties["enable-query"] = "false"
	if err = writeText(stage, "server.properties", encodeProperties(properties), ""); err != nil {
		return err
	}
	if err = prepareOwnership(stage); err != nil {
		return err
	}
	if err = a.removeContainer(ctx, server.ID, logs); err != nil {
		return err
	}
	oldDir := dir + ".previous"
	if _, err = os.Stat(oldDir); err == nil {
		return errors.New("previous install directory exists; recover interrupted installation before retrying")
	}
	hadOld := false
	if _, e := os.Stat(dir); e == nil {
		if err = os.Rename(dir, oldDir); err != nil {
			return err
		}
		hadOld = true
	}
	if err = os.Rename(stage, dir); err != nil {
		if hadOld {
			_ = os.Rename(oldDir, dir)
		}
		return err
	}
	if err = atomicJSON(a.specPath(server.ID), serverSpec{Server: server, Install: install, Launch: launch}, 0600); err != nil {
		_ = os.Rename(dir, stage)
		if hadOld {
			_ = os.Rename(oldDir, dir)
		}
		return err
	}
	if hadOld {
		if err = os.RemoveAll(oldDir); err != nil {
			*logs = append(*logs, "Installed successfully, but prior staging directory needs cleanup: "+err.Error())
		}
	}
	*logs = append(*logs, "Installation committed; launching the selected profile")
	return nil
}

func (a *Agent) runInstaller(ctx context.Context, id, dir, image string, launch []string, logs *[]string) error {
	name := "mp-installer-" + id
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = docker(cleanup, nil, "rm", "--force", name)
	}()
	args := []string{"run", "--rm", "--name", name, "--label", "mp.managed=true", "--label", "mp.server_id=" + id, "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "512", "--memory", "2g", "--workdir", "/data", "--env", "HOME=/data", "--mount", "type=bind,source=" + dir + ",target=/data", image}
	args = append(args, launch...)
	out, err := docker(ctx, nil, args...)
	*logs = append(*logs, splitLines(out)...)
	return err
}

func checkLaunchJar(dir, jar string) error {
	r, p, err := openJail(dir, jar)
	if err != nil {
		return err
	}
	defer r.Close()
	if !strings.HasSuffix(strings.ToLower(p), ".jar") {
		return errors.New("launch target must be a JAR")
	}
	info, err := r.Stat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("launch JAR is not a nonempty regular file")
	}
	return nil
}

func removeIfExists(dir, p string) error {
	r, clean, err := openJail(dir, p)
	if err != nil {
		return err
	}
	defer r.Close()
	if clean == "." {
		return errors.New("cannot remove server root")
	}
	if _, err = r.Lstat(clean); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return r.RemoveAll(clean)
}

func copyTree(ctx context.Context, src, dst string) error {
	r, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer r.Close()
	return filepath.WalkDir(src, func(p string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symbolic links cannot be copied")
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		target := filepath.Join(dst, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0750)
		}
		if !info.Mode().IsRegular() {
			return errors.New("special files cannot be copied")
		}
		in, e := r.Open(relative)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, contextReader{ctx: ctx, r: in})
		cerr := out.Close()
		if e == nil {
			e = cerr
		}
		return e
	})
}
