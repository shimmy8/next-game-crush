package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

func scanLease(t *testing.T, s *DiscoveryService, mode DiscoveryMode, now time.Time, generation int) (jobs.Lease, ScanPayload) {
	t.Helper()
	state, err := s.repo.BeginScan(context.Background(), mode, now)
	if err != nil {
		t.Fatal(err)
	}
	p := ScanPayload{Version: 1, Mode: mode, ScanStartedAt: *state.ScanStartedAt, AfterAppID: state.CursorAppID, BudgetHour: now.UTC().Truncate(time.Hour), PageGeneration: generation}
	raw, _ := json.Marshal(p)
	id, err := s.queue.Enqueue(context.Background(), jobs.Spec{Lane: jobs.LaneInteractive, Kind: "catalog.discover." + string(mode), Payload: raw, IdempotencyKey: scanKey(p), Priority: scanPriority(mode)})
	if err != nil {
		t.Fatal(err)
	}
	return leaseDiscoveryID(t, s, id), p
}
func leaseDiscoveryID(t *testing.T, s *DiscoveryService, id uuid.UUID) jobs.Lease {
	t.Helper()
	l := jobs.Lease{ID: id, WorkerID: uuid.NewString(), FencingToken: uuid.New()}
	err := s.repo.store.Pool.QueryRow(context.Background(), `UPDATE sync_jobs SET status='running',worker_id=$2,fencing_token=$3,lease_until=now()+interval '1 minute',attempts=GREATEST(attempts,1) WHERE id=$1
		RETURNING lane,kind,payload,idempotency_key,attempts,max_attempts,priority,COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid),result`,
		id, l.WorkerID, l.FencingToken).Scan(&l.Lane, &l.Kind, &l.Payload, &l.IdempotencyKey, &l.Attempts, &l.MaxAttempts, &l.Priority, &l.UserID, &l.Result)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func TestDiscoveryPageReplayAfterRestart(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	lease, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	page := DiscoveryPage{Apps: []DiscoveredApp{{AppID: 42, LastModified: now}}, NextAppID: 42}
	m, err := s.repo.SchedulePage(ctx, lease, p, page, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{}, s.cfg)
	if err != nil || replayed.Items[0].JobID != m.Items[0].JobID {
		t.Fatalf("manifest replay: %v", err)
	}
	restarted, err := NewDiscoveryService(s.repo, s.source, s.metadata, s.queue, s.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.handleScan(ctx, lease); !errors.Is(err, jobs.ErrDeferred) {
		t.Fatalf("pending page=%v", err)
	}
	child := leaseDiscoveryID(t, s, m.Items[0].JobID)
	if err = s.handleQualification(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err = s.queue.Complete(ctx, child); err != nil {
		t.Fatal(err)
	}
	lease = leaseDiscoveryID(t, s, lease.ID)
	if err = restarted.handleScan(ctx, lease); err != nil {
		t.Fatal(err)
	}
	state, err := s.repo.State(ctx, DiscoveryIncremental)
	if err != nil {
		t.Fatal(err)
	}
	if state.WatermarkAt == nil || !state.WatermarkAt.Equal(now) {
		t.Fatal("watermark advanced to completion instead of captured scan start")
	}
	var used, observations int
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT (SELECT used FROM catalog_discovery_budget),(SELECT count(*) FROM catalog_discovery_observations)`).Scan(&used, &observations); err != nil {
		t.Fatal(err)
	}
	if used != 1 || observations != 1 {
		t.Fatalf("restart duplicated work: used=%d observations=%d", used, observations)
	}
}
func TestDiscoveryPageWaitsForTerminalItems(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	lease, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	m, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{Apps: []DiscoveredApp{{AppID: 42, LastModified: now}, {AppID: 0}}, NextAppID: 42}, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ready, _, err := s.repo.ReconcilePage(ctx, lease, m, now)
	if err != nil || ready {
		t.Fatalf("pending ready=%v error=%v", ready, err)
	}
	state, _ := s.repo.State(ctx, DiscoveryIncremental)
	if state.WatermarkAt.Equal(now) {
		t.Fatal("checkpoint committed early")
	}
	if _, err = s.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',attempts=max_attempts WHERE id=$1`, m.Items[0].JobID); err != nil {
		t.Fatal(err)
	}
	ready, _, err = s.repo.ReconcilePage(ctx, lease, m, now)
	if err != nil || !ready {
		t.Fatalf("terminal ready=%v error=%v", ready, err)
	}
	var status string
	var retry time.Time
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT status,next_retry_at FROM catalog_discovery_observations`).Scan(&status, &retry); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !retry.Equal(now.Add(time.Hour)) {
		t.Fatal("terminal failure missing durable retry")
	}
	var skippedInvalid int
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT COALESCE((counts->>'skipped_invalid')::integer,0) FROM catalog_discovery_budget WHERE hour_at=$1`, p.BudgetHour).Scan(&skippedInvalid); err != nil {
		t.Fatal(err)
	}
	if skippedInvalid != 1 {
		t.Fatalf("invalid manifest skips=%d, want 1", skippedInvalid)
	}
	ready, advanced, err := s.repo.ReconcilePage(ctx, lease, m, now)
	if err != nil || !ready || advanced {
		t.Fatalf("replayed terminal page ready=%v advanced=%v error=%v", ready, advanced, err)
	}
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT COALESCE((counts->>'skipped_invalid')::integer,0) FROM catalog_discovery_budget WHERE hour_at=$1`, p.BudgetHour).Scan(&skippedInvalid); err != nil {
		t.Fatal(err)
	}
	if skippedInvalid != 1 {
		t.Fatalf("replayed invalid manifest skips=%d, want 1", skippedInvalid)
	}
}
func TestOptionalQualificationLeaseExhaustion(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	lease, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	m, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{Apps: []DiscoveredApp{{AppID: 42, LastModified: now}}, NextAppID: 42}, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='running',worker_id='crashed',fencing_token=$2,lease_until=now()-interval '1 minute',attempts=max_attempts WHERE id=$1`, m.Items[0].JobID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	ready, _, err := s.repo.ReconcilePage(ctx, lease, m, now)
	if err != nil || !ready {
		t.Fatalf("exhausted page: %v %v", ready, err)
	}
	var status string
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, m.Items[0].JobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Fatal("exhausted child still active")
	}
}
func TestDiscoveryMalformedEnvelopeKeepsCheckpoint(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	lease, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	if _, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{HasMore: true}, s.cfg); err == nil {
		t.Fatal("nonprogressing page accepted")
	}
	state, _ := s.repo.State(ctx, DiscoveryIncremental)
	if state.CursorAppID != 0 || state.ScanCompletedAt != nil {
		t.Fatal("malformed page moved checkpoint")
	}
}
func TestConcurrentCoordinatorsReserveOnce(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	app := DiscoveredApp{AppID: 42, LastModified: now.Add(-time.Hour)}
	due := now.Add(-time.Second)
	if err := s.repo.SaveObservation(ctx, Observation{Revision: Revision{AppID: app.AppID, LastModified: app.LastModified}, Status: "failed", Attempts: 3, NextRetryAt: &due}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(QualificationPayload{Version: 1, App: app, RetryGeneration: 0, BudgetHour: now.Truncate(time.Hour), Priority: 50})
	old, err := s.queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: QualifyJobKind, Payload: raw, IdempotencyKey: qualificationKey(app, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',attempts=max_attempts WHERE id=$1`, old); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.repo.ScheduleDueRetries(ctx, now, s.cfg); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	var count, generation, used int
	var oldStatus string
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sync_jobs WHERE kind='catalog.qualify'),(SELECT retry_generation FROM catalog_discovery_observations),(SELECT used FROM catalog_discovery_budget),(SELECT status FROM sync_jobs WHERE id=$1)`, old).Scan(&count, &generation, &used, &oldStatus); err != nil {
		t.Fatal(err)
	}
	if count != 2 || generation != 1 || used != 1 || oldStatus != "dead" {
		t.Fatal("retry generation was reserved twice or history was rearmed")
	}
	var id uuid.UUID
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE id<>$1`, old).Scan(&id); err != nil {
		t.Fatal(err)
	}
	l := leaseDiscoveryID(t, s, id)
	if err = s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	var observations int
	var next *time.Time
	var status string
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) OVER(),status,next_retry_at FROM catalog_discovery_observations`).Scan(&observations, &status, &next); err != nil {
		t.Fatal(err)
	}
	if observations != 1 || status != "admitted" || next != nil {
		t.Fatal("retry success duplicated observation or retained retry")
	}
}
func TestDiscoveryTickSurvivesRestartAndNudge(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for range 20 {
		if err := s.Nudge(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var future int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1 AND available_at>now()`, TickJobKind).Scan(&future); err != nil {
		t.Fatal(err)
	}
	if future != 1 {
		t.Fatalf("future ticks=%d", future)
	}
	var id uuid.UUID
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE kind=$1 AND available_at<=now()`, TickJobKind).Scan(&id); err != nil {
		t.Fatal(err)
	}
	l := leaseDiscoveryID(t, s, id)
	if err := s.handleTick(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.handleTick(ctx, l); err != nil {
		t.Fatal(err)
	}
	var scans int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1`, IncrementalJobKind).Scan(&scans); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatalf("incremental scans=%d", scans)
	}
}

