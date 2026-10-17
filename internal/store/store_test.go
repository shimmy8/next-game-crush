package store

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestMigrateCreatesRequiredTables(t *testing.T) {
	db := openTestStore(t)
	rows, err := db.Pool.Query(context.Background(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	required := []string{
		"users", "sessions", "steam_accounts", "games", "game_features",
		"game_embeddings", "game_release_dates", "user_games", "wishlist_entries",
		"achievement_summaries", "preference_profiles", "profile_embeddings",
		"recommendations", "recommendation_factors", "feedback_events",
		"feedback_classifications", "notification_preferences", "email_deliveries",
		"sync_jobs", "job_attempts",
	}
	for _, name := range required {
		if !got[name] {
			t.Errorf("migration did not create %s", name)
		}
	}
	for table, columns := range map[string][]string{
		"game_embeddings":    {"input_content_hash", "source", "last_successful_fetch_at"},
		"profile_embeddings": {"input_content_hash", "source", "last_successful_fetch_at"},
		"recommendations":    {"explanation_input_content_hash"},
	} {
		for _, column := range columns {
			var exists bool
			if err := db.Pool.QueryRow(context.Background(), `SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2
			)`, table, column).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Errorf("migration did not create %s.%s", table, column)
			}
		}
	}
	for _, table := range []string{"game_embeddings", "profile_embeddings"} {
		var dataType string
		if err := db.Pool.QueryRow(context.Background(), `
			SELECT format_type(a.atttypid, a.atttypmod)
			FROM pg_attribute a
			JOIN pg_class c ON c.oid=a.attrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=current_schema() AND c.relname=$1 AND a.attname='embedding'`, table).Scan(&dataType); err != nil {
			t.Fatal(err)
		}
		if dataType != "vector(1536)" {
			t.Errorf("%s.embedding type = %q, want vector(1536)", table, dataType)
		}
	}
}

func TestEmbeddingRoundTripAndCosineQuery(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	gameID := uuid.New()
	if err := db.UpsertGame(ctx, Game{ID: gameID, SteamAppID: 620, Name: "Portal 2", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, EmbeddingDimensions)
	for i := range vector {
		vector[i] = float32(i%17) / 17
	}
	if err := db.PutGameEmbedding(ctx, GameEmbedding{GameID: gameID, Model: "test-model", ContentHash: "sha256:test", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGameEmbedding(ctx, GameEmbedding{GameID: gameID, Model: "other-model", ContentHash: "sha256:other", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetGameEmbedding(ctx, gameID, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Vector) != EmbeddingDimensions {
		t.Fatalf("embedding length = %d, want %d", len(got.Vector), EmbeddingDimensions)
	}
	for i := range vector {
		if got.Vector[i] != vector[i] {
			t.Fatalf("embedding[%d] = %v, want %v", i, got.Vector[i], vector[i])
		}
	}
	nearest, err := db.NearestGameEmbeddings(ctx, "test-model", vector, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nearest) != 1 || nearest[0].GameID != gameID || nearest[0].Model != "test-model" || math.Abs(nearest[0].CosineDistance) > 1e-6 {
		t.Fatalf("nearest = %#v, want game %s at distance zero", nearest, gameID)
	}
}

func TestEmbeddingRejectsWrongDimensions(t *testing.T) {
	db := openTestStore(t)
	err := db.PutGameEmbedding(context.Background(), GameEmbedding{
		GameID: uuid.New(), Model: "test-model", ContentHash: "bad", Vector: []float32{1, 2, 3},
	})
	if err == nil || !strings.Contains(err.Error(), "1536") {
		t.Fatalf("error = %v, want 1536-dimension validation", err)
	}
}

func TestEmbeddingRefreshUpdatesLastSuccessfulFetchTime(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	gameID := uuid.New()
	if err := db.UpsertGame(ctx, Game{ID: gameID, SteamAppID: 10, Name: "Counter-Strike", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, EmbeddingDimensions)
	if err := db.PutGameEmbedding(ctx, GameEmbedding{GameID: gameID, Model: "test-model", ContentHash: "old", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Pool.Exec(ctx, "UPDATE game_embeddings SET last_successful_fetch_at=$1 WHERE game_id=$2", old, gameID); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGameEmbedding(ctx, GameEmbedding{GameID: gameID, Model: "test-model", ContentHash: "new", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	var refreshed time.Time
	if err := db.Pool.QueryRow(ctx, "SELECT last_successful_fetch_at FROM game_embeddings WHERE game_id=$1 AND model='test-model'", gameID).Scan(&refreshed); err != nil {
		t.Fatal(err)
	}
	if !refreshed.After(old) {
		t.Fatalf("last_successful_fetch_at = %s, want after %s", refreshed, old)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		conn.Close(ctx)
		t.Fatalf("create isolated schema: %v", err)
	}
	conn.Close(ctx)

	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolatedURL := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	db, err := Open(context.Background(), isolatedURL)
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup, err := pgx.Connect(cleanupCtx, baseURL)
		if err == nil {
			_, _ = cleanup.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(cleanupCtx)
		}
	})
	return db
}
