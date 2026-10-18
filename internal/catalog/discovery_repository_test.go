package catalog

import (
	"context"
	"encoding/json"
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
	"next-game-crush/internal/store"
)

func openCatalogTestStore(t *testing.T) *store.Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Path != "/next_game_crush_test" && parsed.Path != "/next_game_crush_e2e") {
		t.Fatal("requires isolated test database")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "catalog_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	t.Cleanup(func() {
		conn, e := pgx.Connect(context.Background(), raw)
		if e == nil {
			defer conn.Close(context.Background())
			_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	})
	q := parsed.Query()
	q.Set("search_path", schema+",public")
	parsed.RawQuery = q.Encode()
	s, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestDiscoveryRepositoryUpsertsRevisionAndBudget(t *testing.T) {
	s := openCatalogTestStore(t)
	r := NewDiscoveryRepository(s)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	retry := now.Add(time.Hour)
	o := Observation{Revision: Revision{AppID: 42, LastModified: now}, Status: "failed", Attempts: 1, NextRetryAt: &retry, SafeError: "request_failed"}
	if err := r.SaveObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT first_observed_at FROM catalog_discovery_observations`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	o.Status = "admitted"
	o.Attempts = 2
	o.RetryGeneration = 1
	o.Window = ParseReleaseDate("2027", now)
	if err := r.SaveObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
	var count, attempts int
	var next *time.Time
	var safe string
	var retained time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) OVER(),attempts,next_retry_at,safe_error,first_observed_at FROM catalog_discovery_observations`).Scan(&count, &attempts, &next, &safe, &retained); err != nil {
		t.Fatal(err)
	}
	if count != 1 || attempts != 2 || next != nil || safe != "" || !retained.Equal(first) {
		t.Fatal("revision update lost idempotence/history")
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := r.Reserve(ctx, now, "duplicate", 200)
			if err != nil || !ok {
				t.Errorf("duplicate reservation: %v", err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 201; i++ {
		if _, err := r.Reserve(ctx, now, uuid.NewString(), 200); err != nil {
			t.Fatal(err)
		}
	}
	var used int
	if err := s.Pool.QueryRow(ctx, `SELECT used FROM catalog_discovery_budget`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 200 {
		t.Fatalf("used=%d", used)
	}
	if ok, err := r.Reserve(ctx, now, "duplicate", 200); err != nil || !ok {
		t.Fatal("duplicate rejected after capacity")
	}
}

func TestDiscoveryRepositoryKeepsDistinctRevisions(t *testing.T) {
	s := openCatalogTestStore(t)
	r := NewDiscoveryRepository(s)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, when := range []time.Time{now, now.Add(-time.Hour)} {
		if err := r.SaveObservation(ctx, Observation{Revision: Revision{AppID: 42, LastModified: when}, Status: "released", Attempts: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_discovery_observations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatal("revisions collapsed")
	}
	start, err := r.BeginScan(ctx, DiscoveryIncremental, now)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := r.BeginScan(ctx, DiscoveryIncremental, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if start.WatermarkAt == nil || !start.WatermarkAt.Equal(now.Add(-24*time.Hour)) || !resumed.ScanStartedAt.Equal(*start.ScanStartedAt) {
		t.Fatal("unfinished scan restarted")
	}
}

func TestDiscoveryPresentationStatus(t *testing.T) {
	s := openCatalogTestStore(t)
	r := NewDiscoveryRepository(s)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		enabled bool
		sql     string
		want    string
	}{
		{"disabled", false, "", "disabled"},
		{"initial", true, "", "updating"},
		{"ready", true, `UPDATE catalog_discovery_state SET availability='available',scan_started_at='2026-10-18 11:00:00+00',scan_completed_at='2026-10-18 11:30:00+00',watermark_at='2026-10-18 11:00:00+00'`, "ready"},
		{"stale", true, `UPDATE catalog_discovery_state SET scan_completed_at='2026-10-18 08:00:00+00',scan_started_at='2026-10-18 07:00:00+00',watermark_at='2026-10-18 07:00:00+00'`, "stale"},
		{"degraded", true, `UPDATE catalog_discovery_state SET availability='request_failed',last_error='request_failed' WHERE mode='incremental'`, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sql != "" {
				if _, err := s.Pool.Exec(ctx, tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			status, err := r.PresentationStatus(ctx, tc.enabled, now, time.Hour)
			if err != nil || status.Status != tc.want {
				t.Fatalf("status=%+v error=%v want %s", status, err, tc.want)
			}
		})
	}
	game := uuid.New()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,42,'Game','steam',now());`, game); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_entry_sources(game_id,source) VALUES($1,'seed'),($1,'steam_discovery')`, game); err != nil {
		t.Fatal(err)
	}
	status, err := r.PresentationStatus(ctx, true, now, time.Hour)
	if err != nil || status.ActiveDiscoveredGames != 1 {
		t.Fatal("overlap count is not distinct")
	}
}

func TestDiscoveryDiagnosticsAreAggregateAndSafe(t *testing.T) {
	s := openCatalogTestStore(t)
	r := NewDiscoveryRepository(s)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	gameID, wishlistGameID, userID := uuid.New(), uuid.New(), uuid.New()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,42,'Discovered','steam',now()),($2,43,'Wishlist only','steam',now())`, gameID, wishlistGameID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_entry_sources(game_id,source) VALUES($1,'seed'),($1,'steam_discovery')`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,priority,source,last_successful_fetch_at) VALUES($1,$2,1,'steam',now())`, userID, wishlistGameID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE catalog_discovery_state SET cursor_app_id=77 WHERE mode='backfill'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_discovery_budget(hour_at,used,counts) VALUES($1,4,'{"evaluated":3}')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status,last_attempt_at) VALUES
		('steam',42,$1,'admitted',$2),('steam',44,$1,'outside_horizon',$2),('steam',45,$1,'failed',$2),
		('steam',46,$1,'admitted',$1)`, now, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(s)
	for _, appID := range []int64{42, 44, 45} {
		payload, _ := json.Marshal(QualificationPayload{Version: 1, App: DiscoveredApp{AppID: appID, Name: "fixture", LastModified: now}, BudgetHour: now, Priority: 50})
		jobID, err := queue.Enqueue(ctx, jobs.Spec{Kind: QualifyJobKind, IdempotencyKey: fmt.Sprintf("completed-%d", appID), Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE sync_jobs SET status='succeeded' WHERE id=$1`, jobID); err != nil {
			t.Fatal(err)
		}
	}
	pendingRetry, _ := json.Marshal(QualificationPayload{Version: 1, App: DiscoveredApp{AppID: 42, Name: "fixture", LastModified: now}, RetryGeneration: 1, BudgetHour: now, Priority: 50})
	if _, err := queue.Enqueue(ctx, jobs.Spec{Kind: QualifyJobKind, IdempotencyKey: "pending-newer-retry", Payload: pendingRetry}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pending-a", "pending-b"} {
		if _, err := queue.Enqueue(ctx, jobs.Spec{Kind: QualifyJobKind, IdempotencyKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	diagnostics, err := r.Diagnostics(ctx, true, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.ActiveDiscoveredGames != 1 || diagnostics.ActiveBySource["seed"] != 1 || diagnostics.ActiveBySource["steam_discovery"] != 1 || diagnostics.PendingQualifications != 3 || diagnostics.BackfillCursor != 77 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if diagnostics.LatestRun != (DiscoveryRunCounts{Discovered: 4, Evaluated: 3, Admitted: 1, Skipped: 1, Failed: 1}) {
		t.Fatalf("latest=%+v", diagnostics.LatestRun)
	}
	encoded, err := json.Marshal(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{userID.String(), gameID.String(), "Wishlist only"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("identifier escaped diagnostics: %s", encoded)
		}
	}
}

func TestDiscoveryDiagnosticsCountInvalidManifestItemsAsSkipped(t *testing.T) {
	s := openCatalogTestStore(t)
	r := NewDiscoveryRepository(s)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour)
	if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_discovery_budget(hour_at,used,counts) VALUES($1,2,'{"evaluated":3,"skipped_invalid":1}')`, hour); err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(s)
	for index, item := range []struct {
		appID  int64
		status string
	}{{71, "admitted"}, {72, "unavailable"}} {
		app := DiscoveredApp{AppID: item.appID, LastModified: hour.Add(time.Duration(index+1) * time.Second)}
		payload, _ := json.Marshal(QualificationPayload{Version: 1, App: app, BudgetHour: hour, Priority: 50})
		jobID, err := queue.Enqueue(ctx, jobs.Spec{Kind: QualifyJobKind, IdempotencyKey: fmt.Sprintf("diagnostic-mixed-%d", item.appID), Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE sync_jobs SET status='succeeded' WHERE id=$1`, jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status) VALUES('steam',$1,$2,$3)`, app.AppID, app.LastModified, item.status); err != nil {
			t.Fatal(err)
		}
	}
	manifest := PageManifest{Version: 1, Mode: DiscoveryIncremental, ScanStartedAt: hour, Items: []PageItem{{Ordinal: 0, Invalid: true}, {App: DiscoveredApp{AppID: 71, LastModified: hour.Add(time.Second)}, Ordinal: 1}, {App: DiscoveredApp{AppID: 72, LastModified: hour.Add(2 * time.Second)}, Ordinal: 2}}}
	result, _ := json.Marshal(manifest)
	payload, _ := json.Marshal(ScanPayload{Version: 1, Mode: DiscoveryIncremental, ScanStartedAt: hour, BudgetHour: hour})
	scanID, err := queue.Enqueue(ctx, jobs.Spec{Kind: IncrementalJobKind, IdempotencyKey: "diagnostic-mixed-page", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE sync_jobs SET status='succeeded',result=$2 WHERE id=$1`, scanID, result); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := r.Diagnostics(ctx, true, hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.LatestRun != (DiscoveryRunCounts{Discovered: 2, Evaluated: 3, Admitted: 1, Skipped: 2}) {
		t.Fatalf("latest=%+v", diagnostics.LatestRun)
	}
}
