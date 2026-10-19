package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/preferences"
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
	command := Command{UserID: userID, RecommendationID: recommendationID, Action: ActionLike, Reason: "genre", Reasons: []string{"genre", "setting"}, FreeText: "More like this", IdempotencyKey: "stable-browser-event"}
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
	if !reflect.DeepEqual(first.Reasons, []string{"genre", "setting"}) || !reflect.DeepEqual(second.Reasons, first.Reasons) {
		t.Fatalf("persisted reasons first=%v second=%v", first.Reasons, second.Reasons)
	}
	var snapshot struct {
		Score float64 `json:"score"`
		Name  string  `json:"name"`
	}
	if err := json.Unmarshal(first.Snapshot, &snapshot); err != nil || snapshot.Score != 88 || snapshot.Name != "Original name" {
		t.Fatalf("snapshot=%#v err=%v raw=%s", snapshot, err, first.Snapshot)
	}
}

func TestSQLRecordNormalizesNilReasonsAndReplays(t *testing.T) {
	for _, test := range []struct {
		name, action, text string
	}{
		{name: "dislike with comment", action: ActionDislike, text: "Prefer solo exploration"},
		{name: "immediate reminder", action: ActionRemind},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openFeedbackStore(t)
			ctx := context.Background()
			userID, profileID := uuid.New(), uuid.New()
			if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, userID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, profileID, userID); err != nil {
				t.Fatal(err)
			}
			gameID := insertFeedbackGame(t, db, 90100, "Nil reasons fixture")
			recommendationID := insertFeedbackRanking(t, db, userID, profileID, gameID, 1, 15, "nil-reasons-ranking")
			repository, _ := NewSQLRepository(db)
			command := Command{UserID: userID, RecommendationID: recommendationID, Action: test.action, FreeText: test.text, IdempotencyKey: "nil-reasons-command"}
			first, created, err := repository.Record(ctx, command)
			if err != nil || !created || first.Reasons == nil || len(first.Reasons) != 0 || first.FreeText != test.text || first.Action != test.action {
				t.Fatalf("first record: created=%v reasons=%v err=%v", created, first.Reasons, err)
			}
			var storedReasons string
			if err := db.Pool.QueryRow(ctx, `SELECT reasons::text FROM feedback_events WHERE id=$1`, first.ID).Scan(&storedReasons); err != nil || storedReasons != "{}" {
				t.Fatalf("stored reasons=%q err=%v", storedReasons, err)
			}
			second, created, err := repository.Record(ctx, command)
			if err != nil || created || second.ID != first.ID || second.Reasons == nil || len(second.Reasons) != 0 {
				t.Fatalf("replay: created=%v reasons=%v err=%v", created, second.Reasons, err)
			}
			var count int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM feedback_events WHERE user_id=$1`, userID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("event count=%d err=%v", count, err)
			}
		})
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
	if version, err := repo.FinalizeRanking(ctx, eventID); err != nil || version != 2 {
		t.Fatalf("finalized version=%d err=%v", version, err)
	}
	_ = insertFeedbackRanking(t, db, userID, profileID, gameA, 3, 99, "later-unrelated-run")
	diff, err := repo.RankingDiff(ctx, userID, 1, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if diff.RebuildStatus != "succeeded" || diff.CurrentVersion != 2 || diff.Classification == nil || !diff.Classification.Actionable || len(diff.Added) != 1 || diff.Added[0] != newC || len(diff.Removed) != 1 || diff.Removed[0] != oldA || len(diff.Reordered) != 1 || diff.Reordered[0] != newB {
		t.Fatalf("diff=%#v", diff)
	}
}

func TestClassificationDoesNotDuplicateOverlayDirectGameSignals(t *testing.T) {
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
	otherVector := make([]float32, profilepkg.Dimensions)
	otherVector[1] = 1
	for gameID, embedding := range map[uuid.UUID][]float32{highGame: vector, lowGame: otherVector} {
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: gameID.String(), Vector: embedding}); err != nil {
			t.Fatal(err)
		}
	}
	lastPlayed := time.Date(2020, 10, 18, 12, 0, 0, 0, time.UTC)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,source,last_successful_fetch_at)VALUES($1,$2,10000,0,$3,'test',now())`, userID, highGame, lastPlayed); err != nil {
		t.Fatal(err)
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
	if len(signals) != 1 || signals[0].GameID != highGame || signals[0].Likes != 0 || signals[0].Dislikes != 0 {
		t.Fatalf("activity signals include feedback-only evidence: %+v", signals)
	}
	preferenceRepository, _ := preferences.NewSQLRepository(db)
	snapshot, err := preferenceRepository.LoadOverlaySnapshot(ctx, userID)
	if err != nil || len(snapshot.Reactions) != 2 {
		t.Fatalf("initial overlay reactions=%+v err=%v", snapshot.Reactions, err)
	}
	for _, reaction := range snapshot.Reactions {
		if reaction.Action != ActionLike {
			t.Fatalf("initial reaction=%+v", reaction)
		}
	}
	for _, gameID := range []uuid.UUID{highGame, lowGame} {
		rec := highRec
		if gameID == lowGame {
			rec = lowRec
		}
		event := uuid.New()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'dislike',$4)`, event, rec, userID, event.String()); err != nil {
			t.Fatal(err)
		}
		polarity := "negative"
		if gameID == lowGame {
			polarity = "positive"
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash)VALUES($1,'genre',$2,.9,true,'test','dislike')`, event, polarity); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err = preferenceRepository.LoadOverlaySnapshot(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Reactions) != 2 {
		t.Fatalf("negative overlay reactions=%+v", snapshot.Reactions)
	}
	for _, reaction := range snapshot.Reactions {
		if reaction.Action != ActionDislike {
			t.Fatalf("negative reaction=%+v", reaction)
		}
	}
	vectors := preferences.BuildOverlay(snapshot)
	if vectors.PositiveSources != 0 || vectors.NegativeSources != 2 {
		t.Fatalf("classification changed direct overlay weight: %+v", vectors)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'dislike','second-dislike')`, uuid.New(), highRec, userID); err != nil {
		t.Fatal(err)
	}
	snapshot, err = preferenceRepository.LoadOverlaySnapshot(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Reactions) != 2 || preferences.BuildOverlay(snapshot).NegativeSources != 2 {
		t.Fatalf("repeated direct dislike stacked: %+v", snapshot.Reactions)
	}
}

func feedbackAdjustmentFixture(t *testing.T) (*store.Store, *SQLRepository, Event) {
	t.Helper()
	db := openFeedbackStore(t)
	ctx := context.Background()
	user, profile := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, profile, user); err != nil {
		t.Fatal(err)
	}
	game := insertFeedbackGame(t, db, 99, "Cooperative game")
	rec := insertFeedbackRanking(t, db, user, profile, game, 1, 90, "adjustment")
	repo, _ := NewSQLRepository(db)
	event, _, err := repo.Record(ctx, Command{UserID: user, RecommendationID: rec, Action: ActionDislike, FreeText: "не люблю коопы", IdempotencyKey: "coop"})
	if err != nil {
		t.Fatal(err)
	}
	return db, repo, event
}

func TestCoopDislikeCreatesExactlyOneAdjustment(t *testing.T) {
	db, repo, event := feedbackAdjustmentFixture(t)
	ctx := context.Background()
	calls := 0
	service, _ := NewService(repo, fakeClassifier{calls: &calls, output: ai.FeedbackClassification{Taxonomy: "multiplayer", Polarity: "negative", Confidence: .92, Actionable: true, Preference: &preferences.Target{Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid"}, EvidenceKeys: []string{"feedback-text"}}}, "test", &fakeQueue{})
	for range 2 {
		if err := service.ProcessClassification(ctx, event.ID); err != nil {
			t.Fatal(err)
		}
	}
	pr, _ := preferences.NewSQLRepository(db)
	rows, err := pr.List(ctx, event.UserID, true)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(rows) != 1 {
		t.Fatal("classification/projection not idempotent")
	}
	a := rows[0]
	if !a.Active || a.Target != preferences.ModeCoop || a.Dimension != "mode" || a.Direction != "avoid" || a.Confidence != .92 || a.Strength != 1 || a.SourceFeedbackID != event.ID || a.SourceGameID != event.GameID || a.UserID != event.UserID {
		t.Fatal("incorrect preference projection")
	}
}

func TestNonActionableFeedbackNeverCreatesStoredAdjustment(t *testing.T) {
	db, repo, original := feedbackAdjustmentFixture(t)
	ctx := context.Background()
	valid := ai.FeedbackClassification{Taxonomy: "multiplayer", Polarity: "negative", Confidence: .92, Actionable: true, Preference: &preferences.Target{Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid"}, EvidenceKeys: []string{"feedback-text"}}
	invalid := valid
	invalid.Taxonomy = "single-player"
	for _, test := range []struct {
		name, action, reason, text string
		output                     ai.FeedbackClassification
		err                        error
		want                       preferences.Mode
	}{
		{"like", ActionLike, "", "кооп", valid, nil, ""},
		{"remind", ActionRemind, "", "", ai.FeedbackClassification{}, nil, ""},
		{"contradiction", ActionDislike, "multiplayer", "кооп", invalid, nil, ""},
		{"refusal", ActionDislike, "multiplayer", "кооп", ai.FeedbackClassification{}, ai.ErrRefusal, ""},
		{"incomplete", ActionDislike, "multiplayer", "кооп", ai.FeedbackClassification{}, ai.ErrIncomplete, ""},
		{"unavailable free text", ActionDislike, "", "кооп", ai.FeedbackClassification{}, ai.ErrCapabilityDisabled, ""},
		{"nonactionable", ActionDislike, "multiplayer", "maybe", ai.FeedbackClassification{Taxonomy: "multiplayer", Polarity: "neutral", Confidence: .3, EvidenceKeys: []string{"feedback-text"}}, nil, ""},
		{"predefined reason", ActionDislike, "multiplayer", "", ai.FeedbackClassification{}, nil, preferences.ModeMultiplayer},
		{"unavailable reason", ActionDislike, "multiplayer", "кооп", ai.FeedbackClassification{}, ai.ErrCapabilityDisabled, preferences.ModeMultiplayer},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, _, err := repo.Record(ctx, Command{UserID: original.UserID, RecommendationID: original.RecommendationID, Action: test.action, Reason: test.reason, FreeText: test.text, IdempotencyKey: test.name})
			if err != nil {
				t.Fatal(err)
			}
			s, _ := NewService(repo, fakeClassifier{output: test.output, err: test.err}, "test", &fakeQueue{})
			if err := s.ProcessClassification(ctx, event.ID); err != nil {
				t.Fatal(err)
			}
			stored, err := repo.Classification(ctx, event.ID)
			if err != nil {
				t.Fatal(err)
			}
			pr, _ := preferences.NewSQLRepository(db)
			a, err := pr.ForFeedback(ctx, event.UserID, event.ID)
			if test.want == "" {
				if !errors.Is(err, pgx.ErrNoRows) || stored.Preference != nil {
					t.Fatal("non-actionable feedback created a preference")
				}
			} else if err != nil || a.Target != test.want || a.Confidence != 1 || stored.Model != "deterministic-fallback" {
				t.Fatal("fallback preference missing or has incorrect provenance")
			}
			if _, err := repo.Event(ctx, event.ID); err != nil {
				t.Fatal("classification failure lost direct event")
			}
		})
	}
}

func TestClassificationResumeRepairsMissingProjectionWithoutRecallingAI(t *testing.T) {
	db, repo, event := feedbackAdjustmentFixture(t)
	ctx := context.Background()
	calls := 0
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,schema_version,preference_dimension,preference_target,preference_direction)VALUES($1,'multiplayer','negative',.92,true,'test','saved',2,'mode','co-op','avoid')`, event.ID); err != nil {
		t.Fatal(err)
	}
	s, _ := NewService(repo, fakeClassifier{calls: &calls, err: ai.ErrCapabilityDisabled}, "test", &fakeQueue{})
	if err := s.ProcessClassification(ctx, event.ID); err != nil {
		t.Fatal(err)
	}
	pr, _ := preferences.NewSQLRepository(db)
	a, err := pr.ForFeedback(ctx, event.UserID, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	job := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key)VALUES($1,'preferences.disable','disable')`, job); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE preference_adjustments SET active=false,disabled_at=now(),disable_rebuild_job_id=$2,disable_previous_ranking_version=1 WHERE id=$1`, a.ID, job); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessClassification(ctx, event.ID); err != nil {
		t.Fatal(err)
	}
	a, err = pr.ForFeedback(ctx, event.UserID, event.ID)
	if err != nil || a.Active || calls != 0 {
		t.Fatal("cached replay recalled AI or reactivated adjustment")
	}
}

