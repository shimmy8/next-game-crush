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

func TestSQLRepositoryBoundsActiveDemoSessionsWithoutInvalidatingVisitors(t *testing.T) {
	database := openAuthTestStore(t)
	repository := NewSQLRepository(database)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO steam_accounts(user_id,steam_id) VALUES($1,$2)`, userID, allowedSteamID); err != nil {
		t.Fatal(err)
	}
	ownerID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO sessions(id,user_id,csrf_token,expires_at,access_mode) VALUES($1,$2,'owner',now()+interval '1 hour','owner')`, ownerID, userID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxActiveDemoSessionsPerOwner; i++ {
		if _, err := database.Pool.Exec(ctx, `INSERT INTO sessions(id,user_id,csrf_token,expires_at,access_mode) VALUES($1,$2,$3,now()+interval '1 hour','demo')`, uuid.New(), userID, fmt.Sprintf("demo-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	candidate := Session{ID: uuid.New(), CSRFToken: "candidate", ExpiresAt: time.Now().Add(time.Hour), AccessMode: AccessModeDemo}
	if _, err := repository.CreateDemoSession(ctx, allowedSteamID, candidate); !errors.Is(err, ErrDemoUnavailable) {
		t.Fatalf("at cap error = %v, want ErrDemoUnavailable", err)
	}
	var active, owners int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE access_mode='demo' AND expires_at>now()), count(*) FILTER (WHERE access_mode='owner') FROM sessions WHERE user_id=$1`, userID).Scan(&active, &owners); err != nil {
		t.Fatal(err)
	}
	if active != maxActiveDemoSessionsPerOwner || owners != 1 {
		t.Fatalf("after rejection active demo=%d owners=%d", active, owners)
	}

	if _, err := database.Pool.Exec(ctx, `UPDATE sessions SET expires_at=now()-interval '1 second' WHERE id=(SELECT id FROM sessions WHERE user_id=$1 AND access_mode='demo' LIMIT 1)`, userID); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateDemoSession(ctx, allowedSteamID, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != candidate.ID || created.UserID != userID {
		t.Fatalf("created session = %#v", created)
	}
	var expired int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE access_mode='demo' AND expires_at>now()), count(*) FILTER (WHERE access_mode='demo' AND expires_at<=now()), count(*) FILTER (WHERE access_mode='owner') FROM sessions WHERE user_id=$1`, userID).Scan(&active, &expired, &owners); err != nil {
		t.Fatal(err)
	}
	if active != maxActiveDemoSessionsPerOwner || expired != 0 || owners != 1 {
		t.Fatalf("after replacement active demo=%d expired demo=%d owners=%d", active, expired, owners)
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
