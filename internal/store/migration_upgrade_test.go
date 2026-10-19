package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pressly/goose/v3"
	migrationfiles "next-game-crush/db/migrations"
	dbgen "next-game-crush/internal/store/sqlc"
)

func TestSteamSearchMigrationAllowsEmptyRollback(t *testing.T) {
	ctx, database, provider := searchMigrationDatabase(t)
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("empty additive schema rollback: %v", err)
	}
	var removed bool
	if err := database.QueryRowContext(ctx, `SELECT to_regclass('catalog_search_runs') IS NULL AND
		NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='sync_jobs' AND column_name='lane')`).Scan(&removed); err != nil || !removed {
		t.Fatalf("empty rollback left search schema: removed=%v err=%v", removed, err)
	}
}

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

func TestPreferenceAdjustmentsMigration(t *testing.T) {
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
		cleanup, err := pgx.Connect(context.Background(), baseURL)
		if err == nil {
			defer cleanup.Close(context.Background())
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
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
	if _, err = provider.UpTo(ctx, 9); err != nil {
		t.Fatal(err)
	}
	user, game, profile, recommendation, feedback := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id)VALUES($1)`, []any{user}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,123,'Existing','catalog_seed',now())`, []any{game}},
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, []any{profile, user}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score)VALUES($1,$2,$3,$4,'old',15)`, []any{recommendation, user, game, profile}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'dislike','existing')`, []any{feedback, recommendation, user}},
		{`INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash)VALUES($1,'multiplayer','negative',.9,true,'old','old-hash')`, []any{feedback}},
	} {
		if _, err = database.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = provider.UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var target *string
	var hash string
	if err = database.QueryRowContext(ctx, `SELECT preference_target,input_content_hash FROM feedback_classifications WHERE feedback_id=$1`, feedback).Scan(&target, &hash); err != nil || target != nil || hash != "old-hash" {
		t.Fatalf("classification target=%v hash=%q err=%v", target, hash, err)
	}
	var score float64
	if err = database.QueryRowContext(ctx, `SELECT score FROM recommendations WHERE id=$1`, recommendation).Scan(&score); err != nil || score != 15 {
		t.Fatalf("ranking score=%v err=%v", score, err)
	}
	for _, table := range []string{"preference_adjustments", "game_modes"} {
		var count int
		if err = database.QueryRowContext(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count != 0 {
			t.Fatalf("table %s count=%d err=%v", table, count, err)
		}
	}
	if _, err = provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = database.QueryRowContext(ctx, `SELECT to_regclass('game_modes') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("down left game_modes: %v err=%v", exists, err)
	}
	if err = database.QueryRowContext(ctx, `SELECT score FROM recommendations WHERE id=$1`, recommendation).Scan(&score); err != nil || score != 15 {
		t.Fatalf("down changed old ranking: %v err=%v", score, err)
	}
}

func TestGroundedExplanationMigrationBackfillsLegacySources(t *testing.T) {
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
			defer cleanup.Close(context.Background())
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
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
	if _, err = provider.UpTo(ctx, 8); err != nil {
		t.Fatal(err)
	}
	userID, profileID := uuid.New(), uuid.New()
	generatedGameID, deterministicGameID := uuid.New(), uuid.New()
	generatedID, deterministicID := uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,101,'Generated','catalog_seed',now()),($2,102,'Deterministic','catalog_seed',now())`, []any{generatedGameID, deterministicGameID}},
		{`INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score,explanation_generated) VALUES($1,$2,$3,$4,'legacy',2,true),($5,$2,$6,$4,'legacy',1,false)`, []any{generatedID, userID, generatedGameID, profileID, deterministicID, deterministicGameID}},
	} {
		if _, err = database.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id     uuid.UUID
		source string
	}{
		{id: generatedID, source: "ai"},
		{id: deterministicID, source: "deterministic"},
	} {
		var source string
		var reason *string
		if err = database.QueryRowContext(ctx, `SELECT explanation_source,explanation_failure_reason FROM recommendations WHERE id=$1`, test.id).Scan(&source, &reason); err != nil {
			t.Fatal(err)
		}
		if source != test.source || reason != nil {
			t.Fatalf("recommendation %s source=%q reason=%v, want source=%q reason=NULL", test.id, source, reason, test.source)
		}
	}
}

