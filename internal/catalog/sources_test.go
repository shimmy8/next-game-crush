package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/steam"
)

func TestBootstrapKeepsKnownReleasedSeedInactive(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	details := modeDetails()
	details.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "2099"}
	game := modeGame(t, db, details, "steam_store", now)
	client := &modeSteamClient{details: details}
	svc, _ := NewService(db, client, Seed{Version: 1, ObservedDate: "2026-10-17", Entries: []SeedEntry{{AppID: 42, ReleaseWindow: "2027", SourceURL: "https://store.steampowered.com/app/42/", ObservedDate: "2026-10-17"}}})
	if err := svc.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	var active bool
	var count int
	var source string
	var snapshot []byte
	if err := db.Pool.QueryRow(ctx, `SELECT active FROM catalog_entry_sources WHERE game_id=$1 AND source='seed'`, game.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entries WHERE game_id=$1`, game.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT source,source_snapshot FROM games WHERE id=$1`, game.ID).Scan(&source, &snapshot); err != nil {
		t.Fatal(err)
	}
	if active || count != 0 || source != "steam_store" || string(snapshot) != string(game.SourceSnapshot) {
		t.Fatal("bootstrap reactivated or overwrote known released Store snapshot")
	}
}

func TestLegacyWishlistCleanupPreservesUnion(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	game := modeGame(t, db, modeDetails(), "steam_store", now)
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, userID, game.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'wishlist')`, game.ID); err != nil {
		t.Fatal(err)
	}
	svc, _ := NewService(db, &modeSteamClient{}, Seed{Version: 1})
	if err := svc.observeRelease(ctx, game.ID, ParseReleaseDate("2027", now), "test", now); err != nil {
		t.Fatal(err)
	}
	before, err := db.Queries.ListKnownCatalogGames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CleanupLegacyWishlistProjection(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := db.Queries.ListKnownCatalogGames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var projections, wish, history int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_entries),(SELECT count(*) FROM wishlist_entries),(SELECT count(*) FROM game_release_dates)`).Scan(&projections, &wish, &history); err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || len(after) != 1 || before[0].ID != after[0].ID || projections != 0 || wish != 1 || history != 1 {
		t.Fatal("legacy cleanup damaged wishlist union or release history")
	}
}

func TestSharedSourcesProjectionAndHistory(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	game := modeGame(t, db, modeDetails(), "steam_store", now)
	set := func(source string, active bool) {
		t.Helper()
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), source, active, now); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	set("seed", true)
	set("steam_discovery", true)
	set("seed", false)
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entries`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("active discovery lost projection: %d %v", n, err)
	}
	set("steam_discovery", false)
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("inactive sources retained projection: %d %v", n, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entry_sources`).Scan(&n); err != nil || n != 2 {
		t.Fatal("source history deleted")
	}
}

func TestSharedSourcesRefreshPreservesFailuresAndRetainsReleasedHistory(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	details := modeDetails()
	game := modeGame(t, db, details, "steam_store", now)
	client := &modeSteamClient{details: details}
	svc, _ := NewService(db, client, Seed{Version: 1})
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, source := range []string{"seed", "steam_discovery"} {
		if _, err := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), source, true, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	client.err = steam.ErrUnavailable
	if err := svc.refreshOne(ctx, game); err != nil {
		t.Fatal(err)
	}
	var active, projections int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_entry_sources WHERE active),(SELECT count(*) FROM catalog_entries)`).Scan(&active, &projections); err != nil {
		t.Fatal(err)
	}
	if active != 2 || projections != 1 {
		t.Fatal("unavailable metadata removed last successful shared membership")
	}
	client.err = nil
	client.details.ReleaseDate.ComingSoon = false
	if err := svc.refreshOne(ctx, game); err != nil {
		t.Fatal(err)
	}
	var sources, history int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_entry_sources),(SELECT count(*) FROM catalog_entry_sources WHERE active),(SELECT count(*) FROM catalog_entries),(SELECT count(*) FROM game_release_dates)`).Scan(&sources, &active, &projections, &history); err != nil {
		t.Fatal(err)
	}
	if sources != 2 || active != 0 || projections != 0 || history != 1 {
		t.Fatal("authoritative release did not deactivate shared sources while retaining history")
	}
}
