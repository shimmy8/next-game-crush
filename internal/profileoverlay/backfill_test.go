package profileoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
	"next-game-crush/internal/store"
)

func TestBoundaryBackfillPreservesLegacyDuringRefreshAndPublishesInsufficientEmptyState(t *testing.T) {
	database := openBackfillStore(t)
	ctx := context.Background()
	queue := jobs.NewQueue(database)
	userID, legacyID := uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,summary,publication_state,published_at,summary_source,build_key)
		VALUES($2,$1,1,'Legacy summary','published',now(),'legacy','legacy-build')`, userID, legacyID); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBoundaryBackfill(ctx, database, queue); err != nil {
		t.Fatal(err)
	}
	profileRepository, err := profile.NewSQLRepository(database, "text-embedding-3-small")
	if err != nil {
		t.Fatal(err)
	}
	profileService, err := profile.NewService(profileRepository)
	if err != nil {
		t.Fatal(err)
	}
	profileService.SetQueue(queue)
	legacy, err := profileService.Latest(ctx, userID)
	if err != nil || legacy.SummarySource != profile.SummarySourceLegacy || !legacy.Updating {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}

	preferenceRepository, err := preferences.NewSQLRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	preferenceService, err := preferences.NewService(preferenceRepository, queue)
	if err != nil {
		t.Fatal(err)
	}
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "boundary-real", jobs.WithPollInterval(5*time.Millisecond), jobs.WithHeartbeatInterval(20*time.Millisecond))
	if err := profileService.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := preferenceService.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register(recommendations.JobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload recommendations.JobPayload
		if err := json.Unmarshal(lease.Payload, &payload); err != nil {
			return err
		}
		var state, buildKey string
		if err := database.Pool.QueryRow(ctx, `SELECT publication_state,build_key FROM preference_profiles WHERE user_id=$1 ORDER BY version DESC LIMIT 1`, userID).Scan(&state, &buildKey); err != nil {
			return err
		}
		if state != "insufficient" || payload.ProfileBuildKey != buildKey {
			return fmt.Errorf("ranking handoff state=%s payload=%q build=%q", state, payload.ProfileBuildKey, buildKey)
		}
		var overlays, vectors int
		if err := database.Pool.QueryRow(ctx, `SELECT count(*),(SELECT count(*) FROM preference_overlay_embeddings) FROM preference_overlays WHERE user_id=$1`, userID).Scan(&overlays, &vectors); err != nil {
			return err
		}
		if overlays != 1 || vectors != 0 {
			return fmt.Errorf("empty overlay rows=%d vectors=%d", overlays, vectors)
		}
		return queue.RecordResult(ctx, lease, map[string]any{"rankingVersion": 1})
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterBoundaryBackfill(worker, queue); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := database.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE kind=$1 AND user_id=$2`, BackfillJobKind, userID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if status == "dead" || time.Now().After(deadline) {
			t.Fatalf("root status=%s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := profileService.Latest(ctx, userID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("public profile after insufficient err=%v", err)
	}
}

