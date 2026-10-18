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
