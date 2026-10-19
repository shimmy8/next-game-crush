package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
)

type semanticClassifierFunc func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error)

func (fn semanticClassifierFunc) ClassifyAchievementSemantics(ctx context.Context, input ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
	return fn(ctx, input)
}

func TestClassificationJobIsSharedAndGameScoped(t *testing.T) {
	service, err := NewService(&SQLRepository{}, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, nil
	}), "semantic-model", &jobs.Queue{})
	if err != nil {
		t.Fatal(err)
	}
	gameID := uuid.New()
	first, err := service.PlanClassificationJob(context.Background(), gameID, strings.Repeat("a", 64), 50)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.PlanClassificationJob(context.Background(), gameID, strings.Repeat("a", 64), 90)
	if err != nil {
		t.Fatal(err)
	}
	if first.UserID != uuid.Nil || second.UserID != uuid.Nil || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("shared job identity differs: first=%+v second=%+v", first, second)
	}
	var payload map[string]any
	if err := json.Unmarshal(first.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 3 || payload["version"] != float64(1) || payload["game_id"] != gameID.String() || payload["definition_set_hash"] != strings.Repeat("a", 64) {
		t.Fatalf("payload=%#v", payload)
	}
	for _, encoded := range []string{string(first.Payload), first.IdempotencyKey} {
		if strings.Contains(encoded, "user_id") || strings.Contains(encoded, "steam_id") {
			t.Fatalf("user identity leaked: %s", encoded)
		}
	}
	result, err := json.Marshal(classificationJobResult{PublishedHookPending: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "user_id") || strings.Contains(string(result), "steam_id") {
		t.Fatalf("user identity leaked in result: %s", result)
	}
}

func TestRecordAchievementSnapshotRollsBackWhenClassificationEnqueueFails(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, ai.ErrCapabilityDisabled
	}), "semantic-model", jobs.NewQueue(db))
	summary := steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "ending", Name: "Ending", Description: "Reach an ending", Achieved: true}}}
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION reject_classification_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='achievement.classify' THEN RAISE EXCEPTION 'injected enqueue failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_classification_enqueue BEFORE INSERT ON sync_jobs FOR EACH ROW EXECUTE FUNCTION reject_classification_enqueue()`); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordAchievementSnapshot(ctx, userID, gameID, summary, time.Now()); err == nil {
		t.Fatal("snapshot unexpectedly succeeded despite enqueue failure")
	}
	for _, table := range []string{"achievement_summaries", "user_achievement_unlocks", "game_achievement_definitions", "user_game_completion_evidence", "user_game_completion_projections", "sync_jobs"} {
		var count int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s retained %d rows after enqueue failure", table, count)
		}
	}
	if _, err := db.Pool.Exec(ctx, `DROP TRIGGER reject_classification_enqueue ON sync_jobs`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := service.RecordAchievementSnapshot(ctx, userID, gameID, summary, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var jobsCount int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='achievement.classify' AND user_id IS NULL`).Scan(&jobsCount); err != nil || jobsCount != 1 {
		t.Fatalf("classification jobs=%d err=%v", jobsCount, err)
	}
}

func TestRecordAchievementSnapshotRecoversCurrentClassificationIntent(t *testing.T) {
	for _, scenario := range []string{"orphan", "old model", "old prompt", "old schema", "old lexical rules", "old definitions", "succeeded job", "dead job", "pending job", "active current", "empty definitions"} {
		t.Run(scenario, func(t *testing.T) {
			db := openCompletionStore(t)
			ctx := context.Background()
			userID, gameID := seedCompletionGame(t, db, nil)
			repository, _ := NewSQLRepository(db)
			service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
				return ai.AchievementSemanticClassification{}, ai.ErrCapabilityDisabled
			}), "semantic-model", jobs.NewQueue(db))
			summary := steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "ending", Name: "Ending", Description: "Reach an ending", Achieved: true}}}
			if scenario == "empty definitions" {
				summary.Achievements[0].Name, summary.Achievements[0].Description = "", ""
			}
			change, err := repository.SaveSteamSnapshot(ctx, userID, gameID, summary, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "old ") || scenario == "active current" {
				model, prompt, schema, lexical := "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion
				switch scenario {
				case "old model":
					model += "-old"
				case "old prompt":
					prompt += "-old"
				case "old schema":
					schema += "-old"
				case "old lexical rules":
					lexical += "-old"
				}
				staged := []StagedClassification{{APIName: "ending", DefinitionContentHash: definitionHashFor(t, db, gameID, "ending"), Class: ai.AchievementSemanticBaseGameEnding, Source: ClassificationSourceLexical}}
				if _, err := repository.PublishClassificationGeneration(ctx, gameID, model, prompt, schema, lexical, change.DefinitionSetHash, staged, time.Now()); err != nil {
					t.Fatal(err)
				}
				if scenario == "old definitions" {
					if _, err := db.Pool.Exec(ctx, `UPDATE achievement_semantic_classification_generations SET definition_set_hash=$2 WHERE game_id=$1`, gameID, strings.Repeat("0", 64)); err != nil {
						t.Fatal(err)
					}
				}
			}
			spec, err := service.PlanClassificationJob(ctx, gameID, change.DefinitionSetHash, 80)
			if err != nil {
				t.Fatal(err)
			}
			var priorJob uuid.UUID
			if strings.HasSuffix(scenario, " job") {
				priorJob, err = jobs.NewQueue(db).Enqueue(ctx, spec)
				if err != nil {
					t.Fatal(err)
				}
				if scenario != "pending job" {
					status := strings.TrimSuffix(scenario, " job")
					if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status=$2,attempts=max_attempts,result='{"published_hook_pending":false}' WHERE id=$1`, priorJob, status); err != nil {
						t.Fatal(err)
					}
				}
			}
			for range 2 {
				if err := service.RecordAchievementSnapshot(ctx, userID, gameID, summary, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='achievement.classify'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if scenario == "active current" || scenario == "empty definitions" {
				if count != 0 {
					t.Fatalf("classification jobs=%d want=0", count)
				}
				return
			}
			if count != 1 {
				t.Fatalf("classification jobs=%d want=1", count)
			}
			var id uuid.UUID
			var status string
			var claimable, shared bool
			var payload []byte
			if err := db.Pool.QueryRow(ctx, `SELECT id,status,attempts<max_attempts,user_id IS NULL,payload FROM sync_jobs WHERE idempotency_key=$1`, spec.IdempotencyKey).Scan(&id, &status, &claimable, &shared, &payload); err != nil {
				t.Fatal(err)
			}
			if status != "queued" || !claimable || !shared || (priorJob != uuid.Nil && priorJob != id) || strings.Contains(string(payload), userID.String()) {
				t.Fatalf("recovered intent status=%s claimable=%v shared=%v reused=%v", status, claimable, shared, priorJob == uuid.Nil || priorJob == id)
			}
		})
	}
}

