package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"next-game-crush/internal/app"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/config"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 0 && (len(args) != 2 || args[0] != "catalog" || args[1] != "refresh") {
		return errors.New("usage: next-game-crush [catalog refresh]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return dispatch(args, cfg, serveApplication, enqueueCatalogRefresh)
}

func dispatch(args []string, cfg config.Config, serve, enqueue func(config.Config) error) error {
	if len(args) == 0 {
		return serve(cfg)
	}
	if len(args) == 2 && args[0] == "catalog" && args[1] == "refresh" {
		return enqueue(cfg)
	}
	return errors.New("usage: next-game-crush [catalog refresh]")
}

func enqueueCatalogRefresh(cfg config.Config) error {
	if err := cfg.CatalogDiscovery.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("open persistence store for catalog refresh failed")
	}
	defer db.Close()
	scheduler, err := catalog.NewSearchScheduler(catalog.NewSearchRepository(db), jobs.NewQueue(db), cfg.CatalogDiscovery, time.Now)
	if err != nil {
		return err
	}
	if _, err := scheduler.EnqueueManual(ctx, time.Now().UTC()); err != nil {
		return errors.New("enqueue catalog refresh failed")
	}
	return nil
}

func serveApplication(cfg config.Config) error {
	application, err := app.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("Next Game Crush listening on %s", cfg.HTTPAddr)
	return application.Run(ctx)
}
