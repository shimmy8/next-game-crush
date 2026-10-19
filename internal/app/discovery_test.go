package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
	migrationfiles "next-game-crush/db/migrations"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/config"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
)

func discoveryAppConfig(t *testing.T) config.Config {
	t.Helper()
	t.Chdir("../..")
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Path != "/next_game_crush_test" {
		t.Fatal("requires isolated test database")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := "app_discovery_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	t.Cleanup(func() {
		conn, openErr := pgx.Connect(context.Background(), base)
		if openErr == nil {
			defer conn.Close(context.Background())
			_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	cfg := config.Config{
		AppBaseURL: "http://localhost:8080", AllowedSteamID: "76561197960287930", SteamWebAPIKey: "fixture-key",
		OpenAIModel: "gpt-6-luna", OpenAIEmbeddingModel: "text-embedding-3-small", DatabaseURL: parsed.String(), SMTPURL: "smtp://localhost:1025",
		EmailActionSecret: strings.Repeat("e", 32), SessionSecret: strings.Repeat("s", 32), HTTPAddr: "127.0.0.1:0", WorkerConcurrency: 1,
		CatalogDiscovery: catalog.DefaultDiscoveryConfig(),
	}
	cfg.CatalogDiscovery.Enabled = true
	return cfg
}

func TestAppRegistersHandlersInCorrectLanes(t *testing.T) {
	cfg := discoveryAppConfig(t)
	cfg.WorkerConcurrency = 3
	cfg.CatalogDiscovery.WorkerConcurrency = 4
	application, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	interactive, ok := application.interactiveWorker.(*jobs.Worker)
	if !ok {
		t.Fatal("interactive worker has unexpected type")
	}
	catalogWorker, ok := application.catalogWorker.(*jobs.Worker)
	if !ok {
		t.Fatal("catalog worker has unexpected type")
	}
	if interactive.Concurrency() != 3 || catalogWorker.Concurrency() != 4 {
		t.Fatalf("concurrency interactive=%d catalog=%d", interactive.Concurrency(), catalogWorker.Concurrency())
	}
	for _, kind := range []string{catalog.QualifyJobKind, catalog.TickJobKind, "steam.sync", "recommendations.rebuild"} {
		if !interactive.Handles(kind) || catalogWorker.Handles(kind) {
			t.Fatalf("kind %q must be interactive only", kind)
		}
	}
	for _, kind := range []string{catalog.SearchRunJobKind, catalog.SearchPageJobKind, catalog.SearchQualificationJobKind, catalog.DeckRefreshJobKind} {
		if !catalogWorker.Handles(kind) || interactive.Handles(kind) {
			t.Fatalf("kind %q must be catalog only", kind)
		}
	}
	if !interactive.Handles("ai.features") || !catalogWorker.Handles("ai.features") {
		t.Fatal("ai.features must be registered on both lanes")
	}
	if catalogWorker.Handles("profile.rebuild") {
		t.Fatal("profile handler leaked into catalog lane")
	}
}

func TestAppBootstrapsSeedWithoutSteamDiscovery(t *testing.T) {
	cfg := discoveryAppConfig(t)
	ctx := context.Background()
	for boot := 0; boot < 2; boot++ {
		application, err := New(cfg)
		if err != nil {
			t.Fatalf("boot %d: %v", boot+1, err)
		}
		var seedCount, legacyJobs int
		if err := application.store.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM catalog_entry_sources WHERE source='seed' AND active),
			(SELECT count(*) FROM sync_jobs WHERE kind IN ('catalog.discovery.tick','catalog.discover.incremental','catalog.discover.backfill','catalog.qualify','catalog.rebuild_wakeup') AND status IN ('queued','running','waiting','retryable'))`).Scan(&seedCount, &legacyJobs); err != nil {
			application.store.Close()
			t.Fatal(err)
		}
		application.store.Close()
		if seedCount != 20 || legacyJobs != 0 {
			t.Fatalf("boot %d seed=%d legacy jobs=%d", boot+1, seedCount, legacyJobs)
		}
	}
}

func TestDisabledSearchSchedulesNoNewCatalogWork(t *testing.T) {
	cfg := discoveryAppConfig(t)
	cfg.CatalogDiscovery.Enabled = false
	application, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var work int
	if err := application.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM sync_jobs
		WHERE lane='catalog' OR kind LIKE 'catalog.search.%' OR kind='catalog.deck.refresh'`).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if work != 0 {
		t.Fatalf("disabled scheduler created %d catalog jobs", work)
	}
}

func TestLegacyCutoverStartupGuardAllowsOnlyFencedRunningDrain(t *testing.T) {
	for _, test := range []struct {
		name      string
		remaining int64
		fenced    int64
		wantErr   bool
	}{
		{name: "drained", remaining: 0, fenced: 0},
		{name: "claimed v1 drain", remaining: 2, fenced: 2},
		{name: "unexpected survivor", remaining: 2, fenced: 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateLegacyCutoverDrain(test.remaining, catalog.LegacyCutoverResult{RunningFenced: test.fenced})
			if (err != nil) != test.wantErr {
				t.Fatalf("validateLegacyCutoverDrain() error=%v", err)
			}
		})
	}
}

