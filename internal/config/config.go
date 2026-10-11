package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	DatabaseURL, Listen, AgentListen, AgentURL, PublicURL, DataDir, WebDir, AgentBinary, UserAgent, GateURL, MCRouterURL, GameDomain string
	CookieSecure                                                                                                                     bool
}

func Env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func Load() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), Listen: Env("MP_LISTEN", ":8080"), AgentListen: Env("MP_AGENT_LISTEN", ":8443"), AgentURL: Env("MP_AGENT_PUBLIC_URL", "https://localhost:8443"), PublicURL: Env("MP_PUBLIC_URL", "http://localhost:8080"), DataDir: Env("MP_DATA_DIR", "data"), WebDir: Env("MP_WEB_DIR", "apps/web/dist"), AgentBinary: Env("MP_AGENT_BINARY", "bin/mp-agent-linux-amd64"), UserAgent: os.Getenv("MP_DOWNLOAD_USER_AGENT"), GateURL: os.Getenv("MP_GATE_API_URL"), GameDomain: strings.ToLower(strings.TrimSpace(os.Getenv("MP_GAME_DOMAIN"))), CookieSecure: Env("MP_COOKIE_SECURE", "false") == "true"}
	c.MCRouterURL = strings.TrimRight(strings.TrimSpace(os.Getenv("MP_MC_ROUTER_API_URL")), "/")
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if strings.TrimSpace(c.UserAgent) == "" || strings.ContainsAny(c.UserAgent, "\r\n") {
		return c, fmt.Errorf("MP_DOWNLOAD_USER_AGENT must identify this installation with a real contact URL or email")
	}
	if c.GateURL != "" && c.MCRouterURL != "" {
		return c, fmt.Errorf("configure only one gateway: MP_GATE_API_URL or MP_MC_ROUTER_API_URL")
	}
	if c.GatewayURL() != "" {
		g, err := url.Parse(c.GatewayURL())
		if err != nil || (g.Scheme != "http" && g.Scheme != "https") || g.Hostname() == "" || g.User != nil || g.RawQuery != "" || g.Fragment != "" || strings.Trim(g.Path, "/") != "" {
			return c, fmt.Errorf("gateway API URL must contain an HTTP(S) scheme, host and port without a path")
		}
	}
	u, e := url.Parse(c.AgentURL)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" {
		return c, fmt.Errorf("MP_AGENT_PUBLIC_URL must be an HTTPS URL reachable by game hosts")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return c, fmt.Errorf("agent URL must contain only scheme, host and port")
	}
	c.DataDir, e = filepath.Abs(c.DataDir)
	p, publicErr := url.Parse(c.PublicURL)
	if publicErr != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Hostname() == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" || strings.Trim(p.Path, "/") != "" {
		return c, fmt.Errorf("MP_PUBLIC_URL must contain an HTTP(S) scheme, host and port")
	}
	if e != nil {
		return c, e
	}
	if e = os.MkdirAll(c.DataDir, 0700); e != nil {
		return c, e
	}
	return c, nil
}

func (c Config) GatewayURL() string {
	if c.MCRouterURL != "" {
		return c.MCRouterURL
	}
	return c.GateURL
}

func (c Config) GatewayProvider() string {
	if c.MCRouterURL != "" {
		return "mc-router"
	}
	if c.GateURL != "" {
		return "gate"
	}
	return ""
}
