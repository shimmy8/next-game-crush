package preferences

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
)

func TestEffectiveDirectReactionUsesCreatedAtThenIDAndIgnoresReminder(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, _ := preferenceFixture(t, db)
	if _, err := db.Pool.Exec(ctx, `DELETE FROM feedback_events WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	var recommendation uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT id FROM recommendations WHERE user_id=$1 AND game_id=$2`, user, game).Scan(&recommendation); err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Truncate(time.Microsecond)
	lower := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	upper := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	for _, event := range []struct {
		id     uuid.UUID
		action string
		at     time.Time
	}{
		{lower, "dislike", when},
		{upper, "like", when},
		{uuid.New(), "remind_me", when.Add(time.Hour)},
	} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at)VALUES($1,$2,$3,$4,$5,$6)`, event.id, recommendation, user, event.action, event.id.String(), event.at); err != nil {
			t.Fatal(err)
		}
	}
	repository, _ := NewSQLRepository(db)
	snapshot, err := repository.LoadOverlaySnapshot(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Reactions) != 1 || snapshot.Reactions[0].FeedbackID != upper || snapshot.Reactions[0].Action != "like" {
		t.Fatalf("effective reactions=%+v", snapshot.Reactions)
	}
}

func TestSupersededAndDisabledCorrectionsAreNotLoaded(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, dislike := preferenceFixture(t, db)
	active := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)VALUES($1,$2,$3,$4,'mode','co-op','avoid',.9,1)`, active, user, dislike, game); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	snapshot, err := repository.LoadOverlaySnapshot(ctx, user)
	if err != nil || len(snapshot.Corrections) != 1 {
		t.Fatalf("active corrections=%d err=%v", len(snapshot.Corrections), err)
	}
	disableJob := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,user_id)VALUES($1,'fixture',$2,$3)`, disableJob, disableJob.String(), user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE preference_adjustments SET active=false,disabled_at=now(),disable_rebuild_job_id=$2,disable_previous_ranking_version=1 WHERE id=$1`, active, disableJob); err != nil {
		t.Fatal(err)
	}
	snapshot, err = repository.LoadOverlaySnapshot(ctx, user)
	if err != nil || len(snapshot.Corrections) != 0 {
		t.Fatalf("disabled corrections=%d err=%v", len(snapshot.Corrections), err)
	}
	var recommendation uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT recommendation_id FROM feedback_events WHERE id=$1`, dislike).Scan(&recommendation); err != nil {
		t.Fatal(err)
	}
	like := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at)VALUES($1,$2,$3,'like',$4,now()+interval '1 second')`, like, recommendation, user, like.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE preference_adjustments SET active=true,disabled_at=NULL,disable_rebuild_job_id=NULL,disable_previous_ranking_version=NULL WHERE id=$1`, active); err != nil {
		t.Fatal(err)
	}
	snapshot, err = repository.LoadOverlaySnapshot(ctx, user)
	if err != nil || len(snapshot.Corrections) != 0 || len(snapshot.Reactions) != 1 || snapshot.Reactions[0].Action != "like" {
		t.Fatalf("superseded snapshot=%+v err=%v", snapshot, err)
	}
}

func TestListPreferenceAdjustmentsDefaultsToEffectiveCorrectionsAndKeepsHistory(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, dislike := preferenceFixture(t, db)
	adjustmentID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)VALUES($1,$2,$3,$4,'mode','co-op','avoid',.9,1)`, adjustmentID, user, dislike, game); err != nil {
		t.Fatal(err)
	}
	var recommendation uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT recommendation_id FROM feedback_events WHERE id=$1`, dislike).Scan(&recommendation); err != nil {
		t.Fatal(err)
	}
	reminder := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at)VALUES($1,$2,$3,'remind_me',$4,now()+interval '1 second')`, reminder, recommendation, user, reminder.String()); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	effective, err := repository.List(ctx, user, false)
	if err != nil || len(effective) != 1 || effective[0].ID != adjustmentID {
		t.Fatalf("effective after reminder=%+v err=%v", effective, err)
	}
	disableJob := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,user_id)VALUES($1,'fixture',$2,$3)`, disableJob, disableJob.String(), user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE preference_adjustments SET active=false,disabled_at=now(),disable_rebuild_job_id=$2,disable_previous_ranking_version=1 WHERE id=$1`, adjustmentID, disableJob); err != nil {
		t.Fatal(err)
	}
	effective, err = repository.List(ctx, user, false)
	history, historyErr := repository.List(ctx, user, true)
	if err != nil || len(effective) != 0 || historyErr != nil || len(history) != 1 || history[0].ID != adjustmentID {
		t.Fatalf("disabled effective=%+v history=%+v err=%v/%v", effective, history, err, historyErr)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE preference_adjustments SET active=true,disabled_at=NULL,disable_rebuild_job_id=NULL,disable_previous_ranking_version=NULL WHERE id=$1`, adjustmentID); err != nil {
		t.Fatal(err)
	}
	like := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at)VALUES($1,$2,$3,'like',$4,now()+interval '2 seconds')`, like, recommendation, user, like.String()); err != nil {
		t.Fatal(err)
	}
	effective, err = repository.List(ctx, user, false)
	history, historyErr = repository.List(ctx, user, true)
	if err != nil || len(effective) != 0 || historyErr != nil || len(history) != 1 || history[0].ID != adjustmentID {
		t.Fatalf("superseded effective=%+v history=%+v err=%v/%v", effective, history, err, historyErr)
	}
}