func TestAppWiresSearchQualificationAndCatalogAIAdmission(t *testing.T) {
	t.Run("published search page produces durable qualification", func(t *testing.T) {
		cfg := discoveryAppConfig(t)
		application, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer application.store.Close()
		ctx := context.Background()
		scheduler := application.scheduler.(*catalog.SearchScheduler)
		run, err := scheduler.EnqueueManual(ctx, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err = application.searchRepo.PrepareRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		release := time.Now().UTC().AddDate(0, 0, 1).Format("2 Jan, 2006")
		page := steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{{
			AppID: 991001, Name: "Wired Candidate", ReleaseText: release,
			Review: steam.ReviewSummary{Band: steam.ReviewUnknown}, Fingerprint: "wired-candidate",
		}}}
		if err = application.searchScan.CommitPage(ctx, run, steam.FeedPopularUpcoming, 0, 0, page); err != nil {
			t.Fatal(err)
		}
		var qualifications int
		if err = application.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'`).Scan(&qualifications); err != nil {
			t.Fatal(err)
		}
		if qualifications != 1 {
			t.Fatalf("published Search page produced %d qualification jobs", qualifications)
		}
	})

	t.Run("saturated AI budget durably defers without upstream call", func(t *testing.T) {
		cfg := discoveryAppConfig(t)
		cfg.OpenAIAPIKey = "fixture-key"
		// Any attempted upstream call would fail and move this job out of the
		// exact durable admission deferral asserted below.
		cfg.OpenAIBaseURL = "http://127.0.0.1:1"
		cfg.CatalogDiscovery.AIHourlyBudget = 1
		application, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer application.store.Close()
		ctx := context.Background()
		now := time.Now().UTC()
		granted, _, err := application.searchRepo.ReserveBudget(ctx, "ai", "fills-hour", 1, now)
		if err != nil || !granted {
			t.Fatalf("fill budget granted=%v err=%v", granted, err)
		}
		gameID := uuid.New()
		details := steam.GameDetails{AppID: 991002, Type: "game", Name: "Deferred Candidate", ShortDescription: "stable evidence",
			ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}}
		snapshot, _ := json.Marshal(details)
		if _, err = application.store.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot,availability)
			VALUES($1,$2,$3,'steam_search',now(),$4,'available')`, gameID, details.AppID, details.Name, snapshot); err != nil {
			t.Fatal(err)
		}
		spec, err := application.aiService.PlanCatalogGameFeatureJob(ctx, gameID, steam.GameContentHash(details), 80)
		if err != nil {
			t.Fatal(err)
		}
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte(spec.Kind+":"+spec.IdempotencyKey)))
		key := fmt.Sprintf("catalog:enrichment:v1:%s:1:%s", gameID, identity)
		spec.IdempotencyKey = key
		spec, err = application.aiService.BindGameFeatureExecution(spec, 1, key)
		if err != nil {
			t.Fatal(err)
		}
		jobID, err := application.queue.Enqueue(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		workerCtx, cancel := context.WithCancel(ctx)
		workerResult := make(chan error, 1)
		go func() { workerResult <- application.catalogWorker.Run(workerCtx) }()
		deadline := time.Now().Add(3 * time.Second)
		var status string
		var attempts, embeddings int
		var availableAt time.Time
		for time.Now().Before(deadline) {
			err = application.store.Pool.QueryRow(ctx, `SELECT status,attempts,available_at,
				(SELECT count(*) FROM sync_jobs WHERE kind='ai.embedding') FROM sync_jobs WHERE id=$1`, jobID).
				Scan(&status, &attempts, &availableAt, &embeddings)
			if err == nil && status == "queued" && availableAt.After(now.Add(30*time.Minute)) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err = <-workerResult; err != nil {
			t.Fatal(err)
		}
		var dead, featureRows int
		if err = application.store.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM sync_jobs WHERE id=$1 AND status='dead'),
			(SELECT count(*) FROM game_features WHERE game_id=$2)`, jobID, gameID).Scan(&dead, &featureRows); err != nil {
			t.Fatal(err)
		}
		nextHour := now.Truncate(time.Hour).Add(time.Hour)
		if status != "queued" || attempts != 0 || availableAt.Before(nextHour) || embeddings != 0 || dead != 0 || featureRows != 0 {
			t.Fatalf("status=%s attempts=%d available=%s embeddings=%d dead=%d features=%d", status, attempts, availableAt, embeddings, dead, featureRows)
		}
	})
}