func TestRecordAchievementSnapshotKeepsClassificationIntentAfterProjectionFailure(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, ai.ErrCapabilityDisabled
	}), "semantic-model", jobs.NewQueue(db))
	summary := steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "ending", Name: "Ending", Description: "Reach an ending", Achieved: true}}}
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION reject_snapshot_projection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected projection failure'; END $$;
		CREATE TRIGGER reject_snapshot_projection BEFORE INSERT ON user_game_completion_projections FOR EACH ROW EXECUTE FUNCTION reject_snapshot_projection()`); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordAchievementSnapshot(ctx, userID, gameID, summary, time.Now()); err == nil {
		t.Fatal("snapshot unexpectedly succeeded despite projection failure")
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='achievement.classify' AND user_id IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("classification intent after projection failure=%d err=%v", count, err)
	}
	if _, err := db.Pool.Exec(ctx, `DROP TRIGGER reject_snapshot_projection ON user_game_completion_projections`); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordAchievementSnapshot(ctx, userID, gameID, summary, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='achievement.classify'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("classification intent after retry=%d err=%v", count, err)
	}
}

func TestClassificationUnavailablePublishesLocalFirstGenerationOnly(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	prior, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "reuse", Name: "Collector", Description: "Find every token"}}}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PublishClassificationGeneration(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, prior.DefinitionSetHash, []StagedClassification{{APIName: "reuse", DefinitionContentHash: definitionHashFor(t, db, gameID, "reuse"), Class: ai.AchievementSemanticOptionalChallenge, Source: ClassificationSourceOpenAI}}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE achievement_semantic_classification_generations SET status='superseded' WHERE game_id=$1 AND status='active'`, gameID); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, ai.ErrCapabilityDisabled
	}), "semantic-model", jobs.NewQueue(db))
	if err != nil {
		t.Fatal(err)
	}
	published := 0
	service.SetClassificationPublishedHook(func(_ context.Context, got uuid.UUID) error {
		if got != gameID {
			t.Fatalf("hook game=%s want=%s", got, gameID)
		}
		published++
		return nil
	})
	change, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{
		{APIName: "reuse", Name: "Collector", Description: "Find every token"},
		{APIName: "ending", Name: "Ending", Description: "Reach an ending"},
		{APIName: "mystery", Name: "Hidden truth", Description: "Discover something"},
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: change.DefinitionSetHash}); err != nil {
		t.Fatal(err)
	}
	rows, err := repository.LoadActiveClassifications(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || len(rows) != 3 || rows["reuse"].Source != ClassificationSourceReused || rows["ending"].Source != ClassificationSourceLexical || rows["mystery"].Source != ClassificationSourceUnknown {
		t.Fatalf("published=%d rows=%+v", published, rows)
	}

	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,basis,algorithm_version,updated_at) VALUES($1,$2,1,false,1.5,'ending','completion-v1',now())`, userID, gameID); err != nil {
		t.Fatal(err)
	}
	changed, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{
		{APIName: "reuse", Name: "Collector", Description: "Find every token"},
		{APIName: "ending", Name: "Ending", Description: "Reach an ending"},
		{APIName: "mystery", Name: "Changed truth", Description: "Still ambiguous"},
	}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: changed.DefinitionSetHash}); err != nil {
		t.Fatal(err)
	}
	var active, projection int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM achievement_semantic_classification_generations WHERE game_id=$1 AND status='active'`, gameID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM user_game_completion_projections WHERE user_id=$1 AND game_id=$2 AND weight=1.5`, userID, gameID).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if published != 1 || active != 1 || projection != 1 {
		t.Fatalf("published=%d active=%d projection=%d", published, active, projection)
	}
	service.classifier = semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, &ai.RequestError{StatusCode: 503, Temporary: true}
	})
	changedAgain, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{
		{APIName: "reuse", Name: "Collector", Description: "Find every token"},
		{APIName: "ending", Name: "Ending", Description: "Reach an ending"},
		{APIName: "mystery", Name: "Changed again", Description: "Still ambiguous"},
	}}, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: changedAgain.DefinitionSetHash}); err == nil {
		t.Fatal("temporary provider failure with last-good generation was swallowed")
	}
	if published != 1 {
		t.Fatalf("temporary failure invoked hook: %d", published)
	}
}

func TestClassificationTemporaryUnavailablePublishesFirstLocalGeneration(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	change, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{
		{APIName: "ending", Name: "Ending", Description: "Reach an ending"},
		{APIName: "mystery", Name: "Mystery", Description: "Ambiguous"},
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, &ai.RequestError{StatusCode: 503, Temporary: true}
	}), "semantic-model", jobs.NewQueue(db))
	if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: change.DefinitionSetHash}); err != nil {
		t.Fatal(err)
	}
	rows, err := repository.LoadActiveClassifications(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows["ending"].Source != ClassificationSourceLexical || rows["mystery"].Source != ClassificationSourceUnknown {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestClassificationGenerationPublishesCompleteSetOnce(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	old := steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "reuse", Name: "Old", Description: "Optional"}}}
	oldChange, err := repository.SaveSteamSnapshot(ctx, userID, gameID, old, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PublishClassificationGeneration(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, oldChange.DefinitionSetHash, []StagedClassification{{APIName: "reuse", DefinitionContentHash: definitionHashFor(t, db, gameID, "reuse"), Class: ai.AchievementSemanticOptionalChallenge, Source: ClassificationSourceOpenAI}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	current := steam.AchievementSummary{Achievements: []steam.Achievement{
		{APIName: "reuse", Name: "Old", Description: "Optional"},
		{APIName: "lexical", Name: "Winner", Description: "Finish the game"},
		{APIName: "provider", Name: "The Path", Description: "A cryptic finale"},
	}}
	change, err := repository.SaveSteamSnapshot(ctx, userID, gameID, current, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	classifier := semanticClassifierFunc(func(_ context.Context, input ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		results := make([]ai.AchievementSemanticResult, 0, len(input.Achievements))
		for i := len(input.Achievements) - 1; i >= 0; i-- {
			results = append(results, ai.AchievementSemanticResult{EvidenceKey: input.Achievements[i].EvidenceKey, Class: ai.AchievementSemanticBaseGameEnding})
		}
		return ai.AchievementSemanticClassification{Results: results}, nil
	})
	service, _ := NewService(repository, classifier, "semantic-model", jobs.NewQueue(db))
	published := 0
	service.SetClassificationPublishedHook(func(context.Context, uuid.UUID) error { published++; return nil })
	for range 2 {
		if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: change.DefinitionSetHash}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := repository.LoadActiveClassifications(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || len(rows) != 3 || rows["reuse"].Source != ClassificationSourceReused || rows["lexical"].Source != ClassificationSourceLexical || rows["provider"].Source != ClassificationSourceOpenAI {
		t.Fatalf("published=%d rows=%+v", published, rows)
	}
	var active, superseded, classifications int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='active'),count(*) FILTER (WHERE status='superseded') FROM achievement_semantic_classification_generations WHERE game_id=$1`, gameID).Scan(&active, &superseded); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM achievement_semantic_classifications WHERE game_id=$1`, gameID).Scan(&classifications); err != nil {
		t.Fatal(err)
	}
	if active != 1 || superseded != 1 || classifications != 4 {
		t.Fatalf("active=%d superseded=%d classifications=%d", active, superseded, classifications)
	}
}

func TestClassificationGenerationActivatesAtomicallyAndPreservesLastGood(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	old := steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "reuse", Name: "Old", Description: "Optional"}, {APIName: "lexical", Name: "Win", Description: "Finish the game"}}}
	oldChange, err := repository.SaveSteamSnapshot(ctx, userID, gameID, old, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	oldRows := []StagedClassification{
		{APIName: "reuse", DefinitionContentHash: definitionHashFor(t, db, gameID, "reuse"), Class: ai.AchievementSemanticOptionalChallenge, Source: ClassificationSourceOpenAI},
		{APIName: "lexical", DefinitionContentHash: definitionHashFor(t, db, gameID, "lexical"), Class: ai.AchievementSemanticBaseGameCompletion, Source: ClassificationSourceLexical},
	}
	if _, err := repository.PublishClassificationGeneration(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, oldChange.DefinitionSetHash, oldRows, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,basis,algorithm_version,updated_at) VALUES($1,$2,1,false,1.5,'ending','completion-v1',now())`, userID, gameID); err != nil {
		t.Fatal(err)
	}
	changed, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{
		{APIName: "reuse", Name: "Old", Description: "Optional"},
		{APIName: "lexical", Name: "Win", Description: "Finish the game"},
		{APIName: "unresolved", Name: "Secret", Description: "Cryptic"},
	}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, ai.ErrInvalidOutput
	}), "semantic-model", jobs.NewQueue(db))
	if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: changed.DefinitionSetHash}); !errors.Is(err, ai.ErrInvalidOutput) {
		t.Fatalf("error=%v want invalid output", err)
	}
	rows, err := repository.LoadActiveClassifications(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion)
	if err != nil {
		t.Fatal(err)
	}
	var active, staged, projection int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='active'),count(*) FILTER (WHERE status='staged') FROM achievement_semantic_classification_generations WHERE game_id=$1`, gameID).Scan(&active, &staged); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM user_game_completion_projections WHERE user_id=$1 AND game_id=$2 AND weight=1.5`, userID, gameID).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || active != 1 || staged != 0 || projection != 1 {
		t.Fatalf("current=%+v active=%d staged=%d projection=%d", rows, active, staged, projection)
	}
}

