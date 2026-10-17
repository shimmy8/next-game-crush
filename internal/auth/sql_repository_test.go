package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/store"
)

func TestSQLRepositoryPersistsHashedOneTimeAuthMaterial(t *testing.T) {
	database := openAuthTestStore(t)
	repository := NewSQLRepository(database)
	provider := &fakeProvider{valid: true}
	service, err := New(Config{AppBaseURL: "http://localhost:8080", AllowedSteamID: allowedSteamID, SessionSecret: strings.Repeat("s", 32), ProviderURL: defaultProviderURL}, repository, provider)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return now }
	callback := beginCallback(t, service)
	state := callback.Get("state")
	var rawStateCount int
	if err := database.Pool.QueryRow(context.Background(), `SELECT count(*) FROM openid_login_states WHERE token_hash=$1`, []byte(state)).Scan(&rawStateCount); err != nil {
		t.Fatal(err)
	}
	if rawStateCount != 0 {
		t.Fatal("raw login state was persisted")
	}
	session, err := service.Complete(context.Background(), callback)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), callback); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("replay error = %v", err)
	}
	var sessionCount int
	if err := database.Pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE id=$1`, service.sessionID(session.Token)).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 1 {
		t.Fatalf("stored sessions = %d", sessionCount)
	}
	now = now.Add(time.Second)
	secondCallback := beginCallback(t, service)
	rotated, err := service.Complete(service.WithSessionRotation(context.Background(), session.ID), secondCallback)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old session error = %v", err)
	}
	if _, err := service.Authenticate(context.Background(), rotated.Token); err != nil {
		t.Fatalf("rotated session: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 1 {
		t.Fatalf("sessions after rotation = %d, want 1", sessionCount)
	}
}

func openAuthTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "auth_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	database, err := store.Open(context.Background(), fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanup, err := pgx.Connect(context.Background(), baseURL)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return database
}
