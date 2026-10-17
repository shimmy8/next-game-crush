package feedback

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	profilepkg "next-game-crush/internal/profile"
	"next-game-crush/internal/store"
)

func TestSQLRecordPersistsSnapshotBeforeClassificationAndSurvivesRetryAfterRerank(t *testing.T) {
	db := openFeedbackStore(t)
	ctx := context.Background()
	userID, profileID, gameID := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,1,'Original name','test',now())`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	recommendationID := insertFeedbackRanking(t, db, userID, profileID, gameID, 1, 88, "run-one")
	repo, _ := NewSQLRepository(db)
	command := Command{UserID: userID, RecommendationID: recommendationID, Action: ActionLike, Reason: "genre", FreeText: "More like this", IdempotencyKey: "stable-browser-event"}
	first, created, err := repo.Record(ctx, command)
	if err != nil || !created {
		t.Fatalf("first=%#v created=%v err=%v", first, created, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE recommendations SET score=1 WHERE id=$1`, recommendationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE recommendation_runs SET published=false WHERE user_id=$1 AND ranking_version=1`, userID); err != nil {
		t.Fatal(err)
	}
	second, created, err := repo.Record(ctx, command)
	if err != nil || created || second.ID != first.ID || second.PreviousRankingVersion != 1 {
		t.Fatalf("second=%#v created=%v err=%v", second, created, err)
	}
	var snapshot struct {
		Score float64 `json:"score"`
		Name  string  `json:"name"`
	}
	if err := json.Unmarshal(first.Snapshot, &snapshot); err != nil || snapshot.Score != 88 || snapshot.Name != "Original name" {
		t.Fatalf("snapshot=%#v err=%v raw=%s", snapshot, err, first.Snapshot)
	}
}

func TestRankingDiffReportsAddedRemovedAndReorderedRecommendationIDs(t *testing.T) {
	db := openFeedbackStore(t)
	ctx := context.Background()
	userID, profileID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	gameA, gameB, gameC := insertFeedbackGame(t, db, 11, "A"), insertFeedbackGame(t, db, 12, "B"), insertFeedbackGame(t, db, 13, "C")
	oldA := insertFeedbackRanking(t, db, userID, profileID, gameA, 1, 90, "run-one")
	_ = insertFeedbackRanking(t, db, userID, profileID, gameB, 1, 80, "run-one")
	newB := insertFeedbackRanking(t, db, userID, profileID, gameB, 2, 95, "run-two")
	newC := insertFeedbackRanking(t, db, userID, profileID, gameC, 2, 90, "run-two")
	jobID, eventID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,status)VALUES($1,'feedback.classify','diff-job','succeeded')`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,recommendation_snapshot,previous_ranking_version,rebuild_job_id)VALUES($1,$2,$3,'like','diff-event','{}',1,$4)`, eventID, oldA, userID, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash)VALUES($1,'genre','positive',.9,true,'test','hash')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,status,result)VALUES($1,'recommendations.rebuild',$2,'succeeded','{"rankingVersion":2}')`, uuid.New(), "feedback:"+eventID.String()+":ranking:v1"); err != nil {
		t.Fatal(err)
	}
	repo, _ := NewSQLRepository(db)
	if version, err := repo.FinalizeRanking(ctx,eventID); err != nil || version != 2 { t.Fatalf("finalized version=%d err=%v",version,err) }
	_ = insertFeedbackRanking(t, db, userID, profileID, gameA, 3, 99, "later-unrelated-run")
	diff, err := repo.RankingDiff(ctx, userID, 1, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if diff.RebuildStatus != "succeeded" || diff.CurrentVersion != 2 || diff.Classification == nil || !diff.Classification.Actionable || len(diff.Added) != 1 || diff.Added[0] != newC || len(diff.Removed) != 1 || diff.Removed[0] != oldA || len(diff.Reordered) != 1 || diff.Reordered[0] != newB {
		t.Fatalf("diff=%#v", diff)
	}
}

func TestOnlyActionableClassificationAddsStructuredProfileWeight(t *testing.T) {
	db := openFeedbackStore(t)
	ctx := context.Background()
	userID, profileID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	highGame, lowGame := insertFeedbackGame(t, db, 21, "High"), insertFeedbackGame(t, db, 22, "Low")
	highRec := insertFeedbackRanking(t, db, userID, profileID, highGame, 1, 90, "signals")
	lowRec := insertFeedbackRanking(t, db, userID, profileID, lowGame, 1, 80, "signals")
	vector := make([]float32, profilepkg.Dimensions)
	vector[0] = 1
	for _, gameID := range []uuid.UUID{highGame, lowGame} {
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: gameID.String(), Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}
	highEvent, lowEvent := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'like','high-signal'),($4,$5,$3,'like','low-signal')`, highEvent, highRec, userID, lowEvent, lowRec); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash)VALUES($1,'genre','positive',.9,true,'test','high'),($2,'genre','positive',.3,false,'test','low')`, highEvent, lowEvent); err != nil {
		t.Fatal(err)
	}
	repo, err := profilepkg.NewSQLRepository(db, "embed")
	if err != nil {
		t.Fatal(err)
	}
	signals, err := repo.LoadSignals(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	likes := map[uuid.UUID]int{}
	for _, signal := range signals {
		likes[signal.GameID] = signal.Likes
	}
	if likes[highGame] != 2 || likes[lowGame] != 1 {
		t.Fatalf("profile likes=%v", likes)
	}
}

func insertFeedbackGame(t *testing.T, db *store.Store, appID int64, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,$2,$3,'test',now())`, id, appID, name); err != nil {
		t.Fatal(err)
	}
	return id
}
func insertFeedbackRanking(t *testing.T, db *store.Store, userID, profileID, gameID uuid.UUID, version int64, score float64, buildKey string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published)VALUES($1,$2,$3,$4,'ranking-v1',$5,true) ON CONFLICT(user_id,ranking_version)DO NOTHING`, uuid.New(), userID, profileID, version, buildKey); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score)VALUES($1,$2,$3,$4,'ranking-v1',$5,$6)`, id, userID, gameID, profileID, version, score); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_factors(recommendation_id,factors,weights)VALUES($1,'{"confidence":0.8}','{}')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func openFeedbackStore(t *testing.T) *store.Store {
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
