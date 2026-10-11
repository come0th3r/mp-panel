package main

import (
	"context"
	"errors"
	"github.com/come0th3r/mp-panel/internal/api"
	"github.com/come0th3r/mp-panel/internal/config"
	"github.com/come0th3r/mp-panel/internal/store"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		client := http.Client{Timeout: 3 * time.Second}
		address := config.Env("MP_LISTEN", ":8080")
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			os.Exit(1)
		}
		resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	s, err := store.Open(ctx, c.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	a, err := api.New(s, c)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = a.SetupToken(); err != nil {
		log.Fatal(err)
	}
	log.Printf("First-owner setup token file: %s", filepath.Join(c.DataDir, "keys", "setup.token"))
	agentURL, _ := url.Parse(c.AgentURL)
	tlsConfig, err := a.CA.TLSConfig(agentURL.Hostname())
	if err != nil {
		log.Fatal(err)
	}
	web := &http.Server{Addr: c.Listen, Handler: a.WebHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	agent := &http.Server{Addr: c.AgentListen, Handler: a.AgentHandler(), TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	failures := make(chan error, 2)
	go func() { failures <- web.ListenAndServe() }()
	go func() { failures <- agent.ListenAndServeTLS("", "") }()
	go a.RunMaintenance(ctx)
	go a.RunGateway(ctx)
	log.Printf("MP web listening at %s; agent mTLS listening at %s", c.Listen, c.AgentListen)
	select {
	case <-ctx.Done():
	case err = <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP listener stopped: %v", err)
		}
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := web.Shutdown(shutdownCtx); err != nil {
		log.Printf("web shutdown: %v", err)
	}
	if err := agent.Shutdown(shutdownCtx); err != nil {
		log.Printf("agent shutdown: %v", err)
	}
}
