package sync

import (
	"context"
	"github.com/google/uuid"
	"next-game-crush/internal/steam"
	"testing"
	"time"
)

func TestSharedSourcesSurviveWishlistRemoval(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	ctx := context.Background()
	if err := service.Run(ctx, userID); err != nil {
		t.Fatal(err)
	}
	var gameID uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT id FROM games WHERE steam_app_id=20`).Scan(&gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entry_sources(game_id,source) VALUES($1,'seed'),($1,'steam_discovery')`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id) VALUES($1) ON CONFLICT(game_id) DO NOTHING`, gameID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.wishlist = nil
	fake.mu.Unlock()
	if err := service.Run(ctx, userID); err != nil {
		t.Fatal(err)
	}
	var sources, projection, history, wishlist int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_entry_sources WHERE game_id=$1 AND active),(SELECT count(*) FROM catalog_entries WHERE game_id=$1),(SELECT count(*) FROM game_release_dates WHERE game_id=$1),(SELECT count(*) FROM wishlist_entries WHERE game_id=$1)`, gameID).Scan(&sources, &projection, &history, &wishlist); err != nil {
		t.Fatal(err)
	}
	if sources != 2 || projection != 1 || history == 0 || wishlist != 0 {
		t.Fatal("wishlist removal damaged shared sources or history")
	}
}

func TestRefreshKnownRefreshesWishlistWithoutProjection(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	ctx := context.Background()
	if err := service.Run(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM catalog_entries WHERE origin='wishlist'`); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	d := fake.details[20]
	d.Name = "Updated independent wishlist"
	d.ReleaseDate = steam.ReleaseDate{ComingSoon: true, Raw: "December 2026"}
	fake.details[20] = d
	fake.mu.Unlock()
	if err := service.catalog.RefreshKnown(ctx); err != nil {
		t.Fatal(err)
	}
	var name, raw string
	var success time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT g.name,r.raw_text,f.last_successful_fetch_at FROM games g JOIN catalog_fetch_states f ON f.game_id=g.id JOIN LATERAL(SELECT raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1)r ON true WHERE g.steam_app_id=20`).Scan(&name, &raw, &success); err != nil {
		t.Fatal(err)
	}
	if name != d.Name || raw != d.ReleaseDate.Raw || success.IsZero() {
		t.Fatal("wishlist without projection was not refreshed")
	}
}
