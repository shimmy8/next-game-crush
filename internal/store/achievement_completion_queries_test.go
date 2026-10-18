package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	dbgen "next-game-crush/internal/store/sqlc"
)

func TestCompletionProjectionInputUsesConfiguredClassificationIdentity(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	userID, gameID := uuid.New(), uuid.New()
	oldGenerationID, newGenerationID := uuid.New(), uuid.New()
	oldClassificationID, newClassificationID := uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,1510,'Identity','test',now())`, []any{gameID}},
		{`INSERT INTO game_achievement_definitions(game_id,api_name,display_name,metadata_source,definition_content_hash,last_successful_fetch_at) VALUES($1,'ENDING','Ending','player_achievements','hash-a',now())`, []any{gameID}},
		{`INSERT INTO achievement_semantic_classification_generations(id,game_id,model,prompt_version,schema_version,definition_set_hash,lexical_rule_version,status,activated_at) VALUES
			($1,$3,'old-model','prompt-v1','schema-v1','set-a','lexical-v1','active',now()),
			($2,$3,'new-model','prompt-v2','schema-v2','set-a','lexical-v2','active',now())`, []any{oldGenerationID, newGenerationID, gameID}},
		{`INSERT INTO achievement_semantic_classifications(id,generation_id,game_id,api_name,definition_content_hash,class,model,prompt_version,schema_version,input_content_hash,classification_source,lexical_rule_version) VALUES
			($1,$2,$4,'ENDING','hash-a','base_game_ending','old-model','prompt-v1','schema-v1','old-input','openai','lexical-v1'),
			($3,$5,$4,'ENDING','hash-a','other','new-model','prompt-v2','schema-v2','new-input','openai','lexical-v2')`, []any{oldClassificationID, oldGenerationID, newClassificationID, gameID, newGenerationID}},
		{`INSERT INTO user_game_completion_evidence(id,user_id,game_id,kind,api_name,definition_content_hash,semantic_classification_id,observed_at,algorithm_version) VALUES(gen_random_uuid(),$1,$2,'ending_marker','ENDING','hash-a',$3,now(),'completion-v1')`, []any{userID, gameID, oldClassificationID}},
	} {
		if _, err := db.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	oldRows, err := db.Queries.GetCompletionProjectionInput(ctx, dbgen.GetCompletionProjectionInputParams{
		UserID: uuidToPG(userID), GameID: uuidToPG(gameID), AlgorithmVersion: "completion-v1",
		Model: "old-model", PromptVersion: "prompt-v1", SchemaVersion: "schema-v1", LexicalRuleVersion: "lexical-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRows) != 1 || oldRows[0].Class.String != "base_game_ending" {
		t.Fatalf("old configured rows=%#v, want the old ending", oldRows)
	}
	newRows, err := db.Queries.GetCompletionProjectionInput(ctx, dbgen.GetCompletionProjectionInputParams{
		UserID: uuidToPG(userID), GameID: uuidToPG(gameID), AlgorithmVersion: "completion-v1",
		Model: "new-model", PromptVersion: "prompt-v2", SchemaVersion: "schema-v2", LexicalRuleVersion: "lexical-v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(newRows) != 0 {
		t.Fatalf("corrected configured identity accepted stale ending rows: %#v", newRows)
	}
}

func TestAchievementCompletionMigrationRejectsMismatchedClassificationIdentity(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	userID, gameID, generationID, classificationID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,1511,'Identity FK','test',now())`, []any{gameID}},
		{`INSERT INTO game_achievement_definitions(game_id,api_name,metadata_source,definition_content_hash,last_successful_fetch_at) VALUES($1,'ENDING','player_achievements','hash-a',now())`, []any{gameID}},
		{`INSERT INTO achievement_semantic_classification_generations(id,game_id,model,prompt_version,schema_version,definition_set_hash,lexical_rule_version,status) VALUES($1,$2,'model','prompt','schema','set','lexical','staged')`, []any{generationID, gameID}},
		{`INSERT INTO achievement_semantic_classifications(id,generation_id,game_id,api_name,definition_content_hash,class,model,prompt_version,schema_version,input_content_hash,classification_source,lexical_rule_version) VALUES($1,$2,$3,'ENDING','hash-a','base_game_ending','model','prompt','schema','input','lexical','lexical')`, []any{classificationID, generationID, gameID}},
	} {
		if _, err := db.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	for _, mismatch := range []struct {
		name, apiName, hash string
	}{
		{name: "api name", apiName: "OTHER", hash: "hash-a"},
		{name: "definition hash", apiName: "ENDING", hash: "hash-b"},
	} {
		t.Run(mismatch.name, func(t *testing.T) {
			_, err := db.Pool.Exec(ctx, `INSERT INTO user_game_completion_evidence(
				id,user_id,game_id,kind,api_name,definition_content_hash,
				semantic_classification_id,observed_at,algorithm_version
			) VALUES(gen_random_uuid(),$1,$2,'ending_marker',$3,$4,$5,now(),'completion-v1')`,
				userID, gameID, mismatch.apiName, mismatch.hash, classificationID)
			if err == nil {
				t.Fatalf("mismatched %s unexpectedly accepted", mismatch.name)
			}
		})
	}
}

