package recommendations

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/profile"
)

func TestCandidatesConfiguredHorizon(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	day90 := insertRankingGame(t, db, 9100, "Horizon Boundary")
	day91 := insertRankingGame(t, db, 9101, "Beyond Default Horizon")
	wishlist := insertRankingGame(t, db, 9102, "Mandatory Wishlist")
	for _, entry := range []struct {
		id   uuid.UUID
		days int
	}{{day90, 90}, {day91, 91}, {wishlist, 200}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, entry.id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'day',$3,$4,$5,'test',$5)`, uuid.New(), entry.id, now.AddDate(0, 0, entry.days), now.AddDate(0, 0, entry.days).Format(time.DateOnly), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, wishlist, now); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	repository.now = func() time.Time { return now }
	assertCandidates := func(limit int, want ...uuid.UUID) {
		t.Helper()
		got, err := repository.Candidates(ctx, userID, profile.Profile{}, limit)
		if err != nil {
			t.Fatal(err)
		}
		ids := make(map[uuid.UUID]bool, len(got))
		for _, candidate := range got {
			ids[candidate.GameID] = true
		}
		if len(got) != len(want) {
			t.Fatalf("got %d candidates, want %d: %#v", len(got), len(want), got)
		}
		for _, id := range want {
			if !ids[id] {
				t.Fatal("configured horizon or mandatory wishlist membership was lost")
			}
		}
	}
	// The constructor retains 90 days; wishlist bypasses both horizon and limit.
	assertCandidates(1, day90, wishlist)
	if err := repository.SetHorizonDays(120); err != nil {
		t.Fatal(err)
	}
	assertCandidates(10, day90, day91, wishlist)
	for _, invalid := range []int{-1, 0, 367} {
		if err := repository.SetHorizonDays(invalid); err == nil {
			t.Fatalf("accepted invalid horizon %d", invalid)
		}
	}
	assertCandidates(10, day90, day91, wishlist)
	for _, boundary := range []int{1, 366} {
		if err := repository.SetHorizonDays(boundary); err != nil {
			t.Fatalf("rejected valid horizon %d: %v", boundary, err)
		}
	}
}
