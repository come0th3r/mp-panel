package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/come0th3r/mp-panel/internal/agent"
	"github.com/come0th3r/mp-panel/internal/model"
)

func main() {
	config := flag.String("config", "/etc/mp-agent/config.json", "Agent configuration file")
	flag.Parse()
	b, err := os.ReadFile(*config)
	if err != nil {
		log.Fatal(err)
	}
	var cfg model.AgentConfig
	if err = json.Unmarshal(b, &cfg); err != nil {
		log.Fatal(err)
	}
	a, err := agent.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