func TestOverlayBuildIsVersionedIdempotentAndDeletionFenced(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, _ := preferenceFixture(t, db)
	repository, _ := NewSQLRepository(db)

	first, err := repository.RebuildOverlay(ctx, user, "fixture-empty")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.InputContentHash == "" || first.Positive != nil || first.Negative != nil {
		t.Fatalf("first=%+v", first)
	}
	repeated, err := repository.RebuildOverlay(ctx, user, "fixture-empty")
	if err != nil || repeated.ID != first.ID || repeated.Version != 1 {
		t.Fatalf("repeat=%+v err=%v", repeated, err)
	}
	var embeddingRows int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM preference_overlay_embeddings WHERE overlay_id=$1`, first.ID).Scan(&embeddingRows); err != nil || embeddingRows != 0 {
		t.Fatalf("empty embedding rows=%d err=%v", embeddingRows, err)
	}

	vector := make([]float32, store.EmbeddingDimensions)
	vector[5] = 1
	if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: game, Model: "fixture-embedding", ContentHash: "game-v1", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	second, err := repository.RebuildOverlay(ctx, user, "fixture-with-vector")
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 || second.ID == first.ID || second.Negative[5] != 1 || second.Metadata.NegativeSources != 1 {
		t.Fatalf("second=%+v", second)
	}
	reusedByContent, err := repository.RebuildOverlay(ctx, user, "another-request-same-input")
	if err != nil || reusedByContent.ID != second.ID {
		t.Fatalf("content reuse=%+v err=%v", reusedByContent, err)
	}
	var recommendation uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT recommendation_id FROM feedback_events WHERE user_id=$1 LIMIT 1`, user).Scan(&recommendation); err != nil {
		t.Fatal(err)
	}
	like := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at)VALUES($1,$2,$3,'like',$4,now()+interval '1 second')`, like, recommendation, user, like.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RebuildOverlay(ctx, user, "fixture-with-vector"); err == nil || !strings.Contains(err.Error(), "build key collision") {
		t.Fatalf("changed input reused a build key: %v", err)
	}
	third, err := repository.RebuildOverlay(ctx, user, "fixture-like")
	if err != nil || third.Version != 3 || third.Positive[5] != 1 || third.Negative != nil {
		t.Fatalf("third=%+v err=%v", third, err)
	}

	if _, err := db.Pool.Exec(ctx, `INSERT INTO deleted_user_tombstones(user_id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RebuildOverlay(ctx, user, "after-delete"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("rebuild after deletion=%v", err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM preference_overlays WHERE user_id=$1`, user).Scan(&count); err != nil || count != 0 {
		t.Fatalf("overlays after deletion=%d err=%v", count, err)
	}
}

func TestEnsureCurrentCreatesOneDurableEmptyOverlay(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	service, _ := NewService(repository, jobs.NewQueue(db))
	first, err := service.EnsureCurrent(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.EnsureCurrent(ctx, user)
	if err != nil || second.ID != first.ID || second.Version != 1 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestConcurrentOverlayBuildPublishesOneVersion(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	var wg sync.WaitGroup
	values := make(chan Overlay, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := repository.RebuildOverlay(ctx, user, "concurrent")
			values <- value
			errs <- err
		}()
	}
	wg.Wait()
	close(values)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id uuid.UUID
	for value := range values {
		if id == uuid.Nil {
			id = value.ID
		}
		if value.ID != id || value.Version != 1 {
			t.Fatalf("concurrent overlay=%+v expected id=%s", value, id)
		}
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM preference_overlays WHERE user_id=$1`, user).Scan(&count); err != nil || count != 1 {
		t.Fatalf("overlay count=%d err=%v", count, err)
	}
}

func TestOverlayBuildSelectsOneDeterministicEmbeddingModel(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, firstGame, _ := preferenceFixture(t, db)
	var profile uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT id FROM preference_profiles WHERE user_id=$1`, user).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	secondGame := uuid.New()
	secondRecommendation := uuid.New()
	secondFeedback := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,$2,'Second model','fixture',now())`, secondGame, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score)VALUES($1,$2,$3,$4,'fixture',10)`, secondRecommendation, user, secondGame, profile); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'dislike',$4)`, secondFeedback, secondRecommendation, user, secondFeedback.String()); err != nil {
		t.Fatal(err)
	}
	firstVector, secondVector := make([]float32, store.EmbeddingDimensions), make([]float32, store.EmbeddingDimensions)
	firstVector[3], secondVector[7] = 1, 1
	if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: firstGame, Model: "model-z", ContentHash: "z", Vector: firstVector}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: secondGame, Model: "model-a", ContentHash: "a", Vector: secondVector}); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	overlay, err := repository.RebuildOverlay(ctx, user, "mixed-models")
	if err != nil {
		t.Fatal(err)
	}
	if overlay.Metadata.NegativeSources != 1 || overlay.Metadata.MissingVectors != 1 || len(overlay.Negative) != store.EmbeddingDimensions || overlay.Negative[7] != 1 {
		t.Fatalf("mixed-model overlay=%+v vector[3]=%v vector[7]=%v", overlay.Metadata, overlay.Negative[3], overlay.Negative[7])
	}
	var model string
	if err := db.Pool.QueryRow(ctx, `SELECT model FROM preference_overlay_embeddings WHERE overlay_id=$1 AND polarity='negative'`, overlay.ID).Scan(&model); err != nil || model != "model-a" {
		t.Fatalf("selected model=%q err=%v", model, err)
	}
}

