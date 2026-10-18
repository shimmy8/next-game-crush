package app

import (
	"context"
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

func TestAppBootstrapsSeedWithoutSteamDiscovery(t *testing.T) {
	cfg := discoveryAppConfig(t)
	ctx := context.Background()
	for boot := 0; boot < 2; boot++ {
		application, err := New(cfg)
		if err != nil {
			t.Fatalf("boot %d: %v", boot+1, err)
		}
		var seedCount, futureTicks int
		if err := application.store.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM catalog_entry_sources WHERE source='seed' AND active),
			(SELECT count(*) FROM sync_jobs WHERE kind='catalog.discovery.tick' AND available_at>now())`).Scan(&seedCount, &futureTicks); err != nil {
			application.store.Close()
			t.Fatal(err)
		}
		application.store.Close()
		if seedCount != 20 || futureTicks != 1 {
			t.Fatalf("boot %d seed=%d future ticks=%d", boot+1, seedCount, futureTicks)
		}
	}
}

func TestAppUpgradeRetiredDiscoveryTicksKeepOneLiveSuccessorChain(t *testing.T) {
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
		if retired != 2 || live != 2 || future != 1 {
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
