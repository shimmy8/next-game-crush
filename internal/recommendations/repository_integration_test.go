package recommendations

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/store"
)

func TestRebuildRetrievesSemanticAndWishlistCandidatesAndPersistsFactors(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	owned := insertRankingGame(t, db, 1, "Owned Signal")
	semantic := insertRankingGame(t, db, 2, "Semantic Match")
	wishlist := insertRankingGame(t, db, 3, "Wishlist Without Vector")
	released := insertRankingGame(t, db, 4, "Already Released")
	now := time.Now().UTC()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at) VALUES($1,$2,600,60,'test',$3)`, userID, owned, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at)VALUES($1,$2,5,10,'test',$3)`, userID, owned, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{owned, semantic} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_features(game_id,model,input_content_hash,features,source,last_successful_fetch_at)VALUES($1,'text',$2,'{"themes":["space"],"mechanics":["building"]}','test',$3)`, id, id.String(), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, wishlist, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{semantic, wishlist, released} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin)VALUES($1,'seed')`, id); err != nil {
			t.Fatal(err)
		}
	}
	future, past := now.AddDate(0, 1, 0), now.AddDate(0, -1, 0)
	for _, release := range []struct {
		id        uuid.UUID
		precision string
		date      any
		raw       string
	}{{semantic, "day", future, "future"}, {wishlist, "month", nil, "January 2027"}, {released, "day", past, "past"}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at)VALUES($1,$2,$3,$4,$5,$6,'test',$6)`, uuid.New(), release.id, release.precision, release.date, release.raw, now); err != nil {
			t.Fatal(err)
		}
	}
	vector := make([]float32, profile.Dimensions)
	vector[0] = 1
	for _, id := range []uuid.UUID{owned, semantic, released} {
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: id, Model: "embed", ContentHash: id.String(), Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}
	profileRepo, _ := profile.NewSQLRepository(db, "embed")
	profiles, _ := profile.NewService(profileRepo)
	built, err := profiles.Rebuild(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if built.Version != 1 {
		t.Fatalf("profile version=%d", built.Version)
	}
	retryOne, err := profileRepo.SaveProfile(ctx, userID, profile.Vectors{Positive: built.Positive, Negative: built.Negative, Confidence: built.Confidence, Evidence: built.EvidenceCount}, profile.AlgorithmVersion, "same-job")
	if err != nil {
		t.Fatal(err)
	}
	retryTwo, err := profileRepo.SaveProfile(ctx, userID, profile.Vectors{Positive: built.Positive, Negative: built.Negative, Confidence: built.Confidence, Evidence: built.EvidenceCount}, profile.AlgorithmVersion, "same-job")
	if err != nil || retryOne.ID != retryTwo.ID || retryOne.Version != retryTwo.Version {
		t.Fatalf("retry profiles=%#v %#v err=%v", retryOne, retryTwo, err)
	}
	repo, _ := NewSQLRepository(db, "embed", "text")
	service, _ := NewService(repo, profiles, fakeExplanationClient{err: ai.ErrRefusal})
	items, err := service.Rebuild(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("recommendations=%d (%#v), want semantic and wishlist", len(items), items)
	}
	for _, item := range items {
		if item.AlgorithmVersion != AlgorithmVersion || item.Explanation == "" || item.GeneratedExplanation {
			t.Fatalf("persisted recommendation=%#v", item)
		}
	}
	if items[0].Factors.TagAffinity == 0 && items[1].Factors.TagAffinity == 0 {
		t.Fatal("structured feature affinity was not computed")
	}
	var factors int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_factors`).Scan(&factors); err != nil || factors != 2 {
		t.Fatalf("factor rows=%d err=%v", factors, err)
	}
	calendar := BuildCalendar(items)
	if len(calendar.ExactDates) != 1 || len(calendar.Month) != 1 {
		t.Fatalf("calendar=%#v", calendar)
	}
	second, err := service.Rebuild(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].RankingVersion != 2 {
		t.Fatalf("second ranking=%#v", second)
	}
	latest, err := service.Latest(ctx, userID)
	if err != nil || len(latest) != 2 || latest[0].RankingVersion != 2 || len(latest[0].Evidence) == 0 || latest[0].Evidence[0].Text == "" {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
}

func insertRankingGame(t *testing.T, db *store.Store, appID int64, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,$2,$3,'test',now())`, id, appID, name); err != nil {
		t.Fatal(err)
	}
	return id
}

func openRankingStore(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	database, err := store.Open(ctx, fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", base, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanup, _ := pgx.Connect(context.Background(), base)
		if cleanup != nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return database
}
