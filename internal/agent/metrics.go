package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

type cpuSample struct{ total, idle uint64 }

func (a *Agent) heartbeatLoop(ctx context.Context) {
	for ctx.Err() == nil {
		cycle, cancel := context.WithTimeout(ctx, 30*time.Second)
		hb := a.heartbeat(cycle)
		_, err := a.request(cycle, "POST", "/agent/v1/heartbeat", hb, nil)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "heartbeat: %v\n", err)
		}
		if !pause(ctx, time.Duration(a.cfg.PollSeconds)*time.Second) {
			return
		}
	}
}

func (a *Agent) heartbeat(ctx context.Context) model.Heartbeat {
	now := time.Now().UTC().Format(time.RFC3339)
	node := model.Node{ID: a.cfg.NodeID, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUCount: runtime.NumCPU(), Status: "online", LastSeen: now}
	hb := model.Heartbeat{Node: node, Servers: []model.ObservedServer{}, Metrics: []model.Metric{}}
	metric, err := a.nodeMetric(now)
	if err == nil {
		hb.Metrics = append(hb.Metrics, metric)
		hb.Node.MemoryTotal = metric.MemoryLimit
		hb.Node.DiskTotal = metric.DiskUsed + metric.DiskFree
		hb.Node.DiskFree = metric.DiskFree
	} else {
		hb.Node.Status = "error"
		hb.Node.Error = err.Error()
	}
	if b, e := os.ReadFile("/etc/os-release"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				hb.Node.OS = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
			}
		}
	}
	if _, err = docker(ctx, nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		hb.Node.Status = "error"
		hb.Node.Error = "Docker unavailable: " + err.Error()
	}
	entries, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "specs"))
	if err != nil {
		hb.Node.Status = "error"
		hb.Node.Error = "Read server inventory: " + err.Error()
		return hb
	}
	logBudget := 256 << 10
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !identifier.MatchString(id) {
			continue
		}
		observed := model.ObservedServer{ID: id, Status: "stopped", Logs: []string{}}
		inspect, exists, err := a.inspect(ctx, id)
		if err != nil {
			observed.Status = "error"
			observed.Error = err.Error()
			hb.Servers = append(hb.Servers, observed)
			continue
		}
		if exists {
			if inspect.State.Running {
				observed.Status = "starting"
				// Remember a successful readiness observation across log rotation
				// and agent restarts, scoped to this exact process start time.
				ready, e := os.ReadFile(filepath.Join(a.cfg.DataDir, "specs", id+".ready"))
				if e == nil && string(ready) == inspect.State.StartedAt {
					observed.Status = "running"
				}
			} else if inspect.State.Status == "restarting" {
				observed.Status = "starting"
			} else if inspect.State.OOMKilled || (inspect.State.ExitCode != 0 && !(inspect.State.ExitCode == 143 && inspect.HostConfig.RestartPolicy.Name == "no")) || inspect.State.Error != "" {
				observed.Status = "error"
				observed.Error = fmt.Sprintf("container exited %d; OOM=%t; %s", inspect.State.ExitCode, inspect.State.OOMKilled, inspect.State.Error)
			}
			logs, e := docker(ctx, nil, "logs", "--since", inspect.State.StartedAt, "--tail", "250", containerName(id))
			if e == nil {
				observed.Logs = boundedLines(splitLines(logs), min(logBudget, 32<<10))
				encodedLogs, _ := json.Marshal(observed.Logs)
				logBudget -= len(encodedLogs)
				if inspect.State.Running && strings.Contains(logs, "Done (") && tcpReachable(ctx, a.gameEndpointFromSpec(id)) {
					observed.Status = "running"
					_ = atomicWrite(filepath.Join(a.cfg.DataDir, "specs", id+".ready"), []byte(inspect.State.StartedAt), 0600)
				}
			}
			if inspect.State.Running {
				if metric, e := a.containerMetric(ctx, id, now, inspect.State.StartedAt); e == nil {
					hb.Metrics = append(hb.Metrics, metric)
				} else {
					observed.Error = "metrics unavailable: " + e.Error()
				}
			}
		}
		hb.Servers = append(hb.Servers, observed)
	}
	return hb
}

func (a *Agent) gameEndpointFromSpec(id string) string {
	s, err := a.readSpec(id)
	if err != nil {
		return ""
	}
	return a.gameEndpoint(s.Server.Port)
}

