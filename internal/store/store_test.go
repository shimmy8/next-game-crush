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
	dbgen "next-game-crush/internal/store/sqlc"
)

func TestSchemaTablesExist(t *testing.T) {
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
		"sync_jobs", "job_attempts", "game_compatibility",
		"game_achievement_definitions", "user_achievement_unlocks",
		"achievement_semantic_classification_generations", "achievement_semantic_classifications",
		"user_game_completion_evidence", "user_game_completion_projections",
	}
	for _, name := range required {
		if !got[name] {
			t.Errorf("migration did not create %s", name)
		}
	}
	for table, columns := range map[string][]string{
		"game_embeddings":    {"input_content_hash", "source", "last_successful_fetch_at"},
		"profile_embeddings": {"input_content_hash", "source", "last_successful_fetch_at"},
		"recommendations":    {"explanation_input_content_hash", "explanation_source", "explanation_failure_reason"},
		"feedback_events":    {"reasons"},
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
	ctx := context.Background()
	userID, gameID, profileID, recommendationID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,620,'Portal 2','catalog_seed',now())`, []any{gameID}},
		{`INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score) VALUES($1,$2,$3,$4,'test',1)`, []any{recommendationID, userID, gameID, profileID}},
	} {
		if _, err := db.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE recommendations SET explanation_source='provider' WHERE id=$1`, recommendationID); err == nil {
		t.Fatal("invalid explanation source unexpectedly accepted")
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE recommendations SET explanation_failure_reason='raw-provider-error' WHERE id=$1`, recommendationID); err == nil {
		t.Fatal("invalid explanation failure reason unexpectedly accepted")
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

func TestGameCompatibilityQueriesPreserveIndependentFreshness(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	gameID := uuid.New()
	if err := db.UpsertGame(ctx, Game{ID: gameID, SteamAppID: 1203, Name: "Compatibility", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	second, third, fourth := first.Add(time.Hour), first.Add(2*time.Hour), first.Add(3*time.Hour)
	if err := db.Queries.RecordDeckCompatibilityFailure(ctx, dbgen.RecordDeckCompatibilityFailureParams{
		GameID: uuidToPG(gameID), DeckAvailability: "request_failed", DeckLastAttemptAt: timestamptz(first),
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM game_compatibility WHERE game_id=$1`, gameID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failure created unobserved platforms: rows=%d err=%v", count, err)
	}
	upsert := dbgen.UpsertGamePlatformsParams{
		GameID: uuidToPG(gameID), Windows: true, Mac: false, Linux: true, PlatformsLastSuccessfulFetchAt: timestamptz(first),
	}
	if err := db.Queries.UpsertGamePlatforms(ctx, upsert); err != nil {
		t.Fatal(err)
	}
	type compatibilityState struct {
		windows, mac, linux bool
		platformsAt         time.Time
		status, available   string
		attempt, success    *time.Time
	}
	load := func() compatibilityState {
		t.Helper()
		var state compatibilityState
		if err := db.Pool.QueryRow(ctx, `SELECT windows,mac,linux,platforms_last_successful_fetch_at,deck_status,deck_availability,deck_last_attempt_at,deck_last_successful_fetch_at FROM game_compatibility WHERE game_id=$1`, gameID).Scan(&state.windows, &state.mac, &state.linux, &state.platformsAt, &state.status, &state.available, &state.attempt, &state.success); err != nil {
			t.Fatal(err)
		}
		return state
	}
	assertPlatforms := func(state compatibilityState, windows, mac, linux bool, at time.Time) {
		t.Helper()
		if state.windows != windows || state.mac != mac || state.linux != linux || !state.platformsAt.Equal(at) {
			t.Fatalf("unexpected platforms/freshness: %+v", state)
		}
	}
	assertDeck := func(state compatibilityState, status, availability string, attempt, success time.Time) {
		t.Helper()
		if state.status != status || state.available != availability || state.attempt == nil || !state.attempt.Equal(attempt) || state.success == nil || !state.success.Equal(success) {
			t.Fatalf("unexpected Deck state/freshness: %+v", state)
		}
	}
	initial := load()
	assertPlatforms(initial, true, false, true, first)
	if initial.status != "unknown" || initial.available != "unknown" || initial.attempt != nil || initial.success != nil {
		t.Fatalf("platform observation invented Deck result: %+v", initial)
	}
	if err := db.Queries.RecordDeckCompatibilitySuccess(ctx, dbgen.RecordDeckCompatibilitySuccessParams{
		GameID: uuidToPG(gameID), DeckStatus: "verified", DeckAvailability: "available",
		DeckLastAttemptAt: timestamptz(second), DeckLastSuccessfulFetchAt: timestamptz(second),
	}); err != nil {
		t.Fatal(err)
	}
	succeeded := load()
	assertPlatforms(succeeded, true, false, true, first)
	assertDeck(succeeded, "verified", "available", second, second)
	upsert.Windows, upsert.Mac, upsert.Linux = false, true, false
	upsert.PlatformsLastSuccessfulFetchAt = timestamptz(third)
	if err := db.Queries.UpsertGamePlatforms(ctx, upsert); err != nil {
		t.Fatal(err)
	}
	refreshed := load()
	assertPlatforms(refreshed, false, true, false, third)
	assertDeck(refreshed, "verified", "available", second, second)
	for _, availability := range []string{"request_failed", "unavailable"} {
		if err := db.Queries.RecordDeckCompatibilityFailure(ctx, dbgen.RecordDeckCompatibilityFailureParams{
			GameID: uuidToPG(gameID), DeckAvailability: availability, DeckLastAttemptAt: timestamptz(fourth),
		}); err != nil {
			t.Fatal(err)
		}
		failed := load()
		assertPlatforms(failed, false, true, false, third)
		assertDeck(failed, "verified", availability, fourth, second)
	}
	if err := db.Queries.RecordDeckCompatibilitySuccess(ctx, dbgen.RecordDeckCompatibilitySuccessParams{
		GameID: uuidToPG(gameID), DeckStatus: "unknown", DeckAvailability: "available",
		DeckLastAttemptAt: timestamptz(fourth), DeckLastSuccessfulFetchAt: timestamptz(fourth),
	}); err != nil {
		t.Fatal(err)
	}
	unknown := load()
	assertPlatforms(unknown, false, true, false, third)
	assertDeck(unknown, "unknown", "available", fourth, fourth)
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
