package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
)

func TestInvalidOutputPreservesLastSuccessfulDatabaseValue(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	gameID := uuid.New()
	if err := database.UpsertGame(ctx, store.Game{ID: gameID, SteamAppID: 620, Name: "Portal 2", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	repository := NewSQLRepository(database)
	previous := GameFeatures{Themes: []string{"science fiction"}, Mechanics: []string{}, Modes: []string{"single-player"}, Tone: []string{}, EvidenceKeys: []string{"title"}}
	if err := repository.SaveGameFeatures(ctx, gameID, "gpt-6-luna", "previous-hash", previous); err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository, &fakeClient{}, "gpt-6-luna", "text-embedding-3-small")
	if err := service.processFeatures(ctx, gameID); err == nil {
		t.Fatal("invalid output was accepted")
	}
	var hash string
	var stored []byte
	if err := database.Pool.QueryRow(ctx, `SELECT input_content_hash,features FROM game_features WHERE game_id=$1 AND model='gpt-6-luna'`, gameID).Scan(&hash, &stored); err != nil {
		t.Fatal(err)
	}
	if hash != "previous-hash" || !strings.Contains(string(stored), "science fiction") {
		t.Fatalf("last successful value changed: hash=%q features=%s", hash, stored)
	}
}

func TestFeatureJobEnqueuesEmbeddingAfterValidatedFeatures(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	gameID := uuid.New()
	if err := database.UpsertGame(ctx, store.Game{ID: gameID, SteamAppID: 620, Name: "Portal 2", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, EmbeddingDimensions)
	client := &fakeClient{features: GameFeatures{Themes: []string{"puzzle"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"title"}}, vectors: [][]float32{vector}}
	queue := jobs.NewQueue(database)
	service, _ := NewService(NewSQLRepository(database), client, "gpt-6-luna", "text-embedding-3-small")
	service.SetQueue(queue)
	worker := jobs.NewWorker(queue, "ai-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
	jobID, err := queue.Enqueue(ctx, jobs.Spec{Kind: FeaturesJobKind, IdempotencyKey: "ai.features:v1:test", Payload: payload, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := database.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if status == "dead" || time.Now().After(deadline) {
			t.Fatalf("feature job status=%s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var features, embeddings int
	if err := database.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM game_features WHERE game_id=$1),(SELECT count(*) FROM game_embeddings WHERE game_id=$1)`, gameID).Scan(&features, &embeddings); err != nil {
		t.Fatal(err)
	}
	if features != 1 || embeddings != 1 {
		t.Fatalf("features/embeddings=%d/%d", features, embeddings)
	}
}

func openAIIntegrationStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ai_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolate := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	database, err := store.Open(ctx, isolate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup, err := pgx.Connect(cleanupCtx, baseURL)
		if err == nil {
			_, _ = cleanup.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(cleanupCtx)
		}
	})
	return database
}