func TestGameCompatibilityMigration(t *testing.T) {
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
	schema := "compatibility_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if _, err = provider.UpTo(ctx, 11); err != nil {
		t.Fatal(err)
	}
	gameID, cascadeGameID := uuid.New(), uuid.New()
	if _, err = database.ExecContext(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,1201,'Existing','catalog_seed',now()),($2,1202,'Cascade','catalog_seed',now())`, gameID, cascadeGameID); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.UpTo(ctx, 12); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = database.QueryRowContext(ctx, `SELECT count(*) FROM game_compatibility`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("compatibility rows after upgrade = %d, err=%v; want no invented observations", count, err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO game_compatibility(game_id,windows,mac,linux,platforms_last_successful_fetch_at) VALUES($1,true,false,true,now()),($2,false,true,false,now())`, gameID, cascadeGameID); err != nil {
		t.Fatal(err)
	}
	var status, availability string
	var attemptedAt, succeededAt *time.Time
	if err = database.QueryRowContext(ctx, `SELECT deck_status,deck_availability,deck_last_attempt_at,deck_last_successful_fetch_at FROM game_compatibility WHERE game_id=$1`, gameID).Scan(&status, &availability, &attemptedAt, &succeededAt); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" || availability != "unknown" || attemptedAt != nil || succeededAt != nil {
		t.Fatalf("unexpected Deck defaults: status=%q availability=%q attempt=%v success=%v", status, availability, attemptedAt, succeededAt)
	}
	for _, value := range []string{"verified", "playable", "unsupported", "unknown"} {
		if _, err = database.ExecContext(ctx, `UPDATE game_compatibility SET deck_status=$1 WHERE game_id=$2`, value, gameID); err != nil {
			t.Fatalf("Deck status %q: %v", value, err)
		}
	}
	if _, err = database.ExecContext(ctx, `UPDATE game_compatibility SET deck_status='invalid' WHERE game_id=$1`, gameID); err == nil {
		t.Fatal("invalid Deck status unexpectedly accepted")
	}
	if _, err = database.ExecContext(ctx, `DELETE FROM games WHERE id=$1`, cascadeGameID); err != nil {
		t.Fatal(err)
	}
	if err = database.QueryRowContext(ctx, `SELECT count(*) FROM game_compatibility WHERE game_id=$1`, cascadeGameID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("compatibility cascade count=%d err=%v", count, err)
	}
	if _, err = provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = database.QueryRowContext(ctx, `SELECT to_regclass('game_compatibility') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("down left game_compatibility: %v err=%v", exists, err)
	}
	var name string
	if err = database.QueryRowContext(ctx, `SELECT name FROM games WHERE id=$1`, gameID).Scan(&name); err != nil || name != "Existing" {
		t.Fatalf("down changed existing game: name=%q err=%v", name, err)
	}
	for _, table := range []string{"catalog_discovery_state", "preference_adjustments", "game_modes"} {
		if err = database.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("down changed earlier table %s: exists=%v err=%v", table, exists, err)
		}
	}
	if err = database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='recommendations' AND column_name='explanation_source')`).Scan(&exists); err != nil || !exists {
		t.Fatalf("down changed earlier explanation schema: exists=%v err=%v", exists, err)
	}
}

