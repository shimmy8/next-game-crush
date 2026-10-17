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
