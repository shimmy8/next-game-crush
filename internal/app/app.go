package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"next-game-crush/internal/auth"
	"next-game-crush/internal/config"
	"next-game-crush/internal/httpapi"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
)

type App struct {
	server *http.Server
	store  *store.Store
	worker *jobs.Worker
}

func New(cfg config.Config) (*App, error) {
	deps := httpapi.Dependencies{}
	if _, err := os.Stat("web/dist/index.html"); err == nil {
		deps.Frontend = os.DirFS("web/dist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open frontend build: %w", err)
	}
	openCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := store.Open(openCtx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open persistence store: %w", err)
	}
	workerID, err := workerIdentity()
	if err != nil {
		db.Close()
		return nil, err
	}
	queue := jobs.NewQueue(db)
	providerClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	authService, err := auth.New(auth.Config{AppBaseURL: cfg.AppBaseURL, AllowedSteamID: cfg.AllowedSteamID, SessionSecret: cfg.SessionSecret}, auth.NewSQLRepository(db), auth.NewHTTPProvider(providerClient, "https://steamcommunity.com/openid/"))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure authentication: %w", err)
	}
	deps.Auth = authService
	return &App{store: db, worker: jobs.NewWorker(queue, workerID), server: &http.Server{
		Addr:              ":8080",
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}}, nil
}

func (app *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer app.store.Close()
	serverError := make(chan error, 1)
	go func() { serverError <- app.server.ListenAndServe() }()
	workerError := make(chan error, 1)
	go func() { workerError <- app.worker.Run(ctx) }()
	select {
	case err := <-serverError:
		cancel()
		<-workerError
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-workerError:
		cancel()
		shutdownErr := app.shutdownHTTP()
		<-serverError
		if err != nil {
			return fmt.Errorf("run job worker: %w", err)
		}
		return shutdownErr
	case <-ctx.Done():
		cancel()
		shutdownErr := app.shutdownHTTP()
		serverErr := <-serverError
		workerErr := <-workerError
		if shutdownErr != nil {
			return shutdownErr
		}
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			return serverErr
		}
		return workerErr
	}
}

func (app *App) shutdownHTTP() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.server.Shutdown(shutdownCtx); err != nil {
		_ = app.server.Close()
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	return nil
}

func workerIdentity() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve worker identity: %w", err)
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "", errors.New("resolve worker identity: empty hostname")
	}
	return hostname + ":local", nil
}
