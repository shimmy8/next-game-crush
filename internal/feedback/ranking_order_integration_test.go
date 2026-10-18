package feedback

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/recommendations"
	"testing"
)

func TestRankingDiffAndPersistedReadsMatchGoCaseTieOrder(t *testing.T) {
	db := openFeedbackStore(t)
	ctx := context.Background()
	user, p := uuid.New(), uuid.New()
	upper := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	lower := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	for _, s := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id)VALUES($1)`, []any{user}},
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, []any{p, user}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,9991,'Alpha','test',now()),($2,9992,'alpha','test',now())`, []any{upper, lower}},
	} {
		if _, err := db.Pool.Exec(ctx, s.query, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	oldLower := insertFeedbackRanking(t, db, user, p, lower, 1, 90, "tie-before")
	insertFeedbackRanking(t, db, user, p, upper, 1, 80, "tie-before")
	newUpper := insertFeedbackRanking(t, db, user, p, upper, 2, 80, "tie-after")
	newLower := insertFeedbackRanking(t, db, user, p, lower, 2, 80, "tie-after")
	event, job := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,status)VALUES($1,'feedback.classify','tie-workflow','succeeded')`, job); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,previous_ranking_version,resulting_ranking_version,rebuild_job_id)VALUES($1,$2,$3,'like','tie-event',1,2,$4)`, event, oldLower, user, job); err != nil {
		t.Fatal(err)
	}
	repo, _ := NewSQLRepository(db)
	diff, err := repo.RankingDiff(ctx, user, 1, job)
	if err != nil || len(diff.Reordered) != 2 || diff.Reordered[0] != newUpper || diff.Reordered[1] != newLower {
		t.Fatalf("case reorder=%#v err=%v", diff, err)
	}
	rankingRepo, _ := recommendations.NewSQLRepository(db, "embed", "text")
	items, err := rankingRepo.Latest(ctx, user)
	if err != nil || len(items) != 2 || items[0].GameID != upper || items[1].GameID != lower {
		t.Fatalf("latest order=%#v err=%v", items, err)
	}
	generated, err := db.Queries.GetLatestRecommendations(ctx, pgtype.UUID{Bytes: user, Valid: true})
	if err != nil || len(generated) != 2 || uuid.UUID(generated[0].GameID.Bytes) != upper || uuid.UUID(generated[1].GameID.Bytes) != lower {
		t.Fatalf("generated order=%#v err=%v", generated, err)
	}
}
