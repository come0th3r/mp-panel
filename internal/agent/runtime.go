package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

type serverSpec struct {
	Server  model.Server      `json:"server"`
	Install model.InstallSpec `json:"install"`
	Launch  []string          `json:"launch"`
}

type payload struct {
	Path       string            `json:"path"`
	NewPath    string            `json:"new_path"`
	Content    string            `json:"content"`
	Revision   string            `json:"revision"`
	Command    string            `json:"command"`
	Artifact   model.Artifact    `json:"artifact"`
	Properties map[string]string `json:"properties"`
	Enabled    bool              `json:"enabled"`
	BackupID   string            `json:"backup_id"`
	Name       string            `json:"name"`
	BackupName string            `json:"backup_name"`
}

func (a *Agent) execute(ctx context.Context, c model.Command) (any, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dir, err := a.serverDir(c.ServerID)
	if err != nil {
		return nil, nil, err
	}
	var p payload
	if len(c.Payload) > 0 {
		if err = json.Unmarshal(c.Payload, &p); err != nil {
			return nil, nil, err
		}
	}
	logs := []string{}
	var output any = map[string]bool{"ok": true}
	if c.Type == "create" || c.Type == "install" {
		if c.Server == nil || c.Install == nil || c.Server.ID != c.ServerID || c.Server.NodeID != a.cfg.NodeID {
			return nil, logs, errors.New("server and install specification required")
		}
		if !c.Server.EULAAccepted {
			return nil, logs, errors.New("EULA acceptance required")
		}
		if c.Type == "create" {
			if _, err = os.Stat(a.specPath(c.ServerID)); err == nil {
				return nil, logs, errors.New("server already exists")
			} else if !os.IsNotExist(err) {
				return nil, logs, err
			}
		} else {
			if _, err = a.readSpec(c.ServerID); err != nil {
				return nil, logs, err
			}
			if err = a.stop(ctx, c.ServerID, &logs); err != nil {
				return nil, logs, err
			}
			backup, err := a.backup(ctx, c.ServerID, p.BackupID, p.BackupName)
			if err != nil {
				return nil, logs, err
			}
			output = map[string]any{"backup": backup}
			logs = append(logs, "Offline snapshot created before installation: "+backup.ID)
		}
		if err = a.install(ctx, *c.Server, *c.Install, &logs); err != nil {
			return output, logs, err
		}
		installed, e := a.readSpec(c.ServerID)
		if e != nil {
			return output, logs, e
		}
		installed.Server.Status = "starting"
		installed.Server.DesiredState = "running"
		if out, ok := output.(map[string]any); ok {
			out["server"] = installed.Server
		} else {
			output = map[string]any{"server": installed.Server}
		}
		if err = a.start(ctx, c.ServerID, &logs); err != nil {
			return output, logs, err
		}
		installed.Server.Status = "running"
		output.(map[string]any)["server"] = installed.Server
		return output, logs, nil
	}
	if _, err = a.readSpec(c.ServerID); err != nil && !(c.Type == "delete" && os.IsNotExist(err)) {
		return nil, logs, err
	}
	switch c.Type {
	case "start":
		err = a.start(ctx, c.ServerID, &logs)
	case "stop":
		err = a.stop(ctx, c.ServerID, &logs)
	case "restart":
		if err = a.stop(ctx, c.ServerID, &logs); err == nil {
			err = a.start(ctx, c.ServerID, &logs)
		}
	case "delete":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		if err = a.removeContainer(ctx, c.ServerID, &logs); err != nil {
			break
		}
		// Delete through Root to avoid following game-created links outside
		// this server tree.
		root, e := os.OpenRoot(filepath.Join(a.cfg.DataDir, "servers"))
		if e != nil {
			err = e
			break
		}
		err = root.RemoveAll(c.ServerID)
		root.Close()
		if err == nil {
			err = os.Remove(a.specPath(c.ServerID))
			if os.IsNotExist(err) {
				err = nil
			}
			_ = os.Remove(filepath.Join(a.cfg.DataDir, "specs", c.ServerID+".ready"))
		}
	case "logs":
		var out string
		out, err = a.managedDocker(ctx, c.ServerID, "logs", "--tail", "250", "--timestamps", containerName(c.ServerID))
		if err == nil {
			output = map[string]any{"lines": splitLines(out)}
		}
	case "console":
		err = a.console(ctx, c.ServerID, p.Command)
	case "file.list":
		output, err = listFiles(dir, p.Path)
	case "file.read":
		output, err = readText(dir, p.Path)
	case "file.write":
		if err = a.requireStopped(ctx, c.ServerID); err == nil {
			err = writeText(dir, p.Path, p.Content, p.Revision)
		}
	case "file.upload":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		if p.Artifact.ID == "" {
			err = errors.New("upload artifact ID required")
			break
		}
		err = a.download(ctx, dir, p.Path, p.Artifact, true)
	case "file.transfer":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		var transfer fileTransferPayload
		if err = json.Unmarshal(c.Payload, &transfer); err == nil {
			output, err = transferFiles(ctx, dir, transfer, &logs)
		}
	case "file.extract":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		var extract fileExtractPayload
		if err = json.Unmarshal(c.Payload, &extract); err == nil {
			output, err = extractFiles(ctx, dir, extract, &logs)
		}
	case "file.upload_batch":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		var upload fileUploadPayload
		if err = json.Unmarshal(c.Payload, &upload); err == nil {
			output, err = a.uploadFiles(ctx, dir, upload, &logs)
		}
	case "file.delete_batch":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		var deletion fileDeletePayload
		if err = json.Unmarshal(c.Payload, &deletion); err == nil {
			output, err = deleteFiles(ctx, dir, deletion, &logs)
		}
	case "file.mkdir":
		if err = a.requireStopped(ctx, c.ServerID); err == nil {
			err = makeDir(dir, p.Path)
		}
	case "file.rename":
		if err = a.requireStopped(ctx, c.ServerID); err == nil {
			err = renamePath(dir, p.Path, p.NewPath)
		}
	case "file.delete":
		if err = a.requireStopped(ctx, c.ServerID); err == nil {
			err = removePath(dir, p.Path)
		}
	case "properties.read":
		var f model.FileContent
		f, err = readText(dir, "server.properties")
		if err != nil {
			break
		}
		var properties map[string]string
		properties, err = parseProperties(f.Content)
		if err == nil {
			output = map[string]any{"properties": properties, "revision": f.Revision}
		}
	case "properties.write":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		if p.Properties == nil {
			err = errors.New("properties required")
			break
		}
		// Minecraft listens on its private container port; the requested public
		// port is managed by Docker and cannot be changed through this file.
		p.Properties["server-port"] = "25565"
		p.Properties["server-ip"] = ""
		p.Properties["enable-rcon"] = "false"
		p.Properties["enable-query"] = "false"
		err = writeText(dir, "server.properties", encodeProperties(p.Properties), p.Revision)
	case "extension.toggle":
		if err = a.requireStopped(ctx, c.ServerID); err != nil {
			break
		}
		var clean string
		clean, err = cleanPath(p.Path)
		if err != nil {
			break
		}
		parts := strings.Split(clean, "/")
		if len(parts) != 2 || (parts[0] != "mods" && parts[0] != "plugins") {
			err = errors.New("extension must be a direct child of mods or plugins")
			break
		}
		if p.Enabled {
			if !strings.HasSuffix(clean, ".jar.disabled") {
				err = errors.New("extension is not disabled")
				break
			}
			err = renamePath(dir, clean, strings.TrimSuffix(clean, ".disabled"))
		} else {
			if !strings.HasSuffix(clean, ".jar") {
				err = errors.New("extension must be a JAR")
				break
			}
			err = renamePath(dir, clean, clean+".disabled")
		}
	case "backup":
		output, err = a.backup(ctx, c.ServerID, p.BackupID, p.Name)
	case "restore":
		err = a.restore(ctx, c.ServerID, p.BackupID, &logs)
		if err == nil {
			var restored serverSpec
			restored, err = a.readSpec(c.ServerID)
			if err == nil {
				restored.Server.Status = "stopped"
				restored.Server.DesiredState = "stopped"
				output = map[string]any{"server": restored.Server}
			}
		}
	default:
		err = fmt.Errorf("unsupported command %q", c.Type)
	}
	return output, logs, err
}