func TestFeedbackMultipleReasonsMigrationBackfillsLegacyReason(t *testing.T) {
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
	schema := "feedback_reasons_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if _, err = provider.UpTo(ctx, 12); err != nil {
		t.Fatal(err)
	}
	userID, gameID, profileID, recommendationID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	eventID, trimmedEventID, unknownEventID, whitespaceEventID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,1301,'Legacy','catalog_seed',now())`, []any{gameID}},
		{`INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score) VALUES($1,$2,$3,$4,'legacy',1)`, []any{recommendationID, userID, gameID, profileID}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,reason,idempotency_key) VALUES
			($1,$5,$6,'like','setting','legacy-reason'),
			($2,$5,$6,'like','  genre  ','legacy-trimmed'),
			($3,$5,$6,'like','old-client-value','legacy-unknown'),
			($4,$5,$6,'like','   ','legacy-whitespace')`, []any{eventID, trimmedEventID, unknownEventID, whitespaceEventID, recommendationID, userID}},
	} {
		if _, err = database.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = provider.UpTo(ctx, 13); err != nil {
		t.Fatal(err)
	}
	var reasons string
	if err = database.QueryRowContext(ctx, `SELECT array_to_string(reasons, ',') FROM feedback_events WHERE id=$1`, eventID).Scan(&reasons); err != nil {
		t.Fatal(err)
	}
	if reasons != "setting" {
		t.Fatalf("backfilled reasons=%q", reasons)
	}
	for _, test := range []struct {
		id   uuid.UUID
		want string
	}{
		{id: trimmedEventID, want: "genre"},
		{id: unknownEventID, want: ""},
		{id: whitespaceEventID, want: ""},
	} {
		if err = database.QueryRowContext(ctx, `SELECT array_to_string(reasons, ',') FROM feedback_events WHERE id=$1`, test.id).Scan(&reasons); err != nil {
			t.Fatal(err)
		}
		if reasons != test.want {
			t.Fatalf("event %s backfilled reasons=%q, want %q", test.id, reasons, test.want)
		}
	}
	if _, err = database.ExecContext(ctx, `UPDATE feedback_events SET reasons=ARRAY['unknown'] WHERE id=$1`, eventID); err == nil {
		t.Fatal("unknown reason unexpectedly accepted")
	}
	if _, err = database.ExecContext(ctx, `UPDATE feedback_events SET reasons=ARRAY['genre',NULL]::text[] WHERE id=$1`, eventID); err == nil {
		t.Fatal("NULL reason unexpectedly accepted")
	}
	if _, err = database.ExecContext(ctx, `UPDATE feedback_events SET reasons=ARRAY['genre','genre'] WHERE id=$1`, eventID); err == nil {
		t.Fatal("duplicate reasons unexpectedly accepted")
	}
	if _, err = database.ExecContext(ctx, `UPDATE feedback_events SET reasons=ARRAY['setting','genre'] WHERE id=$1`, eventID); err == nil {
		t.Fatal("noncanonical reason order unexpectedly accepted")
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

func TestActivityProfilePreferenceOverlayMigration(t *testing.T) {
	ctx, database, provider, _ := migrationDatabaseAt(t, 19)

	userID, blankUserID, whitespaceUserID := uuid.New(), uuid.New(), uuid.New()
	profileID, blankProfileID, whitespaceProfileID := uuid.New(), uuid.New(), uuid.New()
	gameID, recommendationID, runID := uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Date(2026, time.October, 18, 9, 30, 0, 0, time.UTC)
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2),($3)`, []any{userID, blankUserID, whitespaceUserID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,20001,'Shared','test',now())`, []any{gameID}},
		{`INSERT INTO preference_profiles(id,user_id,version,summary,created_at,build_key) VALUES
			($1,$2,1,'Legacy summary',$5,'legacy-ready'),
			($3,$4,1,'   ',$5,'legacy-blank')`, []any{profileID, userID, blankProfileID, blankUserID, createdAt}},
		{`INSERT INTO preference_profiles(id,user_id,version,summary,created_at,build_key) VALUES($1,$2,1,E'\t\n\r',$3,'legacy-whitespace')`, []any{whitespaceProfileID, whitespaceUserID, createdAt}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,'legacy','legacy-run',true)`, []any{runID, userID, profileID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score,ranking_version) VALUES($1,$2,$3,$4,'legacy',50,1)`, []any{recommendationID, userID, gameID, profileID}},
	} {
		if _, err := database.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := provider.UpTo(ctx, 20); err != nil {
		t.Fatal(err)
	}

	var state, source string
	var publishedAt time.Time
	if err := database.QueryRowContext(ctx, `SELECT publication_state,summary_source,published_at FROM preference_profiles WHERE id=$1`, profileID).Scan(&state, &source, &publishedAt); err != nil {
		t.Fatal(err)
	}
	if state != "published" || source != "legacy" || !publishedAt.Equal(createdAt) {
		t.Fatalf("legacy ready profile state=%q source=%q published_at=%v", state, source, publishedAt)
	}
	var blankPublishedAt, blankSource any
	if err := database.QueryRowContext(ctx, `SELECT publication_state,published_at,summary_source FROM preference_profiles WHERE id=$1`, blankProfileID).Scan(&state, &blankPublishedAt, &blankSource); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || blankPublishedAt != nil || blankSource != nil {
		t.Fatalf("legacy blank profile state=%q published_at=%v source=%v", state, blankPublishedAt, blankSource)
	}
	if err := database.QueryRowContext(ctx, `SELECT publication_state,published_at,summary_source FROM preference_profiles WHERE id=$1`, whitespaceProfileID).Scan(&state, &blankPublishedAt, &blankSource); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || blankPublishedAt != nil || blankSource != nil {
		t.Fatalf("legacy control-whitespace profile state=%q published_at=%v source=%v", state, blankPublishedAt, blankSource)
	}
	for _, table := range []string{"recommendation_runs", "recommendations"} {
		var nullable bool
		if err := database.QueryRowContext(ctx, `SELECT is_nullable='YES' FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name='preference_overlay_id'`, table).Scan(&nullable); err != nil || !nullable {
			t.Fatalf("%s.preference_overlay_id nullable=%v err=%v", table, nullable, err)
		}
	}

	overlayID := uuid.New()
	if _, err := database.ExecContext(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash) VALUES($1,$2,1,'overlay-v1','build-one','hash-one')`, overlayID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash) VALUES(gen_random_uuid(),$1,1,'overlay-v1','build-two','hash-two')`, userID); err == nil {
		t.Fatal("duplicate overlay version unexpectedly accepted")
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash) VALUES(gen_random_uuid(),$1,2,'overlay-v1','build-one','hash-two')`, userID); err == nil {
		t.Fatal("duplicate overlay build key unexpectedly accepted")
	}

	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	var removed bool
	if err := database.QueryRowContext(ctx, `SELECT to_regclass('preference_overlays') IS NULL
		AND to_regclass('activity_profile_evidence') IS NULL
		AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='preference_profiles' AND column_name='publication_state')
		AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='recommendations' AND column_name='preference_overlay_id')`).Scan(&removed); err != nil || !removed {
		t.Fatalf("migration rollback incomplete: removed=%v err=%v", removed, err)
	}
	var legacyRows int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM recommendation_runs WHERE id=$1`, runID).Scan(&legacyRows); err != nil || legacyRows != 1 {
		t.Fatalf("rollback changed legacy rows: count=%d err=%v", legacyRows, err)
	}
}

