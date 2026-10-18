package feedback

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
	"testing"
	"time"
)

func TestDisableWorkflowReranksAndDiffPinsItsVersion(t *testing.T) {
	db := openFeedbackStore(t)
	ctx := context.Background()
	user, profileID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, profileID, user); err != nil {
		t.Fatal(err)
	}
	source, target := insertFeedbackGame(t, db, 99001, "Source"), insertFeedbackGame(t, db, 99002, "Matching Game")
	sourceRec := insertFeedbackRanking(t, db, user, profileID, source, 1, 15, "before-adjustment")
	feedbackRepo, _ := NewSQLRepository(db)
	event, _, err := feedbackRepo.Record(ctx, Command{UserID: user, RecommendationID: sourceRec, Action: ActionDislike, IdempotencyKey: "co-op-dislike"})
	if err != nil {
		t.Fatal(err)
	}
	if err := feedbackRepo.SaveClassification(ctx, Classification{FeedbackID: event.ID, Taxonomy: "multiplayer", Polarity: "negative", Confidence: .9, Actionable: true, Model: "fixture", InputContentHash: "fixture", SchemaVersion: 2, Preference: &preferences.Target{Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{source, target} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin)VALUES($1,'seed')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_modes(game_id,mode,source,source_content_hash,last_successful_fetch_at)VALUES($1,'co-op','steam_category','fixture',now())`, target); err != nil {
		t.Fatal(err)
	}
	profilesRepo, _ := profile.NewSQLRepository(db, "embed")
	profiles, _ := profile.NewService(profilesRepo)
	rankingRepo, _ := recommendations.NewSQLRepository(db, "embed", "text")
	ranking, _ := recommendations.NewService(rankingRepo, profiles, nil)
	queue := jobs.NewQueue(db)
	ranking.SetQueue(queue)
	items, err := ranking.Rebuild(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	hadPenalty := false
	for _, item := range items {
		if item.GameID == target && item.Factors.NegativeModePreference == .9 {
			hadPenalty = true
		}
	}
	if !hadPenalty {
		t.Fatal("fixture did not apply preference penalty")
	}
	prefRepo, _ := preferences.NewSQLRepository(db)
	prefs, _ := preferences.NewService(prefRepo, queue)
	values, err := prefs.List(ctx, user, false)
	if err != nil || len(values) != 1 {
		t.Fatalf("adjustments=%d err=%v", len(values), err)
	}
	accepted, err := prefs.Disable(ctx, user, values[0].ID)
	if err != nil || accepted.PreviousRankingVersion != 2 {
		t.Fatalf("accepted=%#v err=%v", accepted, err)
	}
	worker := jobs.NewWorker(queue, "disable-fixture", jobs.WithPollInterval(time.Millisecond*5))
	if err := profiles.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := ranking.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := prefs.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		if err := db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, accepted.RebuildJobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if status == "dead" || time.Now().After(deadline) {
			t.Fatalf("disable workflow status=%s", status)
		}
		time.Sleep(time.Millisecond * 10)
	}
	latest, err := ranking.Latest(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range latest {
		if item.Factors.NegativeModePreference != 0 {
			t.Fatal("disabled penalty survived rerank")
		}
	}
	if _, err := ranking.Rebuild(ctx, user); err != nil {
		t.Fatal(err)
	}
	diff, err := feedbackRepo.RankingDiff(ctx, user, accepted.PreviousRankingVersion, accepted.RebuildJobID)
	if err != nil || diff.CurrentVersion != 3 || diff.RebuildStatus != "succeeded" || diff.Adjustment == nil || diff.Adjustment.Active {
		t.Fatalf("pinned diff=%#v err=%v", diff, err)
	}
	if _, err := feedbackRepo.RankingDiff(ctx, uuid.New(), accepted.PreviousRankingVersion, accepted.RebuildJobID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign diff=%v", err)
	}
	if _, err := feedbackRepo.RankingDiff(ctx, user, 1, accepted.RebuildJobID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong baseline diff=%v", err)
	}
	var events, classifications, badJobs int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM feedback_events),(SELECT count(*) FROM feedback_classifications),(SELECT count(*) FROM sync_jobs WHERE user_id IS DISTINCT FROM $1 OR priority<>100)`, user).Scan(&events, &classifications, &badJobs); err != nil || events != 1 || classifications != 1 || badJobs != 0 {
		t.Fatalf("history/jobs=%d/%d/%d err=%v", events, classifications, badJobs, err)
	}
}
