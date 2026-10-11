package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

type Agent struct {
	cfg            model.AgentConfig
	client         *http.Client
	downloadClient *http.Client
	mu             sync.Mutex
	locks          map[string]*sync.Mutex
	running        map[string]bool
	slots          chan struct{}
	readSlots      chan struct{}
	workers        sync.WaitGroup
	previousCPU    cpuSample
}

type journalEntry struct {
	Command model.Command        `json:"command"`
	Status  string               `json:"status"`
	Result  *model.CommandResult `json:"result,omitempty"`
}

func New(cfg model.AgentConfig) (*Agent, error) {
	if strings.TrimSpace(cfg.DownloadUserAgent) == "" || strings.ContainsAny(cfg.DownloadUserAgent, "\r\n\x00") {
		return nil, errors.New("download_user_agent must identify MP and provide a real administrator contact")
	}
	if !identifier.MatchString(cfg.NodeID) {
		return nil, errors.New("invalid node ID")
	}
	u, err := url.Parse(cfg.PanelURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("panel URL must be HTTPS without userinfo")
	}
	cert, err := tls.LoadX509KeyPair(cfg.Certificate, cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("load agent identity: %w", err)
	}
	ca, err := os.ReadFile(cfg.CA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid panel CA")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/mp-agent"
	}
	if !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("data_dir must be absolute")
	}
	if cfg.BindAddress == "" {
		cfg.BindAddress = "0.0.0.0"
	}
	bindIP := net.ParseIP(cfg.BindAddress)
	if bindIP == nil {
		return nil, errors.New("bind_address must be an IP address")
	}
	cfg.BindAddress = bindIP.String()
	if cfg.PollSeconds < 1 {
		cfg.PollSeconds = 5
	}
	for _, sub := range []string{"servers", "backups", "specs", "journal"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, sub), 0700); err != nil {
			return nil, err
		}
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 35 * time.Second}
	// Public vendor downloads deliberately do not carry the node's client certificate.
	public := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many download redirects")
		}
		if req.URL.Scheme != "https" || req.URL.User != nil {
			return errors.New("download redirects must use HTTPS")
		}
		return nil
	}}
	a := &Agent{cfg: cfg, client: &http.Client{Transport: tr, Timeout: 40 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != u.Scheme || req.URL.Host != u.Host {
			return errors.New("panel redirects cannot change origin")
		}
		return nil
	}}, downloadClient: public, locks: map[string]*sync.Mutex{}, running: map[string]bool{}, slots: make(chan struct{}, 4), readSlots: make(chan struct{}, 4)}
	if err := a.recoverJournal(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) Run(ctx context.Context) error {
	go a.heartbeatLoop(ctx)
	go a.resultLoop(ctx)
	for ctx.Err() == nil {
		var c model.Command
		status, err := a.request(ctx, http.MethodGet, "/agent/v1/commands?wait=20", nil, &c)
		if err != nil {
			log.Printf("command poll: %v", err)
			if !pause(ctx, time.Duration(a.cfg.PollSeconds)*time.Second) {
				break
			}
			continue
		}
		if status == http.StatusNoContent {
			continue
		}
		if err := a.accept(ctx, c); err != nil {
			log.Printf("accept command: %v", err)
			pause(ctx, time.Second)
		}
	}
	a.workers.Wait()
	return ctx.Err()
}

func (a *Agent) request(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.cfg.PanelURL, "/")+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return resp.StatusCode, fmt.Errorf("panel HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		var reader io.Reader = io.LimitReader(resp.Body, 16<<20)
		if method == http.MethodGet && path == "/agent/v1/commands?wait=20" {
			// A folder upload can contain a large authenticated command manifest.
			reader = resp.Body
		}
		err = json.NewDecoder(reader).Decode(out)
	}
	return resp.StatusCode, err
}

