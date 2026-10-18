package profile

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

func TestLatestProfileReportsMissingSummaryAsUnavailable(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID, profileID := uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,summary,algorithm_metadata) VALUES($1,$2,1,NULL,'{"confidence":0.5,"evidenceCount":2}')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}

	got, err := repository.LatestProfile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "" || got.SummaryStatus != SummaryUnavailable {
		t.Fatalf("missing summary = %q/%q, want empty/%q", got.Summary, got.SummaryStatus, SummaryUnavailable)
	}

	if _, err := database.Pool.Exec(ctx, `UPDATE preference_profiles SET summary='Любит вдумчивые кооперативные игры.' WHERE id=$1`, profileID); err != nil {
		t.Fatal(err)
	}
	got, err = repository.LatestProfile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Любит вдумчивые кооперативные игры." || got.SummaryStatus != SummaryReady {
		t.Fatalf("ready summary = %q/%q", got.Summary, got.SummaryStatus)
	}
}

func TestLaterRebuildEnqueuesFreshIdempotentSummaryAfterDeadAttempt(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(database)
	service.SetQueue(queue)
	worker := jobs.NewWorker(queue, "profile-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second), jobs.WithRetryBase(time.Millisecond))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("ai.profile_summary", func(context.Context, jobs.Lease) error { return permanentSummaryError{} }); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, UserID: userID})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker: %v", err)
		}
	})

	for _, key := range []string{"sync:first:profile:v1", "sync:later:profile:v1"} {
		if _, err := queue.Enqueue(ctx, jobs.Spec{Kind: JobKind, IdempotencyKey: key, Payload: payload, MaxAttempts: 3}); err != nil {
			t.Fatal(err)
		}
		waitForProfileJobs(t, database, key)
	}

	var profiles, summaries, distinctSummaryKeys int
	if err := database.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM preference_profiles WHERE user_id=$1),
		(SELECT count(*) FROM sync_jobs WHERE kind='ai.profile_summary' AND user_id=$1),
		(SELECT count(DISTINCT idempotency_key) FROM sync_jobs WHERE kind='ai.profile_summary' AND user_id=$1)`, userID).Scan(&profiles, &summaries, &distinctSummaryKeys); err != nil {
		t.Fatal(err)
	}
	if profiles != 2 || summaries != 2 || distinctSummaryKeys != 2 {
		t.Fatalf("profiles/summaries/distinct keys = %d/%d/%d, want 2/2/2", profiles, summaries, distinctSummaryKeys)
	}
}

type permanentSummaryError struct{}

func (permanentSummaryError) Error() string   { return "summary unavailable" }
func (permanentSummaryError) Retryable() bool { return false }

func waitForProfileJobs(t *testing.T, database *store.Store, profileKey string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var profileStatus, summaryStatus string
		err := database.Pool.QueryRow(context.Background(), `SELECT profile.status, summary.status
			FROM sync_jobs profile
			JOIN sync_jobs summary ON summary.idempotency_key=profile.idempotency_key || ':summary:v1'
			WHERE profile.idempotency_key=$1`, profileKey).Scan(&profileStatus, &summaryStatus)
		if err == nil && profileStatus == "succeeded" && summaryStatus == "dead" {
			return
		}
		if err != nil && !errorsIsNoRows(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("profile %q did not complete with an independently dead summary job: profile=%q summary=%q err=%v", profileKey, profileStatus, summaryStatus, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func errorsIsNoRows(err error) bool { return err == pgx.ErrNoRows }

func openProfileStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = connection.Close(ctx)
		t.Fatal(err)
	}
	_ = connection.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	database, err := store.Open(ctx, fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanup, cleanupErr := pgx.Connect(context.Background(), baseURL)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return database
}
