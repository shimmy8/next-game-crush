package recommendations

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/profile"
)

func TestCandidatesIncludeWishlistWithoutCatalogProjection(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	own, other := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1),($2)`, own, other); err != nil {
		t.Fatal(err)
	}
	wish := insertRankingGame(t, db, 81001, "Own wishlist")
	foreign := insertRankingGame(t, db, 81002, "Other wishlist")
	shared := insertRankingGame(t, db, 81003, "Shared wishlist")
	released := insertRankingGame(t, db, 81004, "Released future text")
	nonGame := insertRankingGame(t, db, 81005, "DLC")
	malformed := insertRankingGame(t, db, 81006, "Malformed fallback")
	for _, id := range []uuid.UUID{wish, shared, released, nonGame, malformed} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, own, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, other, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE games SET source_snapshot='{"AppID":81004,"Type":"game","Name":"Released","ReleaseDate":{"ComingSoon":false,"Raw":"2099"}}' WHERE id=$1`, released); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE games SET source_snapshot='{"AppID":81005,"Type":"dlc","Name":"DLC","ReleaseDate":{"ComingSoon":true,"Raw":"2099"}}' WHERE id=$1`, nonGame); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE games SET source_snapshot='{"AppID":81006,"Type":"","Name":"Malformed","ReleaseDate":{"ComingSoon":false,"Raw":"2099"}}' WHERE id=$1`, malformed); err != nil {
		t.Fatal(err)
	}
	repo, _ := NewSQLRepository(db, "embed", "text")
	repo.now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
	for _, p := range []profile.Profile{{}, {Positive: []float32{1}}} {
		got, err := repo.Candidates(ctx, own, p, 1)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[uuid.UUID]int{}
		for _, c := range got {
			counts[c.GameID]++
		}
		if counts[wish] != 1 || counts[foreign] != 0 || counts[shared] != 1 || counts[released] != 0 || counts[nonGame] != 0 || counts[malformed] != 1 {
			t.Fatalf("candidate union lost, leaked, duplicated or admitted invalid membership: %v", counts)
		}
	}
}