func TestClassificationResumePreservesLegacyHistoryWithoutInference(t *testing.T) {
	db, repo, event := feedbackAdjustmentFixture(t)
	ctx := context.Background()
	calls := 0
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,schema_version)VALUES($1,'multiplayer','negative',.92,true,'legacy','saved',1)`, event.ID); err != nil {
		t.Fatal(err)
	}
	s, _ := NewService(repo, fakeClassifier{calls: &calls}, "test", &fakeQueue{})
	if err := s.ProcessClassification(ctx, event.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Classification(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SchemaVersion != 1 || stored.Preference != nil || stored.Model != "legacy" || calls != 0 {
		t.Fatal("legacy history changed")
	}
	pr, _ := preferences.NewSQLRepository(db)
	rows, err := pr.List(ctx, event.UserID, true)
	if err != nil || len(rows) != 0 {
		t.Fatal("legacy classification inferred adjustment")
	}
}

func TestClassificationAndAdjustmentAtomicWinningValue(t *testing.T) {
	db, repo, event := feedbackAdjustmentFixture(t)
	ctx := context.Background()
	value := Classification{FeedbackID: event.ID, Taxonomy: "multiplayer", Polarity: "negative", Confidence: .92, Actionable: true, Model: "test", InputContentHash: "winner", SchemaVersion: 2, Preference: &preferences.Target{Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid"}}
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION reject_adjustment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected adjustment failure'; END $$; CREATE TRIGGER reject_adjustment BEFORE INSERT ON preference_adjustments FOR EACH ROW EXECUTE FUNCTION reject_adjustment()`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveClassification(ctx, value); err == nil {
		t.Fatal("missing injected failure")
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM feedback_classifications WHERE feedback_id=$1`, event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("classification committed without adjustment")
	}
	if _, err := db.Pool.Exec(ctx, `DROP TRIGGER reject_adjustment ON preference_adjustments`); err != nil {
		t.Fatal(err)
	}
	other := value
	other.InputContentHash = "other"
	other.Preference = &preferences.Target{Dimension: "mode", Target: preferences.ModeOnline, Direction: "avoid"}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, v := range []Classification{value, other} {
		wg.Go(func() { errs <- repo.SaveClassification(ctx, v) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	stored, err := repo.Classification(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := preferences.NewSQLRepository(db)
	a, err := pr.ForFeedback(ctx, event.UserID, event.ID)
	if err != nil || stored.Preference == nil || a.Target != stored.Preference.Target {
		t.Fatal("adjustment does not match immutable winner")
	}
}

func TestIdempotentFeedbackDoesNotStack(t *testing.T) {
	db := openFeedbackStore(t)
	ctx := context.Background()
	userID, profileID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	gameID := insertFeedbackGame(t, db, 90444, "Idempotent overlay fixture")
	recommendationID := insertFeedbackRanking(t, db, userID, profileID, gameID, 1, 80, "idempotent-overlay")
	repository, _ := NewSQLRepository(db)
	command := Command{UserID: userID, RecommendationID: recommendationID, Action: ActionLike, IdempotencyKey: "one-effective-reaction"}
	first, created, err := repository.Record(ctx, command)
	if err != nil || !created {
		t.Fatalf("first=%+v created=%v err=%v", first, created, err)
	}
	second, created, err := repository.Record(ctx, command)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("second=%+v created=%v err=%v", second, created, err)
	}
	preferenceRepository, _ := preferences.NewSQLRepository(db)
	snapshot, err := preferenceRepository.LoadOverlaySnapshot(ctx, userID)
	if err != nil || len(snapshot.Reactions) != 1 || snapshot.Reactions[0].FeedbackID != first.ID {
		t.Fatalf("effective reactions=%+v err=%v", snapshot.Reactions, err)
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
