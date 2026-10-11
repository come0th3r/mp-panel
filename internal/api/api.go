package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/come0th3r/mp-panel/internal/catalog"
	"github.com/come0th3r/mp-panel/internal/config"
	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/security"
	"github.com/come0th3r/mp-panel/internal/store"
	"github.com/gorilla/websocket"
)

type API struct {
	Store   *store.Store
	Config  config.Config
	Vault   *security.Vault
	CA      *security.CA
	Catalog *catalog.Catalog
	mu      sync.Mutex
	rates   map[string]*rateEntry
}
type rateEntry struct {
	start time.Time
	count int
}
type contextKey string

const userKey contextKey = "user"
const csrfKey contextKey = "csrf"

func New(s *store.Store, c config.Config) (*API, error) {
	v, e := security.OpenVault(c.DataDir)
	if e != nil {
		return nil, e
	}
	ca, e := security.OpenCA(c.DataDir)
	if e != nil {
		return nil, e
	}
	return &API{Store: s, Config: c, Vault: v, CA: ca, Catalog: catalog.New(c.UserAgent), rates: map[string]*rateEntry{}}, nil
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	write(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, 16<<20)
}
func decodeAgent(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, 16<<20)
}
func decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "Неверный формат запроса")
		return false
	}
	var rest any
	if d.Decode(&rest) != io.EOF {
		fail(w, 400, "Ожидается один JSON объект")
		return false
	}
	return true
}
func actor(r *http.Request) model.User { u, _ := r.Context().Value(userKey).(model.User); return u }
func allowed(u model.User, permission string) bool {
	switch permission {
	case "read":
		return u.ID != ""
	case "operate":
		return u.Role == "owner" || u.Role == "admin" || u.Role == "operator"
	case "upload":
		return u.Role == "owner" || u.Role == "admin" || u.Role == "operator"
	case "manage":
		return u.Role == "owner" || u.Role == "admin"
	case "owner":
		return u.Role == "owner"
	}
	return false
}
func (a *API) require(permission string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, csrf, e := a.session(r)
		if e != nil {
			fail(w, 401, "Необходимо войти в панель")
			return
		}
		serverScoped := r.PathValue("id") != "" && strings.HasPrefix(r.URL.Path, "/api/v1/servers/")
		permit := allowed(u, permission) || (serverScoped && permission == "manage" && u.Role == "operator")
		if !permit {
			fail(w, 403, "Недостаточно прав для этого действия")
			return
		}
		if serverScoped {
			if !a.checkServerAccess(w, r, u, r.PathValue("id")) {
				return
			}
		}
		if u.Role == "operator" && r.PathValue("id") != "" && strings.HasPrefix(r.URL.Path, "/api/v1/nodes/") {
			visible, err := a.operatorNodeAccess(r.Context(), u.ID, r.PathValue("id"))
			if err != nil {
				fail(w, 503, "Не удалось проверить доступ к хосту")
				return
			}
			if !visible {
				fail(w, 404, "Хост не найден")
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("X-CSRF-Token") != csrf {
				fail(w, 403, "Сессия формы устарела. Обновите страницу")
				return
			}
		}
		r = r.WithContext(context.WithValue(context.WithValue(r.Context(), userKey, u), csrfKey, csrf))
		h(w, r)
	}
}
func (a *API) audit(ctx context.Context, u model.User, action, id, detail string) {
	_, e := a.Store.Pool.Exec(ctx, `INSERT INTO mp_audit(id,actor_id,actor,action,object_id,detail) VALUES($1,$2,$3,$4,$5,$6)`, security.ID(), u.ID, u.Username, action, id, detail)
	if e != nil {
		log.Printf("audit write failed: %v", e)
	}
}
func (a *API) WebHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := a.Store.Pool.Ping(r.Context()); e != nil {
			fail(w, 503, "database unavailable")
			return
		}
		write(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /api/v1/auth/status", a.authStatus)
	m.HandleFunc("POST /api/v1/auth/setup", a.setup)
	m.HandleFunc("POST /api/v1/auth/login", a.login)
	m.HandleFunc("POST /api/v1/auth/logout", a.require("read", a.logout))
	m.HandleFunc("GET /api/v1/auth/me", a.require("read", func(w http.ResponseWriter, r *http.Request) { write(w, 200, actor(r)) }))
	m.HandleFunc("POST /api/v1/auth/totp/setup", a.require("read", a.totpSetup))
	m.HandleFunc("POST /api/v1/auth/totp/enable", a.require("read", a.totpEnable))
	m.HandleFunc("POST /api/v1/auth/totp/disable", a.require("read", a.totpDisable))
	m.HandleFunc("GET /api/v1/users", a.require("owner", a.users))
	m.HandleFunc("POST /api/v1/users", a.require("owner", a.addUser))
	m.HandleFunc("GET /api/v1/nodes", a.require("read", a.nodes))
	m.HandleFunc("GET /api/v1/nodes/{id}", a.require("read", a.node))
	m.HandleFunc("POST /api/v1/nodes/fingerprint", a.require("owner", a.fingerprint))
	m.HandleFunc("POST /api/v1/nodes/bootstrap", a.require("owner", a.bootstrapNode))
	m.HandleFunc("POST /api/v1/nodes/{id}/bootstrap", a.require("owner", a.bootstrapNode))
	m.HandleFunc("DELETE /api/v1/nodes/{id}", a.require("owner", a.deleteNode))
	m.HandleFunc("GET /api/v1/catalog/cores", a.require("read", func(w http.ResponseWriter, r *http.Request) { write(w, 200, a.Catalog.Cores()) }))
	m.HandleFunc("GET /api/v1/catalog/cores/{core}/versions", a.require("read", a.versions))
	m.HandleFunc("GET /api/v1/catalog/cores/{core}/versions/{version}/builds", a.require("read", a.builds))
	m.HandleFunc("POST /api/v1/catalog/resolve", a.require("read", a.resolve))
	m.HandleFunc("POST /api/v1/catalog/refresh", a.require("owner", func(w http.ResponseWriter, r *http.Request) {
		a.Catalog.Refresh()
		write(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("GET /api/v1/servers", a.require("read", a.servers))
	m.HandleFunc("GET /api/v1/servers/{id}", a.require("read", a.server))
	m.HandleFunc("GET /api/v1/servers/{id}/operators", a.require("owner", a.serverOperators))
	m.HandleFunc("PUT /api/v1/servers/{id}/operators", a.require("owner", a.setServerOperators))
	m.HandleFunc("POST /api/v1/servers", a.require("manage", a.createServer))
	m.HandleFunc("POST /api/v1/servers/{id}/actions", a.require("operate", a.serverAction))
	m.HandleFunc("POST /api/v1/servers/{id}/installations", a.require("manage", a.installServer))
	m.HandleFunc("GET /api/v1/servers/{id}/route", a.require("read", a.getRoute))
	m.HandleFunc("PUT /api/v1/servers/{id}/route", a.require("manage", a.putRoute))
	m.HandleFunc("DELETE /api/v1/servers/{id}/route", a.require("manage", a.removeRoute))
	m.HandleFunc("GET /api/v1/external-routes", a.require("manage", a.externalRoutes))
	m.HandleFunc("POST /api/v1/external-routes", a.require("manage", a.saveExternalRoute))
	m.HandleFunc("PUT /api/v1/external-routes/{id}", a.require("manage", a.saveExternalRoute))
	m.HandleFunc("DELETE /api/v1/external-routes/{id}", a.require("manage", a.removeExternalRoute))
	m.HandleFunc("GET /api/v1/jobs", a.require("read", a.jobs))
	m.HandleFunc("GET /api/v1/jobs/{id}", a.require("read", a.job))
	m.HandleFunc("GET /api/v1/nodes/{id}/metrics", a.require("read", a.nodeMetrics))
	m.HandleFunc("GET /api/v1/servers/{id}/metrics", a.require("read", a.serverMetrics))
	m.HandleFunc("POST /api/v1/uploads", a.require("upload", a.upload))
	m.HandleFunc("GET /api/v1/servers/{id}/files", a.require("read", a.files))
	m.HandleFunc("GET /api/v1/servers/{id}/files/content", a.require("read", a.fileContent))
	m.HandleFunc("PUT /api/v1/servers/{id}/files/content", a.require("manage", a.writeFile))
	m.HandleFunc("POST /api/v1/servers/{id}/files/upload", a.require("manage", a.fileUpload))
	m.HandleFunc("POST /api/v1/servers/{id}/files/directory", a.require("manage", a.mkdir))
	m.HandleFunc("POST /api/v1/servers/{id}/files/rename", a.require("manage", a.rename))
	m.HandleFunc("DELETE /api/v1/servers/{id}/files", a.require("manage", a.deleteFile))
	m.HandleFunc("GET /api/v1/servers/{id}/properties", a.require("read", a.properties))
	m.HandleFunc("PUT /api/v1/servers/{id}/properties", a.require("manage", a.writeProperties))
	m.HandleFunc("GET /api/v1/servers/{id}/extensions", a.require("read", a.extensions))
	m.HandleFunc("POST /api/v1/servers/{id}/extensions", a.require("manage", a.addExtension))
	m.HandleFunc("POST /api/v1/servers/{id}/extensions/toggle", a.require("manage", a.toggleExtension))
	m.HandleFunc("GET /api/v1/servers/{id}/logs", a.require("read", a.logs))
	m.HandleFunc("POST /api/v1/servers/{id}/console", a.require("operate", a.console))
	m.HandleFunc("GET /api/v1/servers/{id}/console", a.require("read", a.consoleWS))
	m.HandleFunc("GET /api/v1/servers/{id}/backups", a.require("read", a.backups))
	m.HandleFunc("POST /api/v1/servers/{id}/backups", a.require("manage", a.backup))
	m.HandleFunc("POST /api/v1/servers/{id}/restores", a.require("manage", a.restore))
	m.HandleFunc("GET /api/v1/audit", a.require("owner", a.auditList))
	m.HandleFunc("GET /api/v1/settings", a.require("read", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"agent_public_url": a.Config.AgentURL, "default_java": 21, "version": "0.1.0", "gateway_enabled": a.Config.GatewayURL() != "", "gateway_provider": a.Config.GatewayProvider(), "game_domain": a.Config.GameDomain})
	}))
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "API endpoint не найден") })
	fs := http.FileServer(http.Dir(a.Config.WebDir))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			fail(w, 405, "Method not allowed")
			return
		}
		path := filepath.Join(a.Config.WebDir, filepath.Clean("/"+r.URL.Path))
		if _, e := os.Stat(path); e != nil {
			http.ServeFile(w, r, filepath.Join(a.Config.WebDir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
	return a.headers(m)
}
func (a *API) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		defer func() {
			if e := recover(); e != nil {
				log.Printf("handler panic: %v", e)
				fail(w, 500, "Внутренняя ошибка панели")
			}
		}()
		if r.Method != "GET" && r.Method != "HEAD" {
			if origin := r.Header.Get("Origin"); origin != "" && !a.sameOrigin(r, origin) {
				fail(w, 403, "Недопустимый источник запроса")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (a *API) sameOrigin(r *http.Request, origin string) bool {
	u, e := url.Parse(origin)
	if e != nil {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	p, _ := url.Parse(a.Config.PublicURL)
	return p != nil && u.Scheme == p.Scheme && u.Host == p.Host
}
func (a *API) limited(r *http.Request) bool {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.rates) > 10000 {
		for k, v := range a.rates {
			if now.Sub(v.start) > time.Minute {
				delete(a.rates, k)
			}
		}
	}
	x := a.rates[ip]
	if x == nil || now.Sub(x.start) > time.Minute {
		x = &rateEntry{start: now}
		a.rates[ip] = x
	}
	x.count++
	return x.count > 12
}

var upgrader = websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096}
var errBusy = errors.New("С сервером уже выполняется операция. Дождитесь завершения задания")
