package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/come0th3r/mp-panel/internal/agent"
	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/version"
)

func main() {
	config := flag.String("config", "/etc/mp-agent/config.json", "Agent configuration file")
	showVersion := flag.Bool("version", false, "Print agent version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Current)
		return
	}
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