func TestActivityProfilePublicationChecks(t *testing.T) {
	ctx, database, _, _ := migrationDatabaseAt(t, 20)
	userID := uuid.New()
	if _, err := database.ExecContext(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	valid := []string{
		`INSERT INTO preference_profiles(id,user_id,version,publication_state) VALUES(gen_random_uuid(),$1,1,'building')`,
		`INSERT INTO preference_profiles(id,user_id,version,publication_state) VALUES(gen_random_uuid(),$1,2,'failed')`,
		`INSERT INTO preference_profiles(id,user_id,version,publication_state) VALUES(gen_random_uuid(),$1,3,'insufficient')`,
		`INSERT INTO preference_profiles(id,user_id,version,summary,publication_state,published_at,summary_source) VALUES(gen_random_uuid(),$1,4,'ready','published',now(),'deterministic')`,
	}
	for _, statement := range valid {
		if _, err := database.ExecContext(ctx, statement, userID); err != nil {
			t.Fatalf("valid publication row rejected: %v", err)
		}
	}
	invalid := []string{
		`INSERT INTO preference_profiles(id,user_id,version,summary,publication_state) VALUES(gen_random_uuid(),$1,5,'ready','published')`,
		`INSERT INTO preference_profiles(id,user_id,version,summary,publication_state,published_at,summary_source) VALUES(gen_random_uuid(),$1,6,'   ','published',now(),'openai')`,
		`INSERT INTO preference_profiles(id,user_id,version,summary,publication_state,published_at,summary_source) VALUES(gen_random_uuid(),$1,9,E'\t\n\r','published',now(),'openai')`,
		`INSERT INTO preference_profiles(id,user_id,version,publication_state,published_at) VALUES(gen_random_uuid(),$1,7,'failed',now())`,
		`INSERT INTO preference_profiles(id,user_id,version,publication_state,summary_source) VALUES(gen_random_uuid(),$1,8,'building','legacy')`,
	}
	for _, statement := range invalid {
		if _, err := database.ExecContext(ctx, statement, userID); err == nil {
			t.Fatalf("invalid publication row unexpectedly accepted: %s", statement)
		}
	}
}

func TestPreferenceOverlayDeletionGuards(t *testing.T) {
	ctx, database, _, _ := migrationDatabaseAt(t, 20)
	userID, gameID, profileID, overlayID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,20002,'Shared','test',now())`, []any{gameID}},
		{`INSERT INTO game_embeddings(game_id,model,input_content_hash,embedding) VALUES($1,'shared','shared-hash',$2)`, []any{gameID, migrationZeroVector()}},
		{`INSERT INTO preference_profiles(id,user_id,version,publication_state) VALUES($1,$2,1,'building')`, []any{profileID, userID}},
		{`INSERT INTO activity_profile_evidence(profile_id,game_id,playtime_minutes,recently_played_minutes,achieved,total,completion_weight,completion_basis,completion_algorithm_version) VALUES($1,$2,60,5,1,10,0.5,'ending','completion-v1')`, []any{profileID, gameID}},
		{`INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash) VALUES($1,$2,1,'overlay-v1','build','hash')`, []any{overlayID, userID}},
		{`INSERT INTO preference_overlay_embeddings(overlay_id,polarity,model,input_content_hash,embedding) VALUES($1,'positive','test','hash',$2)`, []any{overlayID, migrationZeroVector()}},
		{`INSERT INTO deleted_user_tombstones(user_id) VALUES($1)`, []any{userID}},
	} {
		if _, err := database.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash) VALUES(gen_random_uuid(),$1,2,'overlay-v1','late','late')`, userID); err == nil {
		t.Fatal("late overlay insert after tombstone unexpectedly accepted")
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO preference_overlay_embeddings(overlay_id,polarity,model,input_content_hash,embedding) VALUES($1,'negative','test','late',$2)`, overlayID, migrationZeroVector()); err == nil {
		t.Fatal("late overlay embedding insert after tombstone unexpectedly accepted")
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"activity_profile_evidence", "preference_overlays", "preference_overlay_embeddings"} {
		var count int
		if err := database.QueryRowContext(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cascade left %s rows=%d err=%v", table, count, err)
		}
	}
	var shared int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM game_embeddings WHERE game_id=$1`, gameID).Scan(&shared); err != nil || shared != 1 {
		t.Fatalf("shared embedding count=%d err=%v", shared, err)
	}
}

