package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
	migrationfiles "next-game-crush/db/migrations"
)

func TestCatalogDiscoveryMigrationPreservesVersion8Data(t *testing.T) {
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
	schema := "discovery_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	q := parsed.Query()
	q.Set("search_path", schema+",public")
	parsed.RawQuery = q.Encode()
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.UpTo(ctx, 8); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users(id) VALUES('00000000-0000-4000-8000-000000000001')`,
		`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES('00000000-0000-4000-8000-000000000002',42,'Seed','steam',now()),('00000000-0000-4000-8000-000000000003',43,'Wishlist','steam',now())`,
		`INSERT INTO catalog_entries(game_id,origin) SELECT id,CASE steam_app_id WHEN 42 THEN 'seed' ELSE 'wishlist' END FROM games`,
		`INSERT INTO game_release_dates(id,game_id,precision,raw_text,observed_at,source,last_successful_fetch_at) VALUES('00000000-0000-4000-8000-000000000004','00000000-0000-4000-8000-000000000002','year','2027',now(),'steam',now())`,
		`INSERT INTO preference_profiles(id,user_id,version) VALUES('00000000-0000-4000-8000-000000000005','00000000-0000-4000-8000-000000000001',1)`,
		`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES('00000000-0000-4000-8000-000000000006','00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000005',1,'v1','first',true)`,
		`INSERT INTO sync_jobs(id,kind,idempotency_key) VALUES('00000000-0000-4000-8000-000000000007','catalog.refresh','old')`,
	} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = p.UpTo(ctx, 9); err != nil {
		t.Fatal(err)
	}
	var seedSources, wishlistSources, legacyWishlist, published, priority, releases int
	if err = db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM catalog_entry_sources WHERE source='seed'),(SELECT count(*) FROM catalog_entry_sources WHERE game_id='00000000-0000-4000-8000-000000000003'),(SELECT count(*) FROM catalog_entries WHERE origin='wishlist'),(SELECT count(*) FROM recommendation_runs WHERE published),(SELECT priority FROM sync_jobs LIMIT 1),(SELECT count(*) FROM game_release_dates)`).Scan(&seedSources, &wishlistSources, &legacyWishlist, &published, &priority, &releases); err != nil {
		t.Fatal(err)
	}
	if seedSources != 1 || wishlistSources != 0 || legacyWishlist != 1 || published != 1 || priority != 50 || releases != 1 {
		t.Fatal("migration changed existing data/defaults")
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO catalog_entry_sources(game_id,source) VALUES('00000000-0000-4000-8000-000000000002','steam_discovery')`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO catalog_discovery_state(source,mode) VALUES('steam','bad')`,
		`INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status) VALUES('steam',42,now(),'bad')`,
		`INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status,precision,release_date) VALUES('steam',42,now(),'admitted','month',current_date)`,
		`INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status,release_date) VALUES('steam',42,now(),'admitted',current_date)`,
		`INSERT INTO catalog_discovery_budget(hour_at,counts) VALUES('2026-10-18 12:00:00+00','{"admitted":-1}')`,
		`INSERT INTO catalog_discovery_budget(hour_at,counts) VALUES('2026-10-18 12:00:00+00','{"unknown_counter":-1}')`,
	} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal("accepted invalid discovery state")
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status) VALUES('steam',42,'2026-10-18 12:00:00+00','released')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status) VALUES('steam',42,'2026-10-18 12:00:00+00','released')`); err == nil {
		t.Fatal("accepted duplicate source revision")
	}
	if _, err = p.Down(ctx); err == nil || !strings.Contains(err.Error(), "disable discovery instead") {
		t.Fatal("populated migration rollback was not safely refused")
	}
}
