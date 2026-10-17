package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"next-game-crush/internal/app"
	"next-game-crush/internal/config"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	application, err := app.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("Next Game Crush listening on %s", cfg.HTTPAddr)
	return application.Run(ctx)
}