func (a *Agent) nodeMetric(now string) (model.Metric, error) {
	m := model.Metric{Timestamp: now, NodeID: a.cfg.NodeID}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return m, err
	}
	fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return m, errors.New("invalid /proc/stat")
	}
	var sample cpuSample
	for i, s := range fields[1:] {
		if i >= 8 {
			break
		}
		n, e := strconv.ParseUint(s, 10, 64)
		if e != nil {
			return m, e
		}
		sample.total += n
		if i == 3 || i == 4 {
			sample.idle += n
		}
	}
	if a.previousCPU.total != 0 && sample.total > a.previousCPU.total && sample.idle >= a.previousCPU.idle {
		total := sample.total - a.previousCPU.total
		idle := sample.idle - a.previousCPU.idle
		if idle > total {
			a.previousCPU = sample
			return m, errors.New("CPU counters changed; resampling")
		}
		m.CPUPercent = float64(total-idle) / float64(total) * 100
	} else {
		// Establish a first measurable sample instead of publishing an invented 0.
		a.previousCPU = sample
		return m, errors.New("CPU sample initializing")
	}
	a.previousCPU = sample
	b, err = os.ReadFile("/proc/meminfo")
	if err != nil {
		return m, err
	}
	mem := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			n, e := strconv.ParseUint(f[1], 10, 64)
			if e == nil {
				mem[strings.TrimSuffix(f[0], ":")] = n * 1024
			}
		}
	}
	if mem["MemTotal"] == 0 || mem["MemAvailable"] > mem["MemTotal"] {
		return m, errors.New("invalid memory observation")
	}
	m.MemoryLimit = mem["MemTotal"]
	m.MemoryUsed = m.MemoryLimit - mem["MemAvailable"]
	total, free, err := diskSpace(a.cfg.DataDir)
	if err != nil {
		return m, err
	}
	m.DiskUsed = total - free
	m.DiskFree = free
	b, err = os.ReadFile("/proc/diskstats")
	if err != nil {
		return m, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		device := f[2]
		if strings.HasPrefix(device, "loop") || strings.HasPrefix(device, "ram") {
			continue
		}
		if _, e := os.Stat(filepath.Join("/sys/block", device)); e != nil {
			continue
		}
		reads, e1 := strconv.ParseUint(f[5], 10, 64)
		writes, e2 := strconv.ParseUint(f[9], 10, 64)
		if e1 == nil && e2 == nil {
			m.IORead += reads * 512
			m.IOWrite += writes * 512
		}
	}
	b, err = os.ReadFile("/proc/uptime")
	if err != nil {
		return m, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return m, errors.New("invalid uptime observation")
	}
	uptime, e := strconv.ParseFloat(f[0], 64)
	if e != nil {
		return m, e
	}
	m.UptimeSeconds = uint64(uptime)
	return m, nil
}

func (a *Agent) containerMetric(ctx context.Context, id, now, started string) (model.Metric, error) {
	m := model.Metric{Timestamp: now, NodeID: a.cfg.NodeID, ServerID: id}
	out, err := docker(ctx, nil, "stats", "--no-stream", "--format", "{{json .}}", containerName(id))
	if err != nil {
		return m, err
	}
	var stats struct {
		CPUPerc  string
		MemUsage string
		BlockIO  string
	}
	if err = json.Unmarshal([]byte(strings.TrimSpace(out)), &stats); err != nil {
		return m, err
	}
	m.CPUPercent, err = strconv.ParseFloat(strings.TrimSuffix(stats.CPUPerc, "%"), 64)
	if err != nil {
		return m, err
	}
	mem := strings.Split(stats.MemUsage, "/")
	block := strings.Split(stats.BlockIO, "/")
	if len(mem) != 2 || len(block) != 2 {
		return m, errors.New("incomplete Docker resource observation")
	}
	if m.MemoryUsed, err = parseBytes(mem[0]); err != nil {
		return m, err
	}
	if m.MemoryLimit, err = parseBytes(mem[1]); err != nil {
		return m, err
	}
	if m.IORead, err = parseBytes(block[0]); err != nil {
		return m, err
	}
	if m.IOWrite, err = parseBytes(block[1]); err != nil {
		return m, err
	}
	dir, _ := a.serverDir(id)
	disk, err := exec.CommandContext(ctx, "du", "-sb", "--", dir).Output()
	if err != nil {
		return m, fmt.Errorf("server disk usage: %w", err)
	}
	fields := strings.Fields(string(disk))
	if len(fields) == 0 {
		return m, errors.New("missing disk usage")
	}
	m.DiskUsed, err = strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return m, err
	}
	_, m.DiskFree, err = diskSpace(dir)
	if err != nil {
		return m, err
	}
	if t, e := time.Parse(time.RFC3339Nano, started); e == nil && t.Before(time.Now()) {
		m.UptimeSeconds = uint64(time.Since(t).Seconds())
	}
	return m, nil
}

func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, errors.New("invalid byte observation")
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, err
	}
	unit := strings.TrimSpace(s[i:])
	multipliers := map[string]float64{"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "KiB": 1024, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}
	mult, ok := multipliers[unit]
	if !ok || n < 0 {
		return 0, errors.New("invalid byte unit")
	}
	return uint64(n * mult), nil
}