func TestAppUpgradeRetiresLegacyDiscoveryTicksWithoutSuccessor(t *testing.T) {
	cfg := discoveryAppConfig(t)
	ctx := context.Background()
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if _, err := provider.UpTo(ctx, 16); err != nil {
		t.Fatal(err)
	}
	current := time.Now().UTC().Truncate(time.Hour)
	for _, at := range []time.Time{current, current.Add(time.Hour)} {
		payload, _ := json.Marshal(map[string]any{"version": 1, "scheduled_at": at})
		key := fmt.Sprintf("catalog.discovery.tick:v1:%d", at.Unix())
		if _, err := db.ExecContext(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload,result,available_at)
			VALUES($1,'catalog.discovery.tick',$2,$3,'{"audit":true}',$4)`, uuid.New(), key, payload, at); err != nil {
			t.Fatal(err)
		}
	}
	var original []byte
	if err := db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM sync_jobs j`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	// New applies migration 17 and must coexist with both retired hourly keys.
	for boot := range 2 {
		application, err := New(cfg)
		if err != nil {
			t.Fatalf("upgraded boot %d: %v", boot+1, err)
		}
		var retired, live, future int
		if err := application.store.Pool.QueryRow(ctx, `SELECT
			count(*) FILTER(WHERE lane='catalog' AND status='dead' AND last_error='superseded_catalog_workflow'),
			count(*) FILTER(WHERE lane='interactive' AND status IN ('queued','running','waiting','retryable')),
			count(*) FILTER(WHERE lane='interactive' AND status IN ('queued','running','waiting','retryable') AND available_at>now())
			FROM sync_jobs WHERE kind='catalog.discovery.tick'`).Scan(&retired, &live, &future); err != nil {
			application.store.Close()
			t.Fatal(err)
		}
		application.store.Close()
		if retired != 2 || live != 0 || future != 0 {
			t.Fatalf("boot %d retired=%d live=%d future=%d", boot+1, retired, live, future)
		}
	}
	var unchanged bool
	if err := db.QueryRowContext(ctx, `SELECT bool_and(
		to_jsonb(j)-ARRAY['lane','status','worker_id','fencing_token','lease_until','last_error','updated_at'] =
		old.row-ARRAY['lane','status','worker_id','fencing_token','lease_until','last_error','updated_at'])
		FROM sync_jobs j JOIN jsonb_array_elements($1::jsonb) old(row) ON j.id=(old.row->>'id')::uuid`, original).Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if !unchanged {
		t.Fatal("upgrade changed retired tick audit history")
	}
}