func (a *Agent) accept(ctx context.Context, c model.Command) error {
	if !identifier.MatchString(c.ID) || c.NodeID != a.cfg.NodeID {
		return errors.New("command identity mismatch")
	}
	path := filepath.Join(a.cfg.DataDir, "journal", c.ID+".json")
	a.mu.Lock()
	if a.running[c.ID] {
		a.mu.Unlock()
		return nil
	}
	if b, err := os.ReadFile(path); err == nil {
		var e journalEntry
		if err = json.Unmarshal(b, &e); err != nil {
			a.mu.Unlock()
			return err
		}
		a.mu.Unlock()
		if e.Result != nil {
			_, err = a.request(ctx, http.MethodPost, "/agent/v1/results", e.Result, nil)
			return err
		}
		return nil
	} else if !os.IsNotExist(err) {
		a.mu.Unlock()
		return err
	}
	if err := atomicJSON(path, journalEntry{Command: c, Status: "accepted"}, 0600); err != nil {
		a.mu.Unlock()
		return err
	}
	a.running[c.ID] = true
	a.workers.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.workers.Done()
		defer func() { a.mu.Lock(); delete(a.running, c.ID); a.mu.Unlock() }()
		workSlots := a.slots
		switch c.Type {
		case "logs", "console", "file.list", "file.read", "properties.read":
			workSlots = a.readSlots
		}
		select {
		case workSlots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-workSlots }()
		// Reads use os.Root; logs and console address an owned container.
		// Keep these usable during serialized installation and readiness waits.
		switch c.Type {
		case "logs", "console", "file.list", "file.read", "properties.read":
		default:
			lock := a.serverLock(c.ServerID)
			lock.Lock()
			defer lock.Unlock()
		}
		result := model.CommandResult{CommandID: c.ID, NodeID: a.cfg.NodeID}
		deadline, err := time.Parse(time.RFC3339, c.Deadline)
		if err != nil {
			result.Error = "invalid command deadline"
		} else {
			commandCtx, cancel := context.WithDeadline(ctx, deadline)
			output, logs, execErr := a.execute(commandCtx, c)
			cancel()
			result.Logs = boundedLines(logs, 256<<10)
			if output != nil {
				result.Output, _ = json.Marshal(output)
			}
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Success = true
			}
		}
		if err := atomicJSON(path, journalEntry{Command: c, Status: "completed", Result: &result}, 0600); err != nil {
			log.Printf("persist result %s: %v", c.ID, err)
			return
		}
		if _, err := a.request(ctx, http.MethodPost, "/agent/v1/results", result, nil); err != nil {
			log.Printf("result delivery %s: %v", c.ID, err)
		}
	}()
	return nil
}

func (a *Agent) serverLock(id string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.locks[id] == nil {
		a.locks[id] = &sync.Mutex{}
	}
	return a.locks[id]
}

func (a *Agent) recoverJournal() error {
	entries, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "journal"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(a.cfg.DataDir, "journal", entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var e journalEntry
		if err = json.Unmarshal(b, &e); err != nil {
			return err
		}
		if e.Result == nil {
			e.Status = "completed"
			e.Result = &model.CommandResult{CommandID: e.Command.ID, NodeID: a.cfg.NodeID, Error: "Agent restarted after accepting this command; execution may have completed. Inspect state before retrying with a new command."}
			if err = atomicJSON(path, e, 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Agent) resultLoop(ctx context.Context) {
	for ctx.Err() == nil {
		entries, _ := os.ReadDir(filepath.Join(a.cfg.DataDir, "journal"))
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(a.cfg.DataDir, "journal", entry.Name())
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var e journalEntry
			if json.Unmarshal(b, &e) != nil || e.Result == nil || e.Status == "delivered" {
				continue
			}
			if _, err = a.request(ctx, http.MethodPost, "/agent/v1/results", e.Result, nil); err == nil {
				e.Status = "delivered"
				_ = atomicJSON(path, e, 0600)
			}
		}
		if !pause(ctx, 5*time.Second) {
			return
		}
	}
}

func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func atomicJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, b, mode)
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mp-write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
