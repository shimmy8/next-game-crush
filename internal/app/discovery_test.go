package app

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/config"
)

func TestAppBootstrapsSeedWithoutSteamDiscovery(t *testing.T) {
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
