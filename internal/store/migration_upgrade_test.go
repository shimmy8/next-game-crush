package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
	migrationfiles "next-game-crush/db/migrations"
)

func TestAuthMigrationUpgradesTask2Database(t *testing.T) {
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		cleanup, err := pgx.Connect(context.Background(), baseURL)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolatedURL := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	database, err := sql.Open("pgx", isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	_ = provider.Close()
	_ = database.Close()
	upgraded, err := Open(ctx, isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	for _, table := range []string{"openid_login_states", "openid_response_nonces"} {
		var exists bool
		if err := upgraded.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("upgrade did not create %s", table)
		}
	}
}

func TestDataLifecycleMigrationBackfillsChildAndProfileJobs(t *testing.T) {
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		cleanup, e := pgx.Connect(context.Background(), baseURL)
		if e == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolatedURL := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	database, err := sql.Open("pgx", isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.UpTo(ctx, 6); err != nil {
		t.Fatal(err)
	}
	userID, profileID, parentID, childID, summaryID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id)VALUES($1)`, []any{userID}},
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO sync_jobs(id,kind,idempotency_key,payload)VALUES($1,'steam.sync','parent',jsonb_build_object('user_id',$2::text))`, []any{parentID, userID}},
		{`INSERT INTO sync_jobs(id,kind,idempotency_key,payload,parent_job_id)VALUES($1,'ai.features','child','{"game_id":"00000000-0000-4000-8000-000000000001"}',$2)`, []any{childID, parentID}},
		{`INSERT INTO sync_jobs(id,kind,idempotency_key,payload)VALUES($1,'ai.profile_summary','summary',jsonb_build_object('profile_id',$2::text))`, []any{summaryID, profileID}},
	} {
		if _, err = database.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	for _, jobID := range []uuid.UUID{parentID, childID, summaryID} {
		var owner uuid.UUID
		if err = database.QueryRowContext(ctx, `SELECT user_id FROM sync_jobs WHERE id=$1`, jobID).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != userID {
			t.Fatalf("job %s owner=%s want %s", jobID, owner, userID)
		}
	}
	_ = provider.Close()
	_ = database.Close()
}

