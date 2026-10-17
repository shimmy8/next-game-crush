package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
)

func TestSyncIsIdempotent(t *testing.T) {
	service, _, db, userID := newTestService(t)
	first, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("active sync IDs differ: %s != %s", first, second)
	}
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"user_games": 1, "wishlist_entries": 1, "achievement_summaries": 1} {
		var got int
		if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", table, got, want)
		}
	}
}

func TestSyncAddsWishlistGameToCatalog(t *testing.T) {
	service, _, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM games g JOIN wishlist_entries w ON w.game_id=g.id WHERE w.user_id=$1 AND g.steam_app_id=20`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("wishlist catalog rows = %d, want 1", count)
	}
}

func TestSyncPreservesSnapshotWhenProfileBecomesPrivate(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := db.Pool.QueryRow(context.Background(), `SELECT ug.playtime_minutes FROM user_games ug JOIN games g ON g.id=ug.game_id WHERE ug.user_id=$1 AND g.steam_app_id=10`, userID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.ownedAvailability, fake.recentAvailability, fake.wishlistAvailability = steam.Unavailable, steam.Unavailable, steam.Unavailable
	fake.owned, fake.recent, fake.wishlist = nil, nil, nil
	fake.mu.Unlock()
	service.now = func() time.Time { return time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC) }
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var after int64
	var availability string
	if err := db.Pool.QueryRow(context.Background(), `SELECT ug.playtime_minutes FROM user_games ug JOIN games g ON g.id=ug.game_id WHERE ug.user_id=$1 AND g.steam_app_id=10`, userID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(context.Background(), `SELECT availability FROM steam_accounts WHERE user_id=$1`, userID).Scan(&availability); err != nil {
		t.Fatal(err)
	}
	if after != before || availability != string(steam.Unavailable) {
		t.Fatalf("snapshot after private = %d/%s, want %d/unavailable", after, availability, before)
	}
	var lastAttempt, lastSuccess time.Time
	if err := db.Pool.QueryRow(context.Background(), `SELECT last_attempt_at,last_successful_fetch_at FROM user_source_states WHERE user_id=$1 AND source='owned_games'`, userID).Scan(&lastAttempt, &lastSuccess); err != nil {
		t.Fatal(err)
	}
	if !lastAttempt.After(lastSuccess) {
		t.Fatalf("private state freshness = attempt %s, success %s", lastAttempt, lastSuccess)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.GetStatus(context.Background(), userID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	foundUnavailable := false
	for _, stage := range status.Stages {
		if stage.Stage == "owned_games" && stage.Availability == string(steam.Unavailable) && stage.Status == "degraded" {
			foundUnavailable = true
		}
	}
	if !foundUnavailable {
		t.Fatalf("sync status did not expose unavailable owned-games source: %#v", status.Stages)
	}
}

func TestCatalogFailurePreservesLastGoodSnapshot(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := db.Pool.QueryRow(context.Background(), `SELECT source_snapshot FROM games WHERE steam_app_id=20`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.detailErrors[20] = steam.ErrUnavailable
	fake.mu.Unlock()
	service.now = func() time.Time { return time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC) }
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after []byte
	var availability string
	if err := db.Pool.QueryRow(context.Background(), `SELECT source_snapshot,availability FROM games WHERE steam_app_id=20`).Scan(&after, &availability); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || availability != string(steam.Unavailable) {
		t.Fatalf("catalog fallback changed snapshot or state: availability=%s", availability)
	}
}

func TestReleaseChangeAppendsHistory(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var gameID uuid.UUID
	if err := db.Pool.QueryRow(context.Background(), `SELECT id FROM games WHERE steam_app_id=30`).Scan(&gameID); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM game_release_dates WHERE game_id=$1`, gameID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	details := fake.details[30]
	details.ReleaseDate.Raw = "Dec 2026"
	fake.details[30] = details
	fake.mu.Unlock()
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM game_release_dates WHERE game_id=$1`, gameID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("history count = %d, want %d", after, before+1)
	}
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var repeated int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM game_release_dates WHERE game_id=$1`, gameID).Scan(&repeated); err != nil {
		t.Fatal(err)
	}
	if repeated != after {
		t.Fatalf("unchanged refresh appended history: count = %d, want %d", repeated, after)
	}
}

func TestReleasedWishlistGameIsNotCatalogMember(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	fake.mu.Lock()
	details := fake.details[20]
	details.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "1 Jan, 2020"}
	fake.details[20] = details
	fake.mu.Unlock()
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM catalog_entries ce JOIN games g ON g.id=ce.game_id WHERE g.steam_app_id=20`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("released wishlist catalog memberships = %d, want 0", count)
	}
}

func TestWorkerRunsEnqueuedWorkflowToRequiredChildCompletion(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	worker := jobs.NewWorker(service.queue, "sync-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if status == "dead" {
			t.Fatal("workflow dead-lettered")
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow status = %s after timeout", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var children, succeeded int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*),count(*) FILTER (WHERE status='succeeded') FROM sync_jobs WHERE parent_job_id=$1`, jobID).Scan(&children, &succeeded); err != nil {
		t.Fatal(err)
	}
	if children != 2 || succeeded != 2 {
		t.Fatalf("child jobs = %d/%d succeeded, want 2/2", succeeded, children)
	}
	fake.mu.Lock()
	ownedCalls := fake.ownedCalls
	fake.mu.Unlock()
	if ownedCalls != 1 {
		t.Fatalf("parent reran Steam import after children: owned calls = %d", ownedCalls)
	}
}