func TestClassificationGenerationCanReactivateAAfterB(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	gameID, generationAID, generationBID := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,1512,'A B A','test',now())`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_achievement_definitions(game_id,api_name,metadata_source,definition_content_hash,last_successful_fetch_at) VALUES($1,'ENDING','player_achievements','hash-a',now())`, gameID); err != nil {
		t.Fatal(err)
	}

	create := func(id uuid.UUID, setHash string) dbgen.AchievementSemanticClassificationGeneration {
		t.Helper()
		generation, err := db.Queries.CreateClassificationGeneration(ctx, dbgen.CreateClassificationGenerationParams{
			ID: uuidToPG(id), GameID: uuidToPG(gameID), Model: "model", PromptVersion: "prompt",
			SchemaVersion: "schema", DefinitionSetHash: setHash, LexicalRuleVersion: "lexical",
		})
		if err != nil {
			t.Fatal(err)
		}
		return generation
	}
	stage := func(id, generationID uuid.UUID, hash string) {
		t.Helper()
		_, err := db.Queries.StageAchievementClassification(ctx, dbgen.StageAchievementClassificationParams{
			ID: uuidToPG(id), GenerationID: uuidToPG(generationID), GameID: uuidToPG(gameID),
			ApiName: "ENDING", DefinitionContentHash: hash, Class: "base_game_ending",
			Model: "model", PromptVersion: "prompt", SchemaVersion: "schema", InputContentHash: "input-" + hash,
			ClassificationSource: "lexical", LexicalRuleVersion: "lexical",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	create(generationAID, "set-a")
	stage(uuid.New(), generationAID, "hash-a")
	if _, err := db.Queries.ActivateClassificationGeneration(ctx, uuidToPG(generationAID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE game_achievement_definitions SET definition_content_hash='hash-b' WHERE game_id=$1`, gameID); err != nil {
		t.Fatal(err)
	}
	create(generationBID, "set-b")
	stage(uuid.New(), generationBID, "hash-b")
	if _, err := db.Queries.ActivateClassificationGeneration(ctx, uuidToPG(generationBID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE game_achievement_definitions SET definition_content_hash='hash-a' WHERE game_id=$1`, gameID); err != nil {
		t.Fatal(err)
	}
	reusedA := create(uuid.New(), "set-a")
	if reusedA.ID != uuidToPG(generationAID) || reusedA.Status != "superseded" {
		t.Fatalf("reused A=%#v, want original superseded generation", reusedA)
	}
	if _, err := db.Queries.ActivateClassificationGeneration(ctx, reusedA.ID); err != nil {
		t.Fatal(err)
	}

	var activeID uuid.UUID
	var activeCount int
	if err := db.Pool.QueryRow(ctx, `SELECT id,(SELECT count(*) FROM achievement_semantic_classification_generations WHERE game_id=$1 AND status='active')
		FROM achievement_semantic_classification_generations WHERE game_id=$1 AND status='active'`, gameID).Scan(&activeID, &activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 || activeID != generationAID {
		t.Fatalf("active generation=%s count=%d, want A=%s count=1", activeID, activeCount, generationAID)
	}
	var bStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM achievement_semantic_classification_generations WHERE id=$1`, generationBID).Scan(&bStatus); err != nil {
		t.Fatal(err)
	}
	if bStatus != "superseded" {
		t.Fatalf("generation B status=%q, want superseded", bStatus)
	}
}