func TestClassificationChunksProviderRequestsAndDoesNotPublishPartialGeneration(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	achievements := make([]steam.Achievement, 201)
	for index := range achievements {
		achievements[index] = steam.Achievement{APIName: fmt.Sprintf("achievement_%03d", index), Name: fmt.Sprintf("Mystery %03d", index), Description: "Ambiguous reward"}
	}
	change, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: achievements}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	service, _ := NewService(repository, semanticClassifierFunc(func(_ context.Context, input ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		calls++
		if len(input.Achievements) > 200 {
			t.Fatalf("provider chunk=%d", len(input.Achievements))
		}
		if calls == 2 {
			return ai.AchievementSemanticClassification{}, ai.ErrInvalidOutput
		}
		results := make([]ai.AchievementSemanticResult, len(input.Achievements))
		for index, item := range input.Achievements {
			results[index] = ai.AchievementSemanticResult{EvidenceKey: item.EvidenceKey, Class: ai.AchievementSemanticOther}
		}
		return ai.AchievementSemanticClassification{Results: results}, nil
	}), "semantic-model", jobs.NewQueue(db))
	if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: change.DefinitionSetHash}); !errors.Is(err, ai.ErrInvalidOutput) {
		t.Fatalf("error=%v want invalid output", err)
	}
	var generations, classifications int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM achievement_semantic_classification_generations WHERE game_id=$1`, gameID).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM achievement_semantic_classifications WHERE game_id=$1`, gameID).Scan(&classifications); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || generations != 0 || classifications != 0 {
		t.Fatalf("calls=%d generations=%d classifications=%d", calls, generations, classifications)
	}
	calls = 0
	service.classifier = semanticClassifierFunc(func(_ context.Context, input ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		calls++
		if calls == 2 {
			return ai.AchievementSemanticClassification{}, &ai.RequestError{StatusCode: 429, Temporary: true}
		}
		results := make([]ai.AchievementSemanticResult, len(input.Achievements))
		for index, item := range input.Achievements {
			results[index] = ai.AchievementSemanticResult{EvidenceKey: item.EvidenceKey, Class: ai.AchievementSemanticOther}
		}
		return ai.AchievementSemanticClassification{Results: results}, nil
	})
	if err := service.runClassification(ctx, JobPayload{Version: 1, GameID: gameID, DefinitionSetHash: change.DefinitionSetHash}); err != nil {
		t.Fatal(err)
	}
	rows, err := repository.LoadActiveClassifications(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(rows) != 201 || rows["achievement_000"].Source != ClassificationSourceOpenAI || rows["achievement_200"].Source != ClassificationSourceUnknown {
		t.Fatalf("calls=%d rows=%d first=%+v last=%+v", calls, len(rows), rows["achievement_000"], rows["achievement_200"])
	}
}

func TestProviderUnavailableClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "disabled", err: ai.ErrCapabilityDisabled, want: true},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "transport", err: fmt.Errorf("request: %w", ai.ErrHTTP), want: true},
		{name: "rate limited", err: &ai.RequestError{StatusCode: 429, Temporary: true}, want: true},
		{name: "server unavailable", err: &ai.RequestError{StatusCode: 503, Temporary: true}, want: true},
		{name: "provider rejected", err: &ai.RequestError{StatusCode: 400, Temporary: false}, want: false},
		{name: "invalid output", err: ai.ErrInvalidOutput, want: false},
		{name: "cancelled", err: context.Canceled, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := providerUnavailable(test.err); got != test.want {
				t.Fatalf("providerUnavailable(%v)=%v want=%v", test.err, got, test.want)
			}
		})
	}
}

func TestClassificationHookFailureIsRetriedAfterPublication(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	change, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "ending", Name: "Ending", Description: "Reach an ending"}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(db)
	service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, errors.New("provider must not be called")
	}), "semantic-model", queue)
	hookCalls := 0
	service.SetClassificationPublishedHook(func(context.Context, uuid.UUID) error {
		hookCalls++
		if hookCalls == 1 {
			return errors.New("temporary fanout failure")
		}
		return nil
	})
	spec, err := service.PlanClassificationJob(ctx, gameID, change.DefinitionSetHash, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, spec); err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(ctx, jobs.LaneInteractive, "completion-test-worker", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	hookErr := service.handleClassification(ctx, *first)
	if hookErr == nil {
		t.Fatal("first hook failure was swallowed")
	}
	if _, err := queue.Fail(ctx, *first, hookErr, time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := queue.Claim(ctx, jobs.LaneInteractive, "completion-test-worker", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim=%+v err=%v", second, err)
	}
	var pending classificationJobResult
	if err := json.Unmarshal(second.Result, &pending); err != nil || !pending.PublishedHookPending {
		t.Fatalf("persisted hook intent=%s err=%v", second.Result, err)
	}
	if err := service.handleClassification(ctx, *second); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, *second); err != nil {
		t.Fatal(err)
	}
	var generations, classifications int
	var result []byte
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM achievement_semantic_classification_generations WHERE game_id=$1`, gameID).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM achievement_semantic_classifications WHERE game_id=$1`, gameID).Scan(&classifications); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT result FROM sync_jobs WHERE id=$1`, second.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result, &pending); err != nil || pending.PublishedHookPending {
		t.Fatalf("cleared hook intent=%s err=%v", result, err)
	}
	if hookCalls != 2 || generations != 1 || classifications != 1 {
		t.Fatalf("hook_calls=%d generations=%d classifications=%d", hookCalls, generations, classifications)
	}
}

func TestClassificationActivationFansOutUserOwnedProjectionJobs(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	firstUser, gameID := seedCompletionGame(t, db, nil)
	secondUser := uuid.New()
	unrelatedUser := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1),($2)`, secondUser, unrelatedUser); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'steam',now())`, secondUser, gameID); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(db)
	summary := steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "ending", Name: "Ending", Description: "Reach an ending", Achieved: true}}}
	firstChange, err := repository.SaveSteamSnapshot(ctx, firstUser, gameID, summary, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SaveSteamSnapshot(ctx, secondUser, gameID, summary, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PublishClassificationGeneration(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, firstChange.DefinitionSetHash, []StagedClassification{{APIName: "ending", DefinitionContentHash: definitionHashFor(t, db, gameID, "ending"), Class: ai.AchievementSemanticBaseGameEnding, Source: ClassificationSourceLexical}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(db)
	service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, nil
	}), "semantic-model", queue)
	classification, err := service.PlanClassificationJob(ctx, gameID, firstChange.DefinitionSetHash, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, classification); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := service.EnqueueProjectionJobsForGame(ctx, gameID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Pool.Query(ctx, `SELECT kind,user_id,idempotency_key,payload::text FROM sync_jobs WHERE kind IN ('achievement.classify','completion.project') ORDER BY kind,user_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	projectionUsers := map[uuid.UUID]bool{}
	classificationUnowned := false
	for rows.Next() {
		var kind, key, payload string
		var owner *uuid.UUID
		if err := rows.Scan(&kind, &owner, &key, &payload); err != nil {
			t.Fatal(err)
		}
		if kind == ClassificationJobKind {
			classificationUnowned = owner == nil
			continue
		}
		if owner == nil || *owner == uuid.Nil || !strings.Contains(key, ProjectionJobKind+":v1:") || !strings.Contains(payload, gameID.String()) {
			t.Fatalf("invalid projection job owner=%v key=%q payload=%s", owner, key, payload)
		}
		projectionUsers[*owner] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !classificationUnowned || len(projectionUsers) != 2 || !projectionUsers[firstUser] || !projectionUsers[secondUser] || projectionUsers[unrelatedUser] {
		t.Fatalf("classification_unowned=%v projection_users=%v", classificationUnowned, projectionUsers)
	}
}