func (a *Agent) specPath(id string) string { return filepath.Join(a.cfg.DataDir, "specs", id+".json") }
func (a *Agent) readSpec(id string) (serverSpec, error) {
	var s serverSpec
	if !identifier.MatchString(id) {
		return s, errors.New("invalid server ID")
	}
	b, err := os.ReadFile(a.specPath(id))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
func containerName(id string) string { return "mp-" + id }

type tailBuffer struct {
	b     []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.limit {
		b.b = append(b.b[:0], p[len(p)-b.limit:]...)
	} else {
		if len(b.b)+len(p) > b.limit {
			b.b = append([]byte(nil), b.b[len(b.b)+len(p)-b.limit:]...)
		}
		b.b = append(b.b, p...)
	}
	return n, nil
}

func docker(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out := &tailBuffer{limit: 1 << 20}
	cmd.Stdout = out
	cmd.Stderr = out
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if err != nil {
		return string(out.b), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out.b)))
	}
	return string(out.b), nil
}

type containerInspect struct {
	ID     string `json:"Id"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		OOMKilled bool   `json:"OOMKilled"`
		Error     string `json:"Error"`
		StartedAt string `json:"StartedAt"`
		ExitCode  int    `json:"ExitCode"`
	} `json:"State"`
}

func (a *Agent) inspect(ctx context.Context, id string) (containerInspect, bool, error) {
	var inspect containerInspect
	if !identifier.MatchString(id) {
		return inspect, false, errors.New("invalid server ID")
	}
	out, err := docker(ctx, nil, "inspect", containerName(id))
	if err != nil {
		message := strings.ToLower(out)
		if strings.Contains(message, "no such object") || strings.Contains(message, "no such container") {
			return inspect, false, nil
		}
		return inspect, false, err
	}
	var list []containerInspect
	if err = json.Unmarshal([]byte(out), &list); err != nil || len(list) != 1 {
		if err == nil {
			err = errors.New("unexpected container inspection")
		}
		return inspect, false, err
	}
	inspect = list[0]
	if inspect.Config.Labels["mp.managed"] != "true" || inspect.Config.Labels["mp.server_id"] != id {
		return inspect, false, errors.New("container is not owned by MP")
	}
	return inspect, true, nil
}

func (a *Agent) managedDocker(ctx context.Context, id string, args ...string) (string, error) {
	if _, exists, err := a.inspect(ctx, id); err != nil {
		return "", err
	} else if !exists {
		return "", errors.New("server container not found")
	}
	return docker(ctx, nil, args...)
}

func (a *Agent) requireStopped(ctx context.Context, id string) error {
	inspect, exists, err := a.inspect(ctx, id)
	if err != nil {
		return err
	}
	if exists && (inspect.State.Running || inspect.State.Status == "restarting" || inspect.State.Status == "paused") {
		return errors.New("stop the server before changing files or creating/restoring a backup")
	}
	if exists && inspect.HostConfig.RestartPolicy.Name != "no" {
		return errors.New("stop the server through the panel to disable automatic restarts before offline operations")
	}
	return nil
}

func (a *Agent) stop(ctx context.Context, id string, logs *[]string) error {
	inspect, exists, err := a.inspect(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	out, err := docker(ctx, nil, "update", "--restart", "no", containerName(id))
	*logs = append(*logs, splitLines(out)...)
	if err != nil {
		return err
	}
	if inspect.State.Status == "paused" {
		if _, err = docker(ctx, nil, "unpause", containerName(id)); err != nil {
			return err
		}
	}
	_ = a.console(ctx, id, "stop")
	out, err = docker(ctx, nil, "stop", "--time", "60", containerName(id))
	*logs = append(*logs, splitLines(out)...)
	return err
}

func (a *Agent) removeContainer(ctx context.Context, id string, logs *[]string) error {
	_, exists, err := a.inspect(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err = a.requireStopped(ctx, id); err != nil {
		return err
	}
	out, err := docker(ctx, nil, "rm", containerName(id))
	*logs = append(*logs, splitLines(out)...)
	return err
}

func javaImage(version int) (string, error) {
	switch version {
	case 16:
		return "azul/zulu-openjdk:16", nil
	case 8, 11, 17, 21, 25:
		return fmt.Sprintf("eclipse-temurin:%d-jdk", version), nil
	default:
		return "", fmt.Errorf("unsupported Java major %d; supported: 8,11,16,17,21,25", version)
	}
}

// A deliberately restricted address must belong to this host. Never replace
// it with a wildcard automatically: that would expose a private listener.
func validateLocalGameBind(address string) error {
	ip := net.ParseIP(address)
	if ip == nil {
		return errors.New("Адрес игрового порта должен быть IPv4 или IPv6")
	}
	if ip.IsUnspecified() {
		return nil
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("Не удалось проверить сетевые адреса игрового хоста: %w", err)
	}
	for _, address := range addresses {
		local, _, err := net.ParseCIDR(address.String())
		if err == nil && local.Equal(ip) {
			return nil
		}
	}
	return fmt.Errorf("Адрес %s отсутствует на игровом хосте. В настройках подключения этого хоста укажите его локальный IP или 0.0.0.0 для всех IPv4-интерфейсов, затем повторно подключите агент", address)
}

func gameBindingMatches(inspect containerInspect, address string, port int) bool {
	bindings := inspect.HostConfig.PortBindings["25565/tcp"]
	if len(bindings) != 1 || bindings[0].HostPort != strconv.Itoa(port) {
		return false
	}
	bound := bindings[0].HostIP
	if bound == "" {
		bound = "0.0.0.0"
	}
	actual, desired := net.ParseIP(bound), net.ParseIP(address)
	return actual != nil && desired != nil && actual.Equal(desired)
}

func (a *Agent) start(ctx context.Context, id string, logs *[]string) error {
	s, err := a.readSpec(id)
	if err != nil {
		return err
	}
	if err = validateLocalGameBind(a.cfg.BindAddress); err != nil {
		return err
	}
	if s.Server.MemoryMB < 512 || s.Server.CPULimit <= 0 || s.Server.Port < 1 || s.Server.Port > 65535 || len(s.Launch) == 0 {
		return errors.New("invalid persisted server resources or launch profile")
	}
	image, err := javaImage(s.Server.JavaVersion)
	if err != nil {
		return err
	}
	inspect, exists, err := a.inspect(ctx, id)
	if err != nil {
		return err
	}
	if exists && !gameBindingMatches(inspect, a.cfg.BindAddress, s.Server.Port) {
		if inspect.State.Running || inspect.State.Status == "restarting" || inspect.State.Status == "paused" {
			return errors.New("Адрес игрового порта изменён. Остановите сервер и запустите его снова, чтобы применить сетевые настройки")
		}
		// Remove only the owned, stopped container. The bind-mounted server
		// directory, worlds, backups and persisted launch profile remain intact.
		out, e := docker(ctx, nil, "update", "--restart", "no", containerName(id))
		*logs = append(*logs, splitLines(out)...)
		if e != nil {
			return e
		}
		if err = a.removeContainer(ctx, id, logs); err != nil {
			return err
		}
		*logs = append(*logs, "Удалён остановленный контейнер со старым игровым адресом; файлы и мир сохранены. Создаём контейнер с новым адресом")
		exists = false
	}
	if exists {
		out, e := docker(ctx, nil, "update", "--restart", "unless-stopped", containerName(id))
		*logs = append(*logs, splitLines(out)...)
		if e != nil {
			return e
		}
	}
	if exists && inspect.State.Running {
		return a.waitReady(ctx, id, s.Server, logs)
	}
	if !exists {
		dir, _ := a.serverDir(id)
		if err = prepareOwnership(dir); err != nil {
			return err
		}
		args := []string{"create", "--name", containerName(id), "--label", "mp.managed=true", "--label", "mp.server_id=" + id, "--init", "--interactive", "--restart", "unless-stopped", "--stop-timeout", "60", "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "512", "--memory", strconv.Itoa(s.Server.MemoryMB) + "m", "--cpus", strconv.FormatFloat(s.Server.CPULimit, 'f', 3, 64), "--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--workdir", "/data", "--mount", "type=bind,source=" + dir + ",target=/data", "--publish", net.JoinHostPort(a.cfg.BindAddress, strconv.Itoa(s.Server.Port)) + ":25565/tcp", image}
		args = append(args, s.Launch...)
		out, err := docker(ctx, nil, args...)
		*logs = append(*logs, splitLines(out)...)
		if err != nil {
			return err
		}
	}
	out, err := docker(ctx, nil, "start", containerName(id))
	*logs = append(*logs, splitLines(out)...)
	if err != nil {
		return err
	}
	return a.waitReady(ctx, id, s.Server, logs)
}

func (a *Agent) console(ctx context.Context, id, command string) error {
	if command == "" || len(command) > 4096 || strings.ContainsAny(command, "\r\n\x00") {
		return errors.New("console command must be a single line up to 4096 bytes")
	}
	inspect, exists, err := a.inspect(ctx, id)
	if err != nil {
		return err
	}
	if !exists || !inspect.State.Running {
		return errors.New("server is not running")
	}
	dial := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dial.DialContext(ctx, "unix", "/var/run/docker.sock")
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Docker's stdin attachment writes directly to the Java console. No shell,
	// RCON port, passwords, or game plugin is needed.
	req, err := http.NewRequest(http.MethodPost, "http://docker/containers/"+containerName(id)+"/attach?stream=1&stdin=1&stdout=0&stderr=0", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err = req.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Docker attach returned HTTP %d", resp.StatusCode)
	}
	_, err = conn.Write([]byte(command + "\n"))
	return err
}

func prepareOwnership(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("server directory contains a symbolic link")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("server directory contains a special file")
		}
		if err = os.Chown(path, 1000, 1000); err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0750)
		}
		return os.Chmod(path, 0640)
	})
}

func splitLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > 1000 {
		lines = lines[len(lines)-1000:]
	}
	return lines
}

// Budget is in JSON bytes, including escape expansion. Truncate diagnostic
// tails before transport rather than allowing one noisy server to reject the
// whole heartbeat or keep command results permanently undeliverable.
func boundedLines(lines []string, budget int) []string {
	out := []string{}
	for i := len(lines) - 1; i >= 0 && budget > 0; i-- {
		line := lines[i]
		if len(line) > 4096 {
			line = line[len(line)-4096:]
		}
		encoded, _ := json.Marshal(line)
		if len(encoded)+1 > budget {
			continue
		}
		budget -= len(encoded) + 1
		out = append(out, line)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
