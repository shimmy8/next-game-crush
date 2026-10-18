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

func TestDemoSessionMigrationPreservesOwnerSessions(t *testing.T) {
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
	schema := "demo_session_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	if _, err = provider.UpTo(ctx, 13); err != nil {
		t.Fatal(err)
	}
	userID, sessionID := uuid.New(), uuid.New()
	if _, err = database.ExecContext(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO sessions(id,user_id,csrf_token,expires_at) VALUES($1,$2,'csrf',now()+interval '1 hour')`, sessionID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	var mode string
	if err = database.QueryRowContext(ctx, `SELECT access_mode FROM sessions WHERE id=$1`, sessionID).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "owner" {
		t.Fatalf("existing session access_mode = %q, want owner", mode)
	}
	if _, err = database.ExecContext(ctx, `UPDATE sessions SET access_mode='demo' WHERE id=$1`, sessionID); err != nil {
		t.Fatalf("demo access mode rejected: %v", err)
	}
	if _, err = database.ExecContext(ctx, `UPDATE sessions SET access_mode='writer' WHERE id=$1`, sessionID); err == nil {
		t.Fatal("invalid access mode unexpectedly accepted")
	}
}