func TestDiscoveryBackfillRechecksUnchangedPolicyRevision(t *testing.T) {
	s, m := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	app := DiscoveredApp{AppID: 42, LastModified: now}
	m.details.ReleaseDate.Raw = now.AddDate(0, 0, 100).Format("2 Jan, 2006")
	old := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Complete(ctx, old); err != nil {
		t.Fatal(err)
	}
	later := now.AddDate(0, 0, 11)
	s.now = func() time.Time { return later }
	lease, p := scanLease(t, s, DiscoveryBackfill, later, 0)
	manifest, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{Apps: []DiscoveredApp{app}, NextAppID: 42}, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Items[0].RetryGeneration != 1 || manifest.Items[0].JobID == old.ID {
		t.Fatal("policy reconsideration reused historical succeeded job")
	}
	child := leaseDiscoveryID(t, s, manifest.Items[0].JobID)
	if err = s.handleQualification(ctx, child); err != nil {
		t.Fatal(err)
	}
	var count, generation int
	var status string
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) OVER(),retry_generation,status FROM catalog_discovery_observations`).Scan(&count, &generation, &status); err != nil {
		t.Fatal(err)
	}
	if count != 1 || generation != 1 || status != "admitted" {
		t.Fatalf("policy revision=%d %d %s", count, generation, status)
	}
}
func TestDiscoveryBackfillExpiresUnknownRevision(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	app := DiscoveredApp{AppID: 42, LastModified: now}
	old := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Complete(ctx, old); err != nil {
		t.Fatal(err)
	}
	later := now.AddDate(0, 0, 181)
	s.now = func() time.Time { return later }
	lease, p := scanLease(t, s, DiscoveryBackfill, later, 0)
	manifest, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{Apps: []DiscoveredApp{app}, NextAppID: 42}, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Items[0].JobID == uuid.Nil {
		t.Fatal("stale unknown admission was never reconsidered")
	}
	child := leaseDiscoveryID(t, s, manifest.Items[0].JobID)
	if err = s.handleQualification(ctx, child); err != nil {
		t.Fatal(err)
	}
	var active bool
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT active FROM catalog_entry_sources WHERE source='steam_discovery'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("stale unknown stayed active")
	}
}
func TestDiscoveryCheckpointFailureRollsBack(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	lease, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	m, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{}, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.repo.store.Pool.Exec(ctx, `CREATE FUNCTION reject_discovery_checkpoint() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''injected checkpoint failure''; END'; CREATE TRIGGER reject_discovery_checkpoint BEFORE UPDATE ON catalog_discovery_state FOR EACH ROW EXECUTE FUNCTION reject_discovery_checkpoint()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.repo.ReconcilePage(ctx, lease, m, now); err == nil {
		t.Fatal("checkpoint failure ignored")
	}
	state, _ := s.repo.State(ctx, DiscoveryIncremental)
	if state.ScanCompletedAt != nil || state.WatermarkAt.Equal(now) {
		t.Fatal("failed checkpoint committed")
	}
}
func TestIncrementalBudgetPrecedesBackfill(t *testing.T) {
	s, _ := discoveryFixture(t)
	s.cfg.HourlyBudget = 2
	s.cfg.PageSize = 2
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	lease, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	_, err := s.repo.SchedulePage(ctx, lease, p, DiscoveryPage{Apps: []DiscoveredApp{{AppID: 42, LastModified: now}, {AppID: 43, LastModified: now}}, NextAppID: 43, HasMore: true}, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err := s.repo.scheduleScan(ctx, DiscoveryBackfill, now, s.cfg)
	if err != nil || scheduled {
		t.Fatal("backfill consumed incremental budget")
	}
	var used int
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT used FROM catalog_discovery_budget`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 2 {
		t.Fatal("budget exceeded")
	}
}

func TestDiscoveryDuplicatePageWaitsForForeignParentItem(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	first, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	page := DiscoveryPage{Apps: []DiscoveredApp{{AppID: 42, LastModified: now}}, NextAppID: 42}
	m, err := s.repo.SchedulePage(ctx, first, p, page, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, p2 := scanLease(t, s, DiscoveryIncremental, now, 1)
	m2, err := s.repo.SchedulePage(ctx, second, p2, page, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m.Items[0].JobID != m2.Items[0].JobID {
		t.Fatal("overlap duplicated revision identity")
	}
	var parent uuid.UUID
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT parent_job_id FROM sync_jobs WHERE id=$1`, m.Items[0].JobID).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != first.ID {
		t.Fatal("duplicate enqueue reassigned parent")
	}
	ready, _, err := s.repo.ReconcilePage(ctx, second, m2, now)
	if err != nil || ready {
		t.Fatalf("foreign-parent item skipped: %v %v", ready, err)
	}
}
func TestDiscoveryPageResumesInNextBudgetHour(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	first, p := scanLease(t, s, DiscoveryIncremental, now, 0)
	if _, err := s.repo.store.Pool.Exec(ctx, `INSERT INTO catalog_discovery_budget(hour_at,used) VALUES($1,200)`, p.BudgetHour); err != nil {
		t.Fatal(err)
	}
	if err := s.handleScan(ctx, first); !errors.Is(err, jobs.ErrDeferred) {
		t.Fatalf("budget pause=%v", err)
	}
	later := now.Add(time.Hour)
	s.now = func() time.Time { return later }
	s.source = &discoverySourceStub{pages: []DiscoveryPage{{Apps: []DiscoveredApp{{AppID: 42, LastModified: now}}, NextAppID: 42}}}
	first = leaseDiscoveryID(t, s, first.ID)
	if err := s.handleScan(ctx, first); !errors.Is(err, jobs.ErrDeferred) {
		t.Fatalf("resumed page=%v", err)
	}
	state, _ := s.repo.State(ctx, DiscoveryIncremental)
	if !state.ScanStartedAt.Equal(now) {
		t.Fatal("budget pause restarted the scan")
	}
	var used int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT used FROM catalog_discovery_budget WHERE hour_at=$1`, later.Truncate(time.Hour)).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Fatal("resumed page charged the exhausted hour")
	}
}
func TestDiscoveryOverlapCannotSkipLowAppID(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	source := &discoverySourceStub{pages: []DiscoveryPage{{Apps: []DiscoveredApp{{AppID: 100, LastModified: start}}, NextAppID: 100, HasMore: true}, {Apps: []DiscoveredApp{{AppID: 200, LastModified: start}}, NextAppID: 200}, {Apps: []DiscoveredApp{{AppID: 50, LastModified: start.Add(time.Minute)}}, NextAppID: 50}}}
	s.source = source
	for index, appID := range []int64{100, 200, 50} {
		when := start
		if index == 2 {
			when = start.Add(time.Hour)
		}
		s.now = func() time.Time { return when }
		metadata.details.AppID = appID
		lease, p := scanLease(t, s, DiscoveryIncremental, when, 0)
		if err := s.handleScan(ctx, lease); !errors.Is(err, jobs.ErrDeferred) {
			t.Fatalf("scan start=%v", err)
		}
		var raw []byte
		if err := s.repo.store.Pool.QueryRow(ctx, `SELECT result FROM sync_jobs WHERE id=$1`, lease.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var manifest PageManifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		child := leaseDiscoveryID(t, s, manifest.Items[0].JobID)
		if err := s.handleQualification(ctx, child); err != nil {
			t.Fatal(err)
		}
		if err := s.queue.Complete(ctx, child); err != nil {
			t.Fatal(err)
		}
		lease = leaseDiscoveryID(t, s, lease.ID)
		ready, _, err := s.repo.ReconcilePage(ctx, lease, manifest, when.Add(time.Minute))
		if err != nil || !ready {
			t.Fatalf("reconcile=%v %v", ready, err)
		}
		if err = s.queue.Complete(ctx, lease); err != nil {
			t.Fatal(err)
		}
		if p.ScanStartedAt.IsZero() {
			t.Fatal("lost scan start")
		}
	}
	if len(source.since) != 3 || !source.since[0].Equal(source.since[1]) || !source.since[2].Equal(start.Add(-5*time.Minute)) {
		t.Fatal("page lower bound changed or next scan lost overlap")
	}
	var n int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entry_sources WHERE active`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatal("modified low app lost")
	}
}
