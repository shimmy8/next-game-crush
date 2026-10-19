package feedback

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/preferences"
)

func TestCompleteFeedbackWorkflowLogsOmitSensitiveInputs(t *testing.T) {
	fixture := newPresentationAcceptanceFixture(t)
	ctx := context.Background()
	const feedbackCanary = "feedback-text-canary-never-log"
	const classifierCanary = "raw-classifier-output-canary-never-log"
	const modelCanary = "classifier-model-canary-never-log"

	sourceGame := fixture.discoveryIDs[0]
	targetGame := fixture.discoveryIDs[1]
	for _, gameID := range []uuid.UUID{sourceGame, targetGame} {
		if _, err := fixture.db.Pool.Exec(ctx, `INSERT INTO game_modes(game_id,mode,source,source_content_hash,last_successful_fetch_at) VALUES($1,'co-op','steam_category','privacy-fixture',now())`, gameID); err != nil {
			t.Fatal(err)
		}
	}

	var logs bytes.Buffer
	logger := observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))
	queue := jobs.NewQueue(fixture.db)
	fixture.profiles.SetQueue(queue)
	fixture.ranking.SetQueue(queue)
	feedbackRepo, err := NewSQLRepository(fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	classifier := canaryWorkflowClassifier{rawOutput: classifierCanary}
	service, err := NewService(feedbackRepo, classifier, modelCanary, queue)
	if err != nil {
		t.Fatal(err)
	}
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "feedback-privacy-worker", jobs.WithLogger(logger), jobs.WithPollInterval(2*time.Millisecond), jobs.WithHeartbeatInterval(time.Second))
	if err := fixture.profiles.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	preferenceRepo, err := preferences.NewSQLRepository(fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	preferenceService, err := preferences.NewService(preferenceRepo, queue)
	if err != nil {
		t.Fatal(err)
	}
	if err := preferenceService.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := fixture.ranking.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("ai.profile_summary", func(context.Context, jobs.Lease) error { return nil }); err != nil {
		t.Fatal(err)
	}

	recommendationID := fixture.recommendationID(t, sourceGame)
	accepted, err := service.Record(ctx, Command{
		UserID: fixture.userID, RecommendationID: recommendationID, Action: ActionDislike,
		Reason: "multiplayer", FreeText: feedbackCanary, IdempotencyKey: "privacy-complete-workflow",
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		if err := fixture.db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, accepted.RebuildJobID).Scan(&status); err != nil {
			cancel()
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if status == "dead" || time.Now().After(deadline) {
			cancel()
			t.Fatalf("feedback workflow status=%q", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	adjustment, err := preferenceRepo.ForFeedback(ctx, fixture.userID, accepted.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if adjustment.Target != preferences.ModeMultiplayer || !adjustment.Active {
		t.Fatalf("complete workflow did not materialize expected correction: %#v", adjustment)
	}
	classification, err := feedbackRepo.Classification(ctx, accepted.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if classification.Model != "deterministic-fallback" {
		t.Fatalf("classifier failure did not use the safe fallback: model=%q", classification.Model)
	}
	var overlays, rankings, profiles, summaries int
	if err := fixture.db.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE kind='preferences.rebuild'),
		count(*) FILTER (WHERE kind='recommendations.rebuild'),
		count(*) FILTER (WHERE kind='profile.rebuild'),
		count(*) FILTER (WHERE kind='ai.profile_summary')
		FROM sync_jobs WHERE parent_job_id=$1`, accepted.RebuildJobID).Scan(&overlays, &rankings, &profiles, &summaries); err != nil {
		t.Fatal(err)
	}
	if overlays != 1 || rankings != 1 || profiles != 0 || summaries != 0 {
		t.Fatalf("feedback child stages overlay/ranking/profile/summary=%d/%d/%d/%d", overlays, rankings, profiles, summaries)
	}
	encoded := logs.String()
	for _, forbidden := range []string{
		feedbackCanary,
		classifierCanary,
		modelCanary,
		fixture.userID.String(),
		recommendationID.String(),
		accepted.EventID.String(),
		sourceGame.String(),
		targetGame.String(),
		adjustment.ID.String(),
	} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("complete feedback workflow logs leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(encoded, `"kind":"feedback.classify"`) || !strings.Contains(encoded, `"stage":"succeeded"`) {
		t.Fatalf("test did not capture the completed workflow logs: %s", encoded)
	}
}

type canaryWorkflowClassifier struct{ rawOutput string }

func (c canaryWorkflowClassifier) ClassifyFeedback(context.Context, ai.FeedbackText) (ai.FeedbackClassification, error) {
	return ai.FeedbackClassification{}, fmt.Errorf("%w: %s", ai.ErrHTTP, c.rawOutput)
}
