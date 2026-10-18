package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/completion"
	"next-game-crush/internal/config"
	"next-game-crush/internal/feedback"
	"next-game-crush/internal/httpapi"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/notifications"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/preferences"
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
	rawLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	eventLogger := observability.New(rawLogger)
	deps := httpapi.Dependencies{Logger: rawLogger, Observability: eventLogger}
	if _, err := os.Stat("web/dist/index.html"); err == nil {
		deps.Frontend = os.DirFS("web/dist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open frontend build: %w", err)
	}
	openCtx, cancelOpen := context.WithTimeout(context.Background(), 90*time.Second)
	err := waitForDatabase(openCtx, time.Second, func(ctx context.Context) error {
		connection, connectErr := pgx.Connect(ctx, cfg.DatabaseURL)
		if connectErr != nil {
			return connectErr
		}
		return connection.Close(ctx)
	})
	if err != nil {
		cancelOpen()
		return nil, fmt.Errorf("wait for persistence store: %w", err)
	}
	db, err := store.Open(openCtx, cfg.DatabaseURL)
	cancelOpen()
	if err != nil {
		return nil, fmt.Errorf("open persistence store: %w", err)
	}
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelSetup()
	workerID, err := workerIdentity()
	if err != nil {
		db.Close()
		return nil, err
	}
	queue := jobs.NewQueue(db)
	steamClient, err := steam.NewClient(steam.HTTPConfig{APIKey: cfg.SteamWebAPIKey, WebAPIBaseURL: cfg.SteamWebAPIBaseURL, StoreBaseURL: cfg.SteamStoreBaseURL, StoreServiceBaseURL: cfg.SteamStoreServiceBaseURL})
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
	if err := catalogService.Bootstrap(setupCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrap catalog: %w", err)
	}
	if err := catalogService.CleanupLegacyWishlistProjection(setupCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("clean legacy catalog projection: %w", err)
	}
	syncService, err := syncservice.NewService(db, queue, steamClient, catalogService, syncservice.WithLogger(eventLogger))
	if err != nil {
		db.Close()
		return nil, err
	}
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, workerID, jobs.WithLogger(eventLogger), jobs.WithConcurrency(cfg.WorkerConcurrency))
	if err := syncService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register synchronization handlers: %w", err)
	}
	openAIClient, err := ai.NewOpenAI(ai.OpenAIConfig{
		APIKey: cfg.OpenAIAPIKey, Model: cfg.OpenAIModel,
		EmbeddingModel: cfg.OpenAIEmbeddingModel,
		BaseURL:        cfg.OpenAIBaseURL,
		HTTPClient:     &http.Client{Timeout: 60 * time.Second},
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure OpenAI client: %w", err)
	}
	completionRepository, err := completion.NewSQLRepository(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure completion repository: %w", err)
	}
	completionService, err := completion.NewService(completionRepository, openAIClient, cfg.OpenAIModel, queue)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure completion service: %w", err)
	}
	completionService.SetClassificationPublishedHook(completionService.EnqueueProjectionJobsForGame)
	if err := completionService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register completion handlers: %w", err)
	}
	syncService.SetAchievementSnapshotRecorder(completionService)
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
	discoveryService, err := catalog.NewDiscoveryService(catalog.NewDiscoveryRepository(db), steamClient, steamClient, queue, cfg.CatalogDiscovery, eventLogger)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure catalog discovery: %w", err)
	}
	discoveryService.SetEnrichmentPlanner(aiService)
	aiService.SetCatalogChangedHook(discoveryService.MarkCatalogChanged)
	if err := discoveryService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register catalog discovery handlers: %w", err)
	}
	if err := discoveryService.EnsureScheduled(setupCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("schedule catalog discovery: %w", err)
	}
	syncService.SetDiscoveryNudger(discoveryService)
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
	if err := recommendationRepository.SetWindowDays(cfg.CatalogDiscovery.LookbackDays, cfg.CatalogDiscovery.HorizonDays); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure recommendation window: %w", err)
	}
	eligibilityRepository := catalog.NewEligibilityRepository(db)
	if err := eligibilityRepository.SetWindowDays(cfg.CatalogDiscovery.LookbackDays, cfg.CatalogDiscovery.HorizonDays); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure current eligibility: %w", err)
	}
	sharedClock := func() time.Time { return time.Now().UTC() }
	recommendationService, err := recommendations.NewService(recommendationRepository, profileService, openAIClient, eligibilityRepository, sharedClock)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure recommendation service: %w", err)
	}
	recommendationService.SetQueue(queue)
	recommendationService.SetExplanationCache(recommendationRepository, cfg.OpenAIModel)
	recommendationService.SetOverviewSources(recommendationRepository, discoveryService)
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
	preferenceRepository, err := preferences.NewSQLRepository(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure preference repository: %w", err)
	}
	preferenceService, err := preferences.NewService(preferenceRepository, queue)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure preference service: %w", err)
	}
	if err := preferenceService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register preference handlers: %w", err)
	}
	notificationRepository, err := notifications.NewSQLRepository(db, eligibilityRepository, sharedClock)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure notification repository: %w", err)
	}
	smtpSender, err := notifications.NewSMTPSender(cfg.SMTPURL)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure SMTP sender: %w", err)
	}
	actionSigner, err := notifications.NewSigner([]byte(cfg.EmailActionSecret), "recommendation-feedback")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure email actions: %w", err)
	}
	notificationService, err := notifications.NewService(notificationRepository, queue, smtpSender, actionSigner, cfg.AppBaseURL)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure notifications: %w", err)
	}
	if err := notificationService.RegisterHandlers(worker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register notification handlers: %w", err)
	}
	syncService.EnableRankingJobs()
	authService, err := auth.New(auth.Config{AppBaseURL: cfg.AppBaseURL, AllowedSteamID: cfg.AllowedSteamID, SessionSecret: cfg.SessionSecret, FixtureAuth: cfg.FixtureAuth, DemoMode: cfg.DemoMode}, auth.NewSQLRepository(db), nil)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure authentication: %w", err)
	}
	deps.Auth = authService
	deps.DemoEnabled = cfg.DemoMode
	deps.Sync = syncService
	deps.Identity = syncService
	deps.Profile = profileService
	deps.Preferences = preferenceService
	deps.Recommendations = recommendationService
	deps.Feedback = feedbackService
	deps.Digests = notificationService
	dataService := httpapi.NewSQLDataService(db, cfg.OpenAI.Enabled, strings.TrimSpace(cfg.SMTPURL) != "")
	dataService.SetDiscoveryDiagnosticsReader(discoveryService)
	deps.Data = dataService
	return &App{store: db, worker: worker, server: &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}}, nil
}

func waitForDatabase(
	ctx context.Context,
	retryInterval time.Duration,
	connect func(context.Context) error,
) error {
	for {
		err := connect(ctx)
		if err == nil {
			return nil
		}
		if !retryableDatabaseConnectError(err) {
			return err
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf("database availability timeout: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func retryableDatabaseConnectError(err error) bool {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Code == "57P03"
	}
	var connectError *pgconn.ConnectError
	return errors.As(err, &connectError) || pgconn.SafeToRetry(err)
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
