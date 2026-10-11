package main

import (
	"context"
	"github.com/come0th3r/mp-panel/internal/config"
	"github.com/come0th3r/mp-panel/internal/store"
	"github.com/come0th3r/mp-panel/internal/worker"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
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
	log.Print("MP bootstrap worker started")
	if err = worker.Run(ctx, s, c); err != nil {
		log.Fatal(err)
	}
}