func TestSteamVisibilityMigration(t *testing.T) {
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		cleanup, cleanupErr := pgx.Connect(context.Background(), baseURL)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolatedURL := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	database, err := sql.Open("pgx", isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if _, err = provider.UpTo(ctx, 7); err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	if _, err = database.ExecContext(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO user_source_states(user_id,source,availability,last_attempt_at) VALUES($1,'owned_games','available',now())`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	var visibility *string
	if err = database.QueryRowContext(ctx, `SELECT visibility FROM user_source_states WHERE user_id=$1`, userID).Scan(&visibility); err != nil {
		t.Fatal(err)
	}
	if visibility != nil {
		t.Fatalf("upgraded visibility = %q, want NULL", *visibility)
	}
	for _, value := range []string{"public", "private", "unknown"} {
		if _, err = database.ExecContext(ctx, `UPDATE user_source_states SET visibility=$1 WHERE user_id=$2`, value, userID); err != nil {
			t.Fatalf("visibility %q: %v", value, err)
		}
	}
	if _, err = database.ExecContext(ctx, `UPDATE user_source_states SET visibility='secret' WHERE user_id=$1`, userID); err == nil {
		t.Fatal("invalid visibility unexpectedly accepted")
	}
}

func TestAchievementCompletionMigrationBackfillsHundredPercentAndConstraints(t *testing.T) {
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "achievement_completion_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		cleanup, cleanupErr := pgx.Connect(context.Background(), baseURL)
		if cleanupErr == nil {
			defer cleanup.Close(context.Background())
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	})
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	database, err := sql.Open("pgx", fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if _, err = provider.UpTo(ctx, 14); err != nil {
		t.Fatal(err)
	}

	userID, completedGameID, partialGameID := uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES
			($1,1501,'Completed','catalog_seed',now()),
			($2,1502,'Partial','catalog_seed',now())`, []any{completedGameID, partialGameID}},
		{`INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at) VALUES
			($1,$2,10,10,'steam',now()),
			($1,$3,9,10,'steam',now())`, []any{userID, completedGameID, partialGameID}},
	} {
		if _, err = database.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = provider.UpTo(ctx, 15); err != nil {
		t.Fatal(err)
	}

	var evidenceCount int
	if err = database.QueryRowContext(ctx, `SELECT count(*) FROM user_game_completion_evidence WHERE kind='hundred_percent'`).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if evidenceCount != 1 {
		t.Fatalf("hundred-percent evidence count=%d, want 1", evidenceCount)
	}
	var weight float64
	var endingCount int
	var basis string
	var completionAt *time.Time
	if err = database.QueryRowContext(ctx, `SELECT weight,ending_count,basis,completion_at FROM user_game_completion_projections`).Scan(&weight, &endingCount, &basis, &completionAt); err != nil {
		t.Fatal(err)
	}
	if weight != 1.5 || endingCount != 0 || basis != "hundred_percent" || completionAt != nil {
		t.Fatalf("projection weight=%v endings=%d basis=%q completed_at=%v", weight, endingCount, basis, completionAt)
	}

	definitionID := uuid.New()
	if _, err = database.ExecContext(ctx, `INSERT INTO game_achievement_definitions(
		id,game_id,api_name,display_name,description,metadata_source,definition_content_hash,last_successful_fetch_at
	) VALUES($1,$2,'ENDING_ONE','Ending','Reach an ending','player_achievements','definition-hash',now())`, definitionID, completedGameID); err != nil {
		t.Fatal(err)
	}
	generationID := uuid.New()
	if _, err = database.ExecContext(ctx, `INSERT INTO achievement_semantic_classification_generations(
		id,game_id,model,prompt_version,schema_version,definition_set_hash,lexical_rule_version,status
	) VALUES($1,$2,'test-model','prompt-v1','schema-v1','set-hash','lexical-v1','staged')`, generationID, completedGameID); err != nil {
		t.Fatal(err)
	}
	classificationID := uuid.New()
	if _, err = database.ExecContext(ctx, `INSERT INTO achievement_semantic_classifications(
		id,generation_id,game_id,api_name,definition_content_hash,class,model,prompt_version,schema_version,input_content_hash,classification_source,lexical_rule_version
	) VALUES($1,$2,$3,'ENDING_ONE','definition-hash','base_game_ending','test-model','prompt-v1','schema-v1','input-hash','lexical','lexical-v1')`, classificationID, generationID, completedGameID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `UPDATE achievement_semantic_classification_generations
		SET status='active',activated_at=now() WHERE id=$1`, generationID); err != nil {
		t.Fatal(err)
	}

	invalidStatements := []string{
		`UPDATE user_game_completion_projections SET weight=2.26 WHERE user_id='` + userID.String() + `' AND game_id='` + completedGameID.String() + `'`,
		`UPDATE user_game_completion_projections SET basis='invalid' WHERE user_id='` + userID.String() + `' AND game_id='` + completedGameID.String() + `'`,
		`UPDATE achievement_semantic_classifications SET class='invalid' WHERE id='` + classificationID.String() + `'`,
		`UPDATE achievement_semantic_classification_generations SET status='invalid' WHERE id='` + generationID.String() + `'`,
		`UPDATE achievement_semantic_classifications SET classification_source='invalid' WHERE id='` + classificationID.String() + `'`,
		`UPDATE achievement_semantic_classification_generations SET lexical_rule_version='   ' WHERE id='` + generationID.String() + `'`,
		`UPDATE achievement_semantic_classifications SET lexical_rule_version='' WHERE id='` + classificationID.String() + `'`,
		`INSERT INTO user_game_completion_evidence(id,user_id,game_id,kind,observed_at,algorithm_version) VALUES(gen_random_uuid(),'` + userID.String() + `','` + completedGameID.String() + `','ending_marker',now(),'completion-v1')`,
		`INSERT INTO game_achievement_definitions(game_id,api_name,metadata_source,definition_content_hash,last_successful_fetch_at) VALUES('` + completedGameID.String() + `','BAD_SOURCE','invalid','hash',now())`,
		`INSERT INTO achievement_semantic_classification_generations(id,game_id,model,prompt_version,schema_version,definition_set_hash,lexical_rule_version,status,activated_at) VALUES(gen_random_uuid(),'` + completedGameID.String() + `','test-model','prompt-v1','schema-v1','other-set','lexical-v1','active',now())`,
	}
	for _, statement := range invalidStatements {
		if _, err = database.ExecContext(ctx, statement); err == nil {
			t.Fatalf("invalid statement unexpectedly accepted: %s", statement)
		}
	}
	if _, err = database.ExecContext(ctx, `DELETE FROM games WHERE id=$1`, completedGameID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"game_achievement_definitions",
		"achievement_semantic_classification_generations",
		"achievement_semantic_classifications",
		"user_game_completion_evidence",
		"user_game_completion_projections",
	} {
		var count int
		if err = database.QueryRowContext(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()+" WHERE game_id=$1", completedGameID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("game cascade left %s rows=%d err=%v", table, count, err)
		}
	}
}