func TestPreferenceOverlayJobRetryRecoversCommittedBuildBeforeReadingChangedSnapshot(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, _ := preferenceFixture(t, db)
	vector := make([]float32, store.EmbeddingDimensions)
	vector[4] = 1
	if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: game, Model: "fixture", ContentHash: "negative", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	queue := jobs.NewQueue(db)
	service, _ := NewService(repository, queue)
	payload, _ := json.Marshal(OverlayJobPayload{Version: 1, UserID: user})
	jobID, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: OverlayJobKind, Stage: "preferences", UserID: user, Priority: 100, IdempotencyKey: "overlay-retry-fixture", Payload: payload, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	firstLease, err := queue.Claim(ctx, jobs.LaneInteractive, "lost-result", time.Minute)
	if err != nil || firstLease == nil || firstLease.ID != jobID {
		t.Fatalf("first lease=%+v err=%v", firstLease, err)
	}
	committed, err := repository.RebuildOverlay(ctx, user, firstLease.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := queue.Fail(ctx, *firstLease, errors.New("lost after durable overlay commit"), time.Now()); err != nil || outcome != jobs.FailureRetryScheduled {
		t.Fatalf("retry outcome=%s err=%v", outcome, err)
	}
	var recommendation uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT recommendation_id FROM feedback_events WHERE user_id=$1 LIMIT 1`, user).Scan(&recommendation); err != nil {
		t.Fatal(err)
	}
	like := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at)VALUES($1,$2,$3,'like',$4,now()+interval '1 second')`, like, recommendation, user, like.String()); err != nil {
		t.Fatal(err)
	}
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "overlay-retry", jobs.WithPollInterval(5*time.Millisecond))
	if err := service.RegisterHandlers(worker); err != nil {
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		var result []byte
		if err := db.Pool.QueryRow(ctx, `SELECT status,result FROM sync_jobs WHERE id=$1`, jobID).Scan(&status, &result); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			var value struct {
				PreferenceOverlayID uuid.UUID `json:"preferenceOverlayId"`
			}
			if err := json.Unmarshal(result, &value); err != nil || value.PreferenceOverlayID != committed.ID {
				t.Fatalf("retry result=%s committed=%s err=%v", result, committed.ID, err)
			}
			break
		}
		if status == "dead" || time.Now().After(deadline) {
			t.Fatalf("retry status=%s result=%s", status, result)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM preference_overlays WHERE user_id=$1`, user).Scan(&count); err != nil || count != 1 {
		t.Fatalf("overlay versions=%d err=%v", count, err)
	}
}

func TestPreferenceOverlayJobBuildsAndRecordsSafeResult(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	queue := jobs.NewQueue(db)
	service, _ := NewService(repository, queue)
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "overlay-fixture", jobs.WithPollInterval(5*time.Millisecond))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(OverlayJobPayload{Version: 1, UserID: user})
	jobID, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: OverlayJobKind, Stage: "preferences", UserID: user, Priority: 100, IdempotencyKey: "overlay-job-fixture", Payload: payload, MaxAttempts: 2})
	if err != nil {
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		var result []byte
		if err := db.Pool.QueryRow(ctx, `SELECT status,result FROM sync_jobs WHERE id=$1`, jobID).Scan(&status, &result); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			var value struct {
				PreferenceOverlayID      uuid.UUID `json:"preferenceOverlayId"`
				PreferenceOverlayVersion int64     `json:"preferenceOverlayVersion"`
				InputContentHash         string    `json:"inputContentHash"`
			}
			if err := json.Unmarshal(result, &value); err != nil || value.PreferenceOverlayID == uuid.Nil || value.PreferenceOverlayVersion != 1 || value.InputContentHash == "" {
				t.Fatalf("result=%s decoded=%+v err=%v", result, value, err)
			}
			return
		}
		if status == "dead" || time.Now().After(deadline) {
			t.Fatalf("overlay job status=%s result=%s", status, result)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