func TestProjectionJobRebuildsProfileThenRecommendationsOnce(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := seedCompletionGame(t, db, nil)
	repository, _ := NewSQLRepository(db)
	change, err := repository.SaveSteamSnapshot(ctx, userID, gameID, steam.AchievementSummary{Achievements: []steam.Achievement{{APIName: "ending", Name: "Ending", Description: "Reach an ending", Achieved: true}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PublishClassificationGeneration(ctx, gameID, "semantic-model", ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, change.DefinitionSetHash, []StagedClassification{{APIName: "ending", DefinitionContentHash: definitionHashFor(t, db, gameID, "ending"), Class: ai.AchievementSemanticBaseGameEnding, Source: ClassificationSourceLexical}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(db)
	service, _ := NewService(repository, semanticClassifierFunc(func(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) {
		return ai.AchievementSemanticClassification{}, nil
	}), "semantic-model", queue)
	spec, err := service.planProjectionJob(userID, gameID, change.DefinitionSetHash, 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, spec); err != nil {
		t.Fatal(err)
	}
	projectionLease := claimKind(t, queue, ProjectionJobKind)
	stale := *projectionLease
	stale.FencingToken = uuid.New()
	if err := service.handleProjection(ctx, stale); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale projection handling err=%v", err)
	}
	var rolledBack int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM user_game_completion_projections WHERE user_id=$1 AND game_id=$2`, userID, gameID).Scan(&rolledBack); err != nil || rolledBack != 0 {
		t.Fatalf("projection rollback count=%d err=%v", rolledBack, err)
	}
	if err := service.handleProjection(ctx, *projectionLease); !errors.Is(err, jobs.ErrDeferred) {
		t.Fatalf("first projection handling err=%v", err)
	}
	assertJobCounts(t, db, 1, 0)
	profileLease := claimKind(t, queue, "profile.rebuild")
	if err := queue.RecordResult(ctx, *profileLease, map[string]any{"profileId": uuid.New(), "profileVersion": 1, "profileBuildKey": "completion-fixture-profile"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, *profileLease); err != nil {
		t.Fatal(err)
	}
	projectionLease = claimKind(t, queue, ProjectionJobKind)
	if err := service.handleProjection(ctx, *projectionLease); !errors.Is(err, jobs.ErrDeferred) {
		t.Fatalf("second projection handling err=%v", err)
	}
	assertJobCounts(t, db, 1, 1)
	recommendationLease := claimKind(t, queue, "recommendations.rebuild")
	if err := queue.Complete(ctx, *recommendationLease); err != nil {
		t.Fatal(err)
	}
	projectionLease = claimKind(t, queue, ProjectionJobKind)
	if err := service.handleProjection(ctx, *projectionLease); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, *projectionLease); err != nil {
		t.Fatal(err)
	}
	assertJobCounts(t, db, 1, 1)

	unchanged, err := service.planProjectionJob(userID, gameID, change.DefinitionSetHash+"-recheck", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, unchanged); err != nil {
		t.Fatal(err)
	}
	unchangedLease := claimKind(t, queue, ProjectionJobKind)
	if err := service.handleProjection(ctx, *unchangedLease); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, *unchangedLease); err != nil {
		t.Fatal(err)
	}
	assertJobCounts(t, db, 1, 1)
}

func TestProjectionRebuildKeysIncludeTransitionIdentity(t *testing.T) {
	userID, gameID := uuid.New(), uuid.New()
	first := projectionRebuildKeyPrefix(userID, gameID, "generation", uuid.New(), "projection")
	second := projectionRebuildKeyPrefix(userID, gameID, "generation", uuid.New(), "projection")
	if first == second {
		t.Fatalf("distinct projection transitions reused child key %q", first)
	}
}

func claimKind(t *testing.T, queue *jobs.Queue, kind string) *jobs.Lease {
	t.Helper()
	lease, err := queue.Claim(context.Background(), jobs.LaneInteractive, "completion-projection-test", time.Minute)
	if err != nil || lease == nil || lease.Kind != kind {
		t.Fatalf("claim kind=%q lease=%+v err=%v", kind, lease, err)
	}
	return lease
}

func assertJobCounts(t *testing.T, db *store.Store, profiles, recommendations int) {
	t.Helper()
	var gotProfiles, gotRecommendations int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE kind='profile.rebuild'),count(*) FILTER (WHERE kind='recommendations.rebuild') FROM sync_jobs`).Scan(&gotProfiles, &gotRecommendations); err != nil {
		t.Fatal(err)
	}
	if gotProfiles != profiles || gotRecommendations != recommendations {
		t.Fatalf("profile jobs=%d recommendations=%d want=%d/%d", gotProfiles, gotRecommendations, profiles, recommendations)
	}
}

func seedCompletionGame(t *testing.T, db *store.Store, _ []steam.Achievement) (uuid.UUID, uuid.UUID) {
	t.Helper()
	userID, gameID := uuid.New(), uuid.New()
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,$2,'Classification Test','test',now())`, gameID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'steam',now())`, userID, gameID); err != nil {
		t.Fatal(err)
	}
	return userID, gameID
}

func definitionHashFor(t *testing.T, db *store.Store, gameID uuid.UUID, apiName string) string {
	t.Helper()
	var hash string
	if err := db.Pool.QueryRow(context.Background(), `SELECT definition_content_hash FROM game_achievement_definitions WHERE game_id=$1 AND api_name=$2`, gameID, apiName).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}
