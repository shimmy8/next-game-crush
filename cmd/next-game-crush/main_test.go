package main

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
	"next-game-crush/internal/store"
)

func TestCatalogRefreshCommandOnlyEnqueues(t *testing.T) {
	cfg := config.Config{CatalogDiscovery: catalog.DefaultDiscoveryConfig()}
	served, enqueued := 0, 0
	err := dispatch([]string{"catalog", "refresh"}, cfg, func(config.Config) error { served++; return nil }, func(config.Config) error { enqueued++; return nil })
	if err != nil || served != 0 || enqueued != 1 {
		t.Fatalf("served=%d enqueued=%d err=%v", served, enqueued, err)
	}
	for _, args := range [][]string{{"catalog"}, {"catalog", "refresh", "extra"}, {"unexpected"}} {
		if err := dispatch(args, cfg, func(config.Config) error { t.Fatal("served unknown command"); return nil }, func(config.Config) error { t.Fatal("enqueued unknown command"); return nil }); err == nil {
			t.Fatal("unknown command accepted")
		}
	}
	t.Run("normal dependencies enqueue only", func(t *testing.T) {
		raw := os.Getenv("TEST_DATABASE_URL")
		if raw == "" {
			t.Skip("TEST_DATABASE_URL is required")
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Path != "/next_game_crush_test" && parsed.Path != "/next_game_crush_e2e") {
			t.Fatal("requires isolated test database")
		}
		ctx := context.Background()
		admin, err := pgx.Connect(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(ctx)
		schema := "catalog_cli_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		defer admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		values := parsed.Query()
		values.Set("search_path", schema+",public")
		parsed.RawQuery = values.Encode()
		cfg.DatabaseURL = parsed.String()
		// Discovery defaults disabled. Explicit manual refresh is still permitted;
		// no Steam credentials, worker identity, frontend, seed or HTTP are needed.
		if err := enqueueCatalogRefresh(cfg); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var roots, queued, feeds int
		if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sync_jobs),(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.run' AND lane='catalog' AND status='queued' AND attempts=0),(SELECT count(*) FROM catalog_search_feeds)`).Scan(&roots, &queued, &feeds); err != nil {
			t.Fatal(err)
		}
		if roots != 1 || queued != 1 || feeds != 0 {
			t.Fatalf("roots=%d queued=%d feeds=%d", roots, queued, feeds)
		}
	})
}