func TestWorkerLeavesReleasedWishlistGameOutsideCatalog(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	fake.mu.Lock()
	details := fake.details[20]
	details.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "1 Jan, 2020"}
	fake.details[20] = details
	fake.mu.Unlock()
	worker := jobs.NewWorker(service.queue, "released-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow status = %s after timeout", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM catalog_entries ce JOIN games g ON g.id=ce.game_id WHERE g.steam_app_id=20`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("released wishlist catalog memberships after worker = %d, want 0", count)
	}
}

func TestKnownReleasedWishlistGameStaysExcludedWhenRefreshFails(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	fake.mu.Lock()
	details := fake.details[20]
	details.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "1 Jan, 2020"}
	fake.details[20] = details
	fake.mu.Unlock()
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.detailErrors[20] = errors.New("temporary")
	fake.mu.Unlock()
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM catalog_entries ce JOIN games g ON g.id=ce.game_id WHERE g.steam_app_id=20`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("known released wishlist membership after failed refresh = %d, want 0", count)
	}
}

func TestSyncStatusExposesCatalogDegradation(t *testing.T) {
	service, fake, _, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.detailErrors[20] = steam.ErrUnavailable
	fake.mu.Unlock()
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.GetStatus(context.Background(), userID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range status.Stages {
		if stage.Stage == "catalog:20" && stage.Status == "degraded" && stage.Availability == string(steam.Unavailable) {
			return
		}
	}
	t.Fatalf("catalog degradation missing from status: %#v", status.Stages)
}

type fakeSteam struct {
	mu                                                          sync.Mutex
	owned                                                       []steam.OwnedGame
	recent                                                      []steam.RecentGame
	wishlist                                                    []steam.WishlistItem
	ownedAvailability, recentAvailability, wishlistAvailability steam.Availability
	details                                                     map[int64]steam.GameDetails
	detailErrors                                                map[int64]error
	ownedCalls                                                  int
}

func (f *fakeSteam) OwnedGames(context.Context, string) ([]steam.OwnedGame, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownedCalls++
	return append([]steam.OwnedGame(nil), f.owned...), f.ownedAvailability, nil
}
func (f *fakeSteam) RecentlyPlayed(context.Context, string) ([]steam.RecentGame, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]steam.RecentGame(nil), f.recent...), f.recentAvailability, nil
}
func (f *fakeSteam) Wishlist(context.Context, string) ([]steam.WishlistItem, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]steam.WishlistItem(nil), f.wishlist...), f.wishlistAvailability, nil
}
func (f *fakeSteam) Achievements(_ context.Context, _ string, appID int64) (steam.AchievementSummary, steam.Availability, error) {
	return steam.AchievementSummary{AppID: appID, GameName: "Game", Total: 10, Unlocked: 5}, steam.Available, nil
}
func (f *fakeSteam) GameDetails(_ context.Context, appID int64) (steam.GameDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.detailErrors[appID]; err != nil {
		return steam.GameDetails{}, err
	}
	if d, ok := f.details[appID]; ok {
		return d, nil
	}
	return steam.GameDetails{}, steam.ErrUnavailable
}

func newTestService(t *testing.T) (*Service, *fakeSteam, *store.Store, uuid.UUID) {
	t.Helper()
	db := openSyncStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO steam_accounts(user_id,steam_id) VALUES($1,'76561197960287930')`, userID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	fake := &fakeSteam{owned: []steam.OwnedGame{{AppID: 10, Name: "Owned", PlaytimeForever: 600, LastPlayedAt: now}}, recent: []steam.RecentGame{{AppID: 10, Name: "Owned", PlaytimeForever: 600, PlaytimeTwoWeeks: 20, LastPlayedAt: now}}, wishlist: []steam.WishlistItem{{AppID: 20, Priority: 1}}, ownedAvailability: steam.Available, recentAvailability: steam.Available, wishlistAvailability: steam.Available, detailErrors: map[int64]error{}, details: map[int64]steam.GameDetails{
		10: {AppID: 10, Type: "game", Name: "Owned", ReleaseDate: steam.ReleaseDate{Raw: "1 Jan, 2020"}},
		20: {AppID: 20, Type: "game", Name: "Wish", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Nov 2026"}},
		30: {AppID: 30, Type: "game", Name: "Seed", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Nov 2026"}},
	}}
	cat, err := catalog.NewService(db, fake, catalog.Seed{Version: 1, ObservedDate: "2026-10-17", Entries: []catalog.SeedEntry{{AppID: 30, ReleaseWindow: "Nov 2026", SourceURL: "https://store.steampowered.com/app/30/", ObservedDate: "2026-10-17"}}})
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(db)
	service, err := NewService(db, queue, fake, cat)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	return service, fake, db, userID
}

func openSyncStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolated := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	db, err := store.Open(context.Background(), isolated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, err := pgx.Connect(context.Background(), baseURL)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return db
}