func TestActivityProfilePublicationQueriesFenceDeletedUser(t *testing.T) {
	ctx, database, _, isolatedURL := migrationDatabaseAt(t, 20)
	userID, profileID := uuid.New(), uuid.New()
	if _, err := database.ExecContext(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO preference_profiles(id,user_id,version,publication_state) VALUES($1,$2,1,'building')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO deleted_user_tombstones(user_id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	storage, err := Open(ctx, isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	pgUserID := pgtype.UUID{Bytes: userID, Valid: true}
	pgProfileID := pgtype.UUID{Bytes: profileID, Valid: true}
	_, err = storage.Queries.PublishActivityProfile(ctx, dbgen.PublishActivityProfileParams{
		Summary:       pgtype.Text{String: "late summary", Valid: true},
		SummarySource: pgtype.Text{String: "deterministic", Valid: true},
		PublishedAt:   pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		ID:            pgProfileID,
		UserID:        pgUserID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late publication error=%v, want pgx.ErrNoRows", err)
	}
	rows, err := storage.Queries.FailBuildingActivityProfile(ctx, dbgen.FailBuildingActivityProfileParams{ID: pgProfileID, UserID: pgUserID})
	if err != nil || rows != 0 {
		t.Fatalf("late failure rows=%d err=%v, want zero rows", rows, err)
	}
	var state string
	if err := database.QueryRowContext(ctx, `SELECT publication_state FROM preference_profiles WHERE id=$1`, profileID).Scan(&state); err != nil || state != "building" {
		t.Fatalf("tombstoned profile state=%q err=%v, want building", state, err)
	}
}

func migrationDatabaseAt(t *testing.T, version int64) (context.Context, *sql.DB, *goose.Provider, string) {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "overlay_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	t.Cleanup(func() { _ = database.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	if _, err = provider.UpTo(ctx, version); err != nil {
		t.Fatal(err)
	}
	return ctx, database, provider, isolatedURL
}

func migrationZeroVector() string {
	return "[" + strings.TrimSuffix(strings.Repeat("0,", 1536), ",") + "]"
}
