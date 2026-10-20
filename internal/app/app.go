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
	"next-game-crush/internal/profileoverlay"
	"next-game-crush/internal/recommendations"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	syncservice "next-game-crush/internal/sync"
)

type App struct {
	server            *http.Server
	store             *store.Store
	queue             *jobs.Queue
	searchRepo        *catalog.SearchRepository
	searchScan        *catalog.SearchScanService
	aiService         *ai.Service
	interactiveWorker interface{ Run(context.Context) error }
	catalogWorker     interface{ Run(context.Context) error }
	scheduler         interface{ Run(context.Context) error }
	catalogRestart    chan chan error
	serve             func() error
	jobsDisabled      bool
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
	steamConfig := steam.HTTPConfig{APIKey: cfg.SteamWebAPIKey, WebAPIBaseURL: cfg.SteamWebAPIBaseURL, StoreBaseURL: cfg.SteamStoreBaseURL, StoreServiceBaseURL: cfg.SteamStoreServiceBaseURL}
	steamClient, err := steam.NewClient(steamConfig)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure Steam client: %w", err)
	}
	hostThrottle, err := catalog.NewPersistentHostThrottle(db, cfg.CatalogDiscovery.WorkerConcurrency)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure catalog throttle: %w", err)
	}
	steamConfig.HostThrottle = hostThrottle
	catalogSteamClient, err := steam.NewClient(steamConfig)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure catalog Steam client: %w", err)
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
	catalogService.SetDeckRefreshClient(catalogSteamClient)
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
	interactiveWorker := jobs.NewWorker(queue, jobs.LaneInteractive, workerID+":interactive", jobs.WithLogger(eventLogger), jobs.WithConcurrency(cfg.WorkerConcurrency))
	catalogWorker := jobs.NewWorker(queue, jobs.LaneCatalog, workerID+":catalog", jobs.WithLogger(eventLogger),
		jobs.WithConcurrency(cfg.CatalogDiscovery.WorkerConcurrency), jobs.WithRetrySchedule(time.Minute, 5*time.Minute, 30*time.Minute), jobs.WithRetryAfterCap(24*time.Hour))
	if err := syncService.RegisterHandlers(interactiveWorker); err != nil {
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
	if err := completionService.RegisterHandlers(interactiveWorker); err != nil {
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
	aiService.SetLogger(eventLogger)
	if err := aiService.RegisterHandlers(interactiveWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register AI handlers: %w", err)
	}
	syncService.SetAIJobPlanner(aiService)
	discoveryRepo := catalog.NewDiscoveryRepository(db)
	discoveryService, err := catalog.NewDiscoveryService(discoveryRepo, catalogSteamClient, queue, cfg.CatalogDiscovery, eventLogger)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure catalog discovery: %w", err)
	}
	discoveryService.SetSearchEnrichmentPlanner(aiService)
	aiService.SetCatalogChangedHook(discoveryService.MarkCatalogChanged)
	searchRepo := catalog.NewSearchRepository(db)
	searchScan, err := catalog.NewSearchScanService(searchRepo, catalogSteamClient, queue, cfg.CatalogDiscovery, nil)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure catalog search: %w", err)
	}
	searchScheduler, err := catalog.NewSearchScheduler(searchRepo, queue, cfg.CatalogDiscovery, nil)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure catalog scheduler: %w", err)
	}
	// These explicit setters are the production activation points for durable
	// page qualification and the sole catalog-AI execution budget.
	searchScan.SetSearchQualificationProducer(searchRepo)
	aiService.SetCatalogFeatureAdmission(discoveryService)
	cutover, err := discoveryRepo.CutOverLegacyDiscovery(setupCtx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("cut over legacy catalog discovery: %w", err)
	}
	legacyRemaining, err := discoveryRepo.CountNonterminalLegacyJobs(setupCtx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("verify legacy catalog cutover: %w", err)
	}
	if err := validateLegacyCutoverDrain(legacyRemaining, cutover); err != nil {
		db.Close()
		return nil, err
	}
	if err := aiService.RegisterCatalogHandlers(catalogWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register catalog AI handlers: %w", err)
	}
	if err := discoveryService.RegisterSearchHandlers(catalogWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register search qualification handlers: %w", err)
	}
	if err := searchScan.RegisterHandlers(catalogWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register catalog search handlers: %w", err)
	}
	if err := catalogService.RegisterDeckRefreshHandlers(catalogWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register Deck refresh handler: %w", err)
	}
	syncService.SetDiscoveryNudger(searchScheduler)
	syncService.SetSearchCandidateRefresher(searchRepo, cfg.CatalogDiscovery)
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
	profileService.SetLogger(eventLogger)
	if err := profileService.RegisterHandlers(interactiveWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register profile handlers: %w", err)
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
	preferenceService.SetLogger(eventLogger)
	// Register the overlay handler before any user workflow that can enqueue it.
	if err := preferenceService.RegisterHandlers(interactiveWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register preference handlers: %w", err)
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
	recommendationService, err := recommendations.NewService(recommendationRepository, profileService, openAIClient, eligibilityRepository, sharedClock, preferenceService)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("configure recommendation service: %w", err)
	}
	recommendationService.SetQueue(queue)
	recommendationService.SetLogger(eventLogger)
	recommendationService.SetExplanationCache(recommendationRepository, cfg.OpenAIModel)
	recommendationService.SetOverviewSources(recommendationRepository, discoveryService)
	if err := recommendationService.RegisterHandlers(interactiveWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register recommendation handlers: %w", err)
	}
	// The rollout coordinator is registered only after every required child
	// stage it can enqueue is available on the interactive worker.
	if err := profileoverlay.RegisterBoundaryBackfill(interactiveWorker, queue); err != nil {
		db.Close()
		return nil, fmt.Errorf("register profile overlay boundary backfill: %w", err)
	}
	recommendationService.SetDeckRefreshPlanner(catalogService)
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
	if err := feedbackService.RegisterHandlers(interactiveWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register feedback handlers: %w", err)
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
	if err := notificationService.RegisterHandlers(interactiveWorker); err != nil {
		db.Close()
		return nil, fmt.Errorf("register notification handlers: %w", err)
	}
	if err := profileoverlay.EnsureBoundaryBackfill(setupCtx, db, queue); err != nil {
		db.Close()
		return nil, fmt.Errorf("schedule profile overlay boundary backfill: %w", err)
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
	dataService.SetSearchDiagnosticsReader(catalog.NewSearchDiagnosticsReader(db, catalog.SearchDiagnosticsConfig{
		Enabled: cfg.CatalogDiscovery.Enabled, StoreBudgetPerHour: cfg.CatalogDiscovery.AppDetailsHourlyBudget,
		AIBudgetPerHour: cfg.CatalogDiscovery.AIHourlyBudget, StaleAfter: 48 * time.Hour,
	}))
	deps.Data = dataService
	application := &App{store: db, queue: queue, searchRepo: searchRepo, searchScan: searchScan, aiService: aiService,
		interactiveWorker: interactiveWorker, catalogWorker: catalogWorker, scheduler: searchScheduler, jobsDisabled: !cfg.JobsEnabled, server: &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           httpapi.NewRouter(deps),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}, catalogRestart: make(chan chan error)}
	if cfg.TestHarness {
		deps.TestCatalog = &catalogHarness{store: db, scheduler: searchScheduler, restart: application.restartCatalogRuntime}
		application.server.Handler = httpapi.NewRouter(deps)
	}
	return application, nil
}

func validateLegacyCutoverDrain(remaining int64, cutover catalog.LegacyCutoverResult) error {
	if remaining < 0 || cutover.RunningFenced < 0 || remaining > cutover.RunningFenced {
		return errors.New("verify legacy catalog cutover: unexpected nonterminal work")
	}
	return nil
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
	if app.store != nil {
		defer app.store.Close()
	}
	type componentResult struct {
		name string
		err  error
	}
	type component struct {
		name string
		run  func() error
	}
	components := []component{{"http server", app.listenAndServe}}
	if !app.jobsDisabled {
		components = append(components,
			component{"interactive job worker", func() error { return app.interactiveWorker.Run(ctx) }},
			component{"catalog runtime", func() error { return app.runCatalogRuntime(ctx) }},
		)
	}
	results := make(chan componentResult, len(components))
	for _, current := range components {
		go func() { results <- componentResult{current.name, current.run()} }()
	}
	var first componentResult
	select {
	case first = <-results:
	case <-ctx.Done():
		first = componentResult{name: "context", err: nil}
	}
	cancel()
	shutdownErr := app.shutdownHTTP()
	remaining := len(components)
	if first.name != "context" {
		remaining--
	}
	for range remaining {
		result := <-results
		if (first.err == nil || errors.Is(first.err, http.ErrServerClosed)) && result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
			first = result
		}
	}
	if first.err != nil && !errors.Is(first.err, http.ErrServerClosed) {
		return fmt.Errorf("run %s: %w", first.name, first.err)
	}
	return shutdownErr
}

func (app *App) listenAndServe() error {
	if app.serve != nil {
		return app.serve()
	}
	return app.server.ListenAndServe()
}

func (app *App) shutdownHTTP() error {
	if app.server == nil {
		return nil
	}
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
