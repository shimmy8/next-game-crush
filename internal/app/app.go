package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"next-game-crush/internal/ai"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/config"
	"next-game-crush/internal/feedback"
	"next-game-crush/internal/httpapi"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	syncservice "next-game-crush/internal/sync"
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
	steamClient, err := steam.NewClient(steam.HTTPConfig{APIKey: cfg.SteamWebAPIKey})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure Steam client: %w", err)
	}
	seed, err := catalog.LoadSeed("catalog_seed.json")
	if err != nil {
		db.Close()
		return nil, err
	}
	catalogService, err := catalog.NewService(db, steamClient, seed)
	if err != nil {
		db.Close()
		return nil, err
	}
	syncService, err := syncservice.NewService(db, queue, steamClient, catalogService)
	if err != nil {
		db.Close()
		return nil, err
	}
	worker := jobs.NewWorker(queue, workerID)
	if err := syncService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register synchronization handlers: %w", err)
	}
	openAIClient, err := ai.NewOpenAI(ai.OpenAIConfig{
		APIKey: cfg.OpenAIAPIKey, Model: cfg.OpenAIModel,
		EmbeddingModel: cfg.OpenAIEmbeddingModel,
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure OpenAI client: %w", err)
	}
	aiService, err := ai.NewService(ai.NewSQLRepository(db), openAIClient, cfg.OpenAIModel, cfg.OpenAIEmbeddingModel)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure AI jobs: %w", err)
	}
	aiService.SetQueue(queue)
	if err := aiService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register AI handlers: %w", err)
	}
	syncService.SetAIJobPlanner(aiService)
	profileRepository, err := profile.NewSQLRepository(db, cfg.OpenAIEmbeddingModel)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure profile repository: %w", err)
	}
	profileService, err := profile.NewService(profileRepository)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure profile service: %w", err)
	}
	profileService.SetQueue(queue)
	if err := profileService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register profile handlers: %w", err)
	}
	recommendationRepository, err := recommendations.NewSQLRepository(db, cfg.OpenAIEmbeddingModel, cfg.OpenAIModel)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure recommendation repository: %w", err)
	}
	recommendationService, err := recommendations.NewService(recommendationRepository, profileService, openAIClient)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure recommendation service: %w", err)
	}
	recommendationService.SetQueue(queue)
	if err := recommendationService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register recommendation handlers: %w", err)
	}
	feedbackRepository, err := feedback.NewSQLRepository(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure feedback repository: %w", err)
	}
	feedbackService, err := feedback.NewService(feedbackRepository, openAIClient, cfg.OpenAIModel, queue)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure feedback service: %w", err)
	}
	if err := feedbackService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register feedback handlers: %w", err)
	}
	syncService.EnableRankingJobs()
	providerClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	authService, err := auth.New(auth.Config{AppBaseURL: cfg.AppBaseURL, AllowedSteamID: cfg.AllowedSteamID, SessionSecret: cfg.SessionSecret}, auth.NewSQLRepository(db), auth.NewHTTPProvider(providerClient, "https://steamcommunity.com/openid/"))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure authentication: %w", err)
	}
	deps.Auth = authService
	deps.Sync = syncService
	deps.Profile = profileService
	deps.Recommendations = recommendationService
	deps.Feedback = feedbackService
	return &App{store: db, worker: worker, server: &http.Server{
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