func TestBoundaryBackfillLegacyProfileReplacementIsAtomic(t *testing.T) {
	database := openBackfillStore(t)
	ctx := context.Background()
	queue := jobs.NewQueue(database)
	userID, legacyID, nextID := uuid.New(), uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,summary,publication_state,published_at,summary_source,build_key)
		VALUES($2,$1,1,'Legacy summary','published',now(),'legacy','legacy-build')`, userID, legacyID); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBoundaryBackfill(ctx, database, queue); err != nil {
		t.Fatal(err)
	}
	repository, err := profile.NewSQLRepository(database, "text-embedding-3-small")
	if err != nil {
		t.Fatal(err)
	}
	service, err := profile.NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if before, err := service.Latest(ctx, userID); err != nil || before.ID != legacyID || !before.Updating {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,publication_state,build_key,algorithm_metadata)
		VALUES($2,$1,2,'building','activity-build','{"algorithmVersion":"activity-profile-v5"}')`, userID, nextID); err != nil {
		t.Fatal(err)
	}
	if during, err := service.Latest(ctx, userID); err != nil || during.ID != legacyID || !during.Updating || during.Summary != "Legacy summary" {
		t.Fatalf("during=%+v err=%v", during, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE preference_profiles SET summary='Activity summary',summary_source='deterministic',publication_state='published',published_at=now() WHERE id=$1 AND publication_state='building'`, nextID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE sync_jobs SET status='succeeded',updated_at=now() WHERE kind=$1 AND user_id=$2`, BackfillJobKind, userID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := service.Latest(ctx, userID)
	if err != nil || after.ID != nextID || after.Version != 2 || after.Summary != "Activity summary" || after.SummarySource != profile.SummarySourceDeterministic || after.Updating {
		t.Fatalf("after=%+v err=%v", after, err)
	}
}

func TestBoundaryBackfillIsPerUserIdempotentAndUsesSavedSnapshots(t *testing.T) {
	database := openBackfillStore(t)
	ctx := context.Background()
	queue := jobs.NewQueue(database)
	if err := EnsureBoundaryBackfill(ctx, database, queue); err != nil {
		t.Fatal(err)
	}
	var roots int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1`, BackfillJobKind).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	if roots != 0 {
		t.Fatalf("empty database roots=%d", roots)
	}

	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBoundaryBackfill(ctx, database, queue); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBoundaryBackfill(ctx, database, queue); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := database.Pool.QueryRow(ctx, `SELECT count(*),min(idempotency_key) FROM sync_jobs WHERE kind=$1 AND user_id=$2`, BackfillJobKind, userID).Scan(&roots, &key); err != nil {
		t.Fatal(err)
	}
	if roots != 1 || !strings.Contains(key, "boundary:v1") {
		t.Fatalf("roots=%d key=%q", roots, key)
	}

	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "boundary-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithHeartbeatInterval(20*time.Millisecond))
	var mu sync.Mutex
	order := []string{}
	record := func(name string) { mu.Lock(); order = append(order, name); mu.Unlock() }
	if err := worker.Register(profile.JobKind, func(ctx context.Context, lease jobs.Lease) error {
		record("profile")
		return queue.RecordResult(ctx, lease, map[string]any{"profileBuildKey": "saved-activity-snapshot", "profileVersion": 2})
	}); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register(preferences.OverlayJobKind, func(ctx context.Context, lease jobs.Lease) error {
		record("overlay")
		return queue.RecordResult(ctx, lease, map[string]any{"preferenceOverlayVersion": 3})
	}); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register(recommendations.JobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload recommendations.JobPayload
		if err := json.Unmarshal(lease.Payload, &payload); err != nil {
			return err
		}
		if payload.ProfileBuildKey != "saved-activity-snapshot" {
			return fmt.Errorf("profile handoff=%q", payload.ProfileBuildKey)
		}
		record("ranking")
		return queue.RecordResult(ctx, lease, map[string]any{"rankingVersion": 4})
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterBoundaryBackfill(worker, queue); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := database.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE kind=$1 AND user_id=$2`, BackfillJobKind, userID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if status == "dead" || time.Now().After(deadline) {
			t.Fatalf("root status=%s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotOrder := append([]string(nil), order...)
	mu.Unlock()
	if strings.Join(gotOrder, ",") != "profile,overlay,ranking" {
		t.Fatalf("stage order=%v", gotOrder)
	}
	var summaries int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='ai.profile_summary'`).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if summaries != 0 {
		t.Fatalf("coordinator scheduled optional summary: %d", summaries)
	}
}

func openBackfillStore(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Path != "/next_game_crush_test" {
		t.Fatal("requires isolated test database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := "profileoverlay_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	database, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanup, openErr := pgx.Connect(context.Background(), base)
		if openErr == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return database
}
