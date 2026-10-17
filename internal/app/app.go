package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"next-game-crush/internal/config"
	"next-game-crush/internal/httpapi"
)

type App struct {
	server *http.Server
}

func New(_ config.Config) (*App, error) {
	deps := httpapi.Dependencies{}
	if _, err := os.Stat("web/dist/index.html"); err == nil {
		deps.Frontend = os.DirFS("web/dist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open frontend build: %w", err)
	}
	return &App{server: &http.Server{
		Addr:              ":8080",
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}}, nil
}

func (app *App) Run(ctx context.Context) error {
	serverError := make(chan error, 1)
	go func() { serverError <- app.server.ListenAndServe() }()
	select {
	case err := <-serverError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.server.Shutdown(shutdownCtx); err != nil {
			_ = app.server.Close()
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		return nil
	}
}
