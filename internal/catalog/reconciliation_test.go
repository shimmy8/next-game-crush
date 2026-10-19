package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
)

type reconciliationSourceStub struct {
	pages    []steam.DiscoveryPage
	errors   []error
	calls    int
	after    func()
	since    []time.Time
	afterIDs []uint32
}

func (*reconciliationSourceStub) Search(context.Context, steam.SearchRequest) (steam.SearchPage, error) {
	return steam.SearchPage{}, nil
}
func (s *reconciliationSourceStub) ListChangedGames(_ context.Context, since time.Time, after uint32, _ int) (steam.DiscoveryPage, error) {
	s.calls++
	s.since = append(s.since, since)
	s.afterIDs = append(s.afterIDs, after)
	if s.after != nil {
		s.after()
	}
	if len(s.errors) > 0 {
		err := s.errors[0]
		s.errors = s.errors[1:]
		if err != nil {
			if len(s.pages) > 0 {
				s.pages = s.pages[1:]
			}
			return steam.DiscoveryPage{}, err
		}
	}
	p := s.pages[0]
	s.pages = s.pages[1:]
	return p, nil
}

func TestReconciliationStoredPageReplayKeepsTuplesStable(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{{
		Apps: []steam.DiscoveredApp{{AppID: 620, LastModified: now}}, NextAppID: 620, HasMore: true,
	}}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueTestReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	runLease, err := queue.Claim(ctx, jobs.LaneCatalog, "replay-run", time.Minute)
	if err != nil || runLease == nil {
		t.Fatalf("claim run=%+v err=%v", runLease, err)
	}
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	pageLease, err := queue.Claim(ctx, jobs.LaneCatalog, "replay-page", time.Minute)
	if err != nil || pageLease == nil {
		t.Fatalf("claim page=%+v err=%v", pageLease, err)
	}
	if err = scan.handleReconciliationPage(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	if err = scan.handleReconciliationPage(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	var ordinal, headRevision, headOrdinal, workRevision, workOrdinal int64
	var successors int
	if err = db.Pool.QueryRow(ctx, `SELECT r.observation_ordinal,h.material_run_revision,h.material_observation_ordinal,
		w.material_run_revision,w.material_observation_ordinal,
		(SELECT count(*) FROM sync_jobs WHERE kind=$2 AND payload->>'run_id'=$3 AND payload->>'after_app_id'='620')
		FROM catalog_search_runs r JOIN catalog_metadata_heads h ON h.steam_app_id=620
		JOIN catalog_metadata_ingress_work w ON w.steam_app_id=620 AND w.ingress_kind='reconciliation'
		WHERE r.id=$1`, run.ID, ReconciliationPageJobKind, run.ID.String()).
		Scan(&ordinal, &headRevision, &headOrdinal, &workRevision, &workOrdinal, &successors); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 || ordinal != 1 || successors != 1 || headRevision != run.Revision || headOrdinal != 1 || workRevision != run.Revision || workOrdinal != 1 {
		t.Fatalf("calls=%d ordinal=%d successors=%d head=%d/%d work=%d/%d", source.calls, ordinal, successors,
			headRevision, headOrdinal, workRevision, workOrdinal)
	}
}

func TestWeeklyReconciliationUsesDurableCatalogLaneAndIngressGate(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status,completed_at,last_successful_at)
		VALUES('daily',$1,'scheduled','complete',$1,$1)`, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,
		material_run_revision,material_observation_ordinal,material_fingerprint,last_successful_appdetails_at,next_refresh_at,active)
		VALUES(620,1,1,1,1,'old',$1,$2,true)`, now.Add(-time.Hour), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{{Apps: []steam.DiscoveredApp{{AppID: 620, LastModified: now.Add(-time.Hour)}}, NextAppID: 620}}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scan.SetSearchQualificationProducer(repo)
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	runLease, err := queue.Claim(ctx, jobs.LaneCatalog, "reconciliation-test", time.Minute)
	if err != nil || runLease == nil || runLease.Kind != SearchRunJobKind {
		t.Fatalf("run lease=%+v err=%v", runLease, err)
	}
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	pageLease, err := queue.Claim(ctx, jobs.LaneCatalog, "reconciliation-test", time.Minute)
	if err != nil || pageLease == nil || pageLease.Kind != ReconciliationPageJobKind || pageLease.Priority != 10 {
		t.Fatalf("page lease=%+v err=%v", pageLease, err)
	}
	if err = scan.handleReconciliationPage(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	if err = scan.handleReconciliationPage(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	var status, fingerprint string
	var ingress, qualifications int
	var materialRevision int64
	var nextRefresh time.Time
	if err = db.Pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM catalog_metadata_ingress_work WHERE ingress_kind='reconciliation'),
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT material_run_revision FROM catalog_search_current WHERE steam_app_id=620),
		(SELECT material_fingerprint FROM catalog_search_current WHERE steam_app_id=620),
		(SELECT next_refresh_at FROM catalog_search_current WHERE steam_app_id=620)
		FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&status, &ingress, &qualifications, &materialRevision, &fingerprint, &nextRefresh); err != nil {
		t.Fatal(err)
	}
	if status != "complete" || ingress != 1 || qualifications != 1 || source.calls != 1 ||
		materialRevision != run.Revision || fingerprint == "old" || !nextRefresh.Equal(now) {
		t.Fatalf("status=%s ingress=%d qualifications=%d calls=%d material=%d fingerprint=%q next=%s",
			status, ingress, qualifications, source.calls, materialRevision, fingerprint, nextRefresh)
	}
	var ordinal, headRevision, headOrdinal, workRevision, workOrdinal int64
	if err = db.Pool.QueryRow(ctx, `SELECT r.observation_ordinal,h.material_run_revision,h.material_observation_ordinal,
		w.material_run_revision,w.material_observation_ordinal FROM catalog_search_runs r
		JOIN catalog_metadata_heads h ON h.steam_app_id=620
		JOIN catalog_metadata_ingress_work w ON w.steam_app_id=620 AND w.ingress_kind='reconciliation'
		WHERE r.id=$1`, run.ID).Scan(&ordinal, &headRevision, &headOrdinal, &workRevision, &workOrdinal); err != nil {
		t.Fatal(err)
	}
	if ordinal != 1 || headRevision != run.Revision || headOrdinal != 1 || workRevision != run.Revision || workOrdinal != 1 {
		t.Fatalf("replay changed tuples: ordinal=%d head=%d/%d work=%d/%d", ordinal, headRevision, headOrdinal, workRevision, workOrdinal)
	}
}

func TestReconciliationCapsPagesAndResumesPersistedContinuation(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	cfg.SearchMaxPagesPerFeed = 1
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{
		{Apps: []steam.DiscoveredApp{{AppID: 100, LastModified: now.Add(-time.Hour)}}, NextAppID: 100, HasMore: true},
		{Apps: []steam.DiscoveredApp{{AppID: 200, LastModified: now.Add(-time.Hour)}}, NextAppID: 200},
	}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scan.SetSearchQualificationProducer(repo)
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueTestReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	var status string
	if err = db.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "partial" || source.calls != 1 {
		t.Fatalf("first capped run status=%s calls=%d", status, source.calls)
	}
	continued, err := scheduler.EnqueueTestReconciliation(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	if err = db.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_runs WHERE id=$1`, continued.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" || source.calls != 2 {
		t.Fatalf("continued run status=%s calls=%d", status, source.calls)
	}
	var secondAfter int64
	if err = db.Pool.QueryRow(ctx, `SELECT (payload->>'after_app_id')::bigint FROM sync_jobs
		WHERE kind=$1 AND payload->>'run_id'=$2 ORDER BY created_at DESC LIMIT 1`, ReconciliationPageJobKind, continued.ID.String()).Scan(&secondAfter); err != nil {
		t.Fatal(err)
	}
	if secondAfter != 100 {
		t.Fatalf("continuation after=%d", secondAfter)
	}
	next, err := scheduler.EnqueueTestReconciliation(ctx, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = scan.ProcessRun(ctx, SearchRunPayload{Version: 1, RunID: next.ID, RunRevision: next.Revision}); err != nil {
		t.Fatal(err)
	}
	var nextAfter int64
	if err = db.Pool.QueryRow(ctx, `SELECT (payload->>'after_app_id')::bigint FROM sync_jobs
		WHERE kind=$1 AND payload->>'run_id'=$2`, ReconciliationPageJobKind, next.ID.String()).Scan(&nextAfter); err != nil {
		t.Fatal(err)
	}
	if nextAfter != 0 {
		t.Fatalf("completed continuation was reused: after=%d", nextAfter)
	}
}

func TestReconciliationCapsNewAppIDsWithoutSkippingRemainder(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	cfg.SearchMaxNewAppsPerRun = 1
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{
		{Apps: []steam.DiscoveredApp{{AppID: 100, LastModified: now}, {AppID: 200, LastModified: now}}, NextAppID: 200},
		{Apps: []steam.DiscoveredApp{{AppID: 200, LastModified: now}}, NextAppID: 200},
	}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueTestReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	var status string
	var count int
	if err = db.Pool.QueryRow(ctx, `SELECT status,new_app_count FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&status, &count); err != nil {
		t.Fatal(err)
	}
	if status != "partial" || count != 1 {
		t.Fatalf("first status=%s new=%d", status, count)
	}
	continued, err := scheduler.EnqueueTestReconciliation(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	if err = db.Pool.QueryRow(ctx, `SELECT status,new_app_count FROM catalog_search_runs WHERE id=$1`, continued.ID).Scan(&status, &count); err != nil {
		t.Fatal(err)
	}
	if status != "complete" || count != 1 || source.calls != 2 {
		t.Fatalf("continued status=%s new=%d calls=%d", status, count, source.calls)
	}
	var after int64
	if err = db.Pool.QueryRow(ctx, `SELECT (payload->>'after_app_id')::bigint FROM sync_jobs WHERE kind=$1
		AND payload->>'run_id'=$2 ORDER BY created_at DESC LIMIT 1`, ReconciliationPageJobKind, continued.ID.String()).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 100 {
		t.Fatalf("new-app continuation skipped remainder: after=%d", after)
	}
}

func TestReconciliationStaleGenerationCannotFetchPersistOrFinalize(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{{Apps: []steam.DiscoveredApp{{AppID: 620, LastModified: now}}}}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueTestReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	runLease, err := queue.Claim(ctx, jobs.LaneCatalog, "stale-run", time.Minute)
	if err != nil || runLease == nil {
		t.Fatalf("claim run=%+v err=%v", runLease, err)
	}
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	pageLease, err := queue.Claim(ctx, jobs.LaneCatalog, "stale-page", time.Minute)
	if err != nil || pageLease == nil {
		t.Fatalf("claim page=%+v err=%v", pageLease, err)
	}
	if _, err = db.Pool.Exec(ctx, `UPDATE catalog_search_runs SET retry_generation=retry_generation+1 WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = scan.ProcessRun(ctx, SearchRunPayload{Version: 1, RunID: run.ID, RunRevision: run.Revision, Generation: 0}); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale run err=%v", err)
	}
	if err = scan.handleReconciliationPage(ctx, *pageLease); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale page err=%v", err)
	}
	if err = scan.finishReconciliationPage(ctx, ReconciliationPagePayload{Version: 1, RunID: run.ID, RunRevision: run.Revision, PageGeneration: 0}, false); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale terminal err=%v", err)
	}
	if err = scan.reconciliationFailure(ctx, *pageLease, ReconciliationPagePayload{Version: 1, RunID: run.ID,
		RunRevision: run.Revision, PageGeneration: 0}, steam.ErrUnavailable); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale failure err=%v", err)
	}
	var ingress int
	if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_metadata_ingress_work`).Scan(&ingress); err != nil {
		t.Fatal(err)
	}
	if source.calls != 0 || ingress != 0 {
		t.Fatalf("stale generation calls=%d ingress=%d", source.calls, ingress)
	}
}

func TestReconciliationGenerationChangeDuringFetchCannotPersistOrEnqueue(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	var run RunRef
	var callbackErr error
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{{
		Apps: []steam.DiscoveredApp{{AppID: 620, LastModified: now}}, NextAppID: 620, HasMore: true,
	}}}
	source.after = func() {
		_, callbackErr = db.Pool.Exec(ctx, `UPDATE catalog_search_runs SET retry_generation=retry_generation+1 WHERE id=$1`, run.ID)
	}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run, err = scheduler.EnqueueTestReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	runLease, err := queue.Claim(ctx, jobs.LaneCatalog, "mid-fetch-run", time.Minute)
	if err != nil || runLease == nil {
		t.Fatalf("claim run=%+v err=%v", runLease, err)
	}
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	pageLease, err := queue.Claim(ctx, jobs.LaneCatalog, "mid-fetch-page", time.Minute)
	if err != nil || pageLease == nil {
		t.Fatalf("claim page=%+v err=%v", pageLease, err)
	}
	if err = scan.handleReconciliationPage(ctx, *pageLease); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("mid-fetch stale page err=%v", err)
	}
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}
	var ingress, successors int
	var result []byte
	if err = db.Pool.QueryRow(ctx, `SELECT result,
		(SELECT count(*) FROM catalog_metadata_ingress_work),
		(SELECT count(*) FROM sync_jobs WHERE kind=$2 AND payload->>'run_id'=$3 AND id<>$1)
		FROM sync_jobs WHERE id=$1`, pageLease.ID, ReconciliationPageJobKind, run.ID.String()).Scan(&result, &ingress, &successors); err != nil {
		t.Fatal(err)
	}
	if string(result) != "{}" || ingress != 0 || successors != 0 {
		t.Fatalf("stale fetch result=%s ingress=%d successors=%d", result, ingress, successors)
	}
}

func TestReconciliationIdenticalFingerprintReusesMaterialTupleAcrossRuns(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	modified := now.Add(-time.Hour)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{
		{Apps: []steam.DiscoveredApp{{AppID: 620, LastModified: modified}}, NextAppID: 620},
		{Apps: []steam.DiscoveredApp{{AppID: 620, LastModified: modified}}, NextAppID: 620},
	}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, err := scheduler.EnqueueTestReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	second, err := scheduler.EnqueueTestReconciliation(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	var currentRun, currentOrdinal, headRun, headOrdinal, workRun, workOrdinal, secondOrdinals int64
	var workStatus string
	if err = db.Pool.QueryRow(ctx, `SELECT c.material_run_revision,c.material_observation_ordinal,
		h.material_run_revision,h.material_observation_ordinal,w.material_run_revision,w.material_observation_ordinal,w.status,
		(SELECT observation_ordinal FROM catalog_search_runs WHERE id=$2)
		FROM catalog_search_current c JOIN catalog_metadata_heads h USING(steam_app_id)
		JOIN catalog_metadata_ingress_work w USING(steam_app_id)
		WHERE c.steam_app_id=620 AND w.ingress_kind='reconciliation' AND w.ingress_identity=$1`, second.ID.String()+":620", second.ID).
		Scan(&currentRun, &currentOrdinal, &headRun, &headOrdinal, &workRun, &workOrdinal, &workStatus, &secondOrdinals); err != nil {
		t.Fatal(err)
	}
	if currentRun != first.Revision || currentOrdinal != 1 || headRun != currentRun || headOrdinal != currentOrdinal ||
		workRun != currentRun || workOrdinal != currentOrdinal || workStatus == "pending" || secondOrdinals != 0 {
		t.Fatalf("current=%d/%d head=%d/%d work=%d/%d status=%s second_ordinals=%d", currentRun, currentOrdinal,
			headRun, headOrdinal, workRun, workOrdinal, workStatus, secondOrdinals)
	}
}

func TestReconciliationCompletedChainKeepsOriginalScanStartWatermark(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	chainStart := clock
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	cfg.SearchMaxPagesPerFeed = 1
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{
		{Apps: []steam.DiscoveredApp{{AppID: 100, LastModified: clock}}, NextAppID: 100, HasMore: true},
		{Apps: []steam.DiscoveredApp{{AppID: 200, LastModified: clock}}, NextAppID: 200},
	}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scheduler.EnqueueTestReconciliation(ctx, clock); err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	clock = clock.Add(7 * 24 * time.Hour)
	if _, err = scheduler.EnqueueTestReconciliation(ctx, clock); err != nil {
		t.Fatal(err)
	}
	processReconciliationRunAndPage(t, ctx, queue, scan)
	clock = clock.Add(7 * 24 * time.Hour)
	next, err := scheduler.EnqueueTestReconciliation(ctx, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err = scan.ProcessRun(ctx, SearchRunPayload{Version: 1, RunID: next.ID, RunRevision: next.Revision}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = db.Pool.QueryRow(ctx, `SELECT payload FROM sync_jobs WHERE kind=$1 AND payload->>'run_id'=$2`,
		ReconciliationPageJobKind, next.ID.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var page ReconciliationPagePayload
	if err = strictDiscoveryPayload(raw, &page); err != nil {
		t.Fatal(err)
	}
	if want := chainStart.Add(-reconciliationWatermarkOverlap); !page.Since.Equal(want) {
		t.Fatalf("next watermark=%s want original scan start overlap=%s", page.Since, want)
	}
}

func TestReconciliationDeadPageResumesCursorAndPreservesCaps(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	cfg.SearchMaxNewAppsPerRun = 2
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{
		{Apps: []steam.DiscoveredApp{{AppID: 100, LastModified: clock}}, NextAppID: 100, HasMore: true},
		{},
		{Apps: []steam.DiscoveredApp{{AppID: 200, LastModified: clock}, {AppID: 300, LastModified: clock}}, NextAppID: 300},
	}, errors: []error{nil, steam.ErrUnavailable, nil}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueTestReconciliation(ctx, clock)
	if err != nil {
		t.Fatal(err)
	}
	runLease := claimCatalogKind(t, ctx, queue, SearchRunJobKind)
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	firstPage := claimCatalogKind(t, ctx, queue, ReconciliationPageJobKind)
	if err = scan.handleReconciliationPage(ctx, *firstPage); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *firstPage); err != nil {
		t.Fatal(err)
	}
	failedPage := claimCatalogKind(t, ctx, queue, ReconciliationPageJobKind)
	if _, err = db.Pool.Exec(ctx, `UPDATE sync_jobs SET attempts=max_attempts WHERE id=$1`, failedPage.ID); err != nil {
		t.Fatal(err)
	}
	failedPage.Attempts = failedPage.MaxAttempts
	handlerErr := scan.handleReconciliationPage(ctx, *failedPage)
	if !errors.Is(handlerErr, steam.ErrUnavailable) {
		t.Fatalf("terminal handler err=%v", handlerErr)
	}
	if outcome, failErr := queue.Fail(ctx, *failedPage, handlerErr, clock); failErr != nil || outcome != jobs.FailureDead {
		t.Fatalf("page failure outcome=%s err=%v", outcome, failErr)
	}
	var rootStatus, pageStatus, runStatus string
	if err = db.Pool.QueryRow(ctx, `SELECT
		(SELECT status FROM sync_jobs WHERE id=$2),(SELECT status FROM sync_jobs WHERE id=$3),status
		FROM catalog_search_runs WHERE id=$1`, run.ID, runLease.ID, failedPage.ID).Scan(&rootStatus, &pageStatus, &runStatus); err != nil {
		t.Fatal(err)
	}
	if rootStatus != "succeeded" || pageStatus != "dead" || runStatus != "failed" {
		t.Fatalf("root=%s page=%s run=%s", rootStatus, pageStatus, runStatus)
	}
	if _, err = db.Pool.Exec(ctx, `UPDATE catalog_search_runs SET next_retry_at=$2 WHERE id=$1`, run.ID, clock.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	rearmed := claimCatalogRun(t, ctx, queue, run.ID)
	if err = scan.handleRun(ctx, *rearmed); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *rearmed); err != nil {
		t.Fatal(err)
	}
	resumed := claimCatalogRunPage(t, ctx, queue, run.ID)
	var resumedPayload ReconciliationPagePayload
	if err = strictDiscoveryPayload(resumed.Payload, &resumedPayload); err != nil {
		t.Fatal(err)
	}
	if resumedPayload.AfterAppID != 100 || resumedPayload.Page != 1 || resumedPayload.NewAppCount != 1 {
		t.Fatalf("resumed payload=%+v", resumedPayload)
	}
	if err = scan.handleReconciliationPage(ctx, *resumed); err != nil {
		t.Fatal(err)
	}
	var count, candidates int
	if err = db.Pool.QueryRow(ctx, `SELECT new_app_count,(SELECT count(*) FROM catalog_search_current WHERE steam_app_id IN (100,200,300))
		FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&count, &candidates); err != nil {
		t.Fatal(err)
	}
	if count != 2 || candidates != 2 {
		t.Fatalf("preserved cap count=%d candidates=%d", count, candidates)
	}
}

func TestReconciliationGenericDeadPageResumesCommittedSuccessorProgress(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	cfg.SearchMaxNewAppsPerRun = 2
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{
		{Apps: []steam.DiscoveredApp{{AppID: 100, LastModified: clock}}, NextAppID: 100, HasMore: true},
		{Apps: []steam.DiscoveredApp{{AppID: 200, LastModified: clock}}, NextAppID: 200},
	}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueTestReconciliation(ctx, clock)
	if err != nil {
		t.Fatal(err)
	}
	runLease := claimCatalogKind(t, ctx, queue, SearchRunJobKind)
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	page := claimCatalogKind(t, ctx, queue, ReconciliationPageJobKind)
	if _, err = db.Pool.Exec(ctx, `UPDATE sync_jobs SET attempts=max_attempts WHERE id=$1`, page.ID); err != nil {
		t.Fatal(err)
	}
	page.Attempts = page.MaxAttempts
	injected := errors.New("injected crash after successor enqueue")
	scan.afterReconciliationSuccessorHook = func() error { return injected }
	if err = scan.handleReconciliationPage(ctx, *page); !errors.Is(err, injected) {
		t.Fatalf("handler error=%v", err)
	}
	scan.afterReconciliationSuccessorHook = nil
	var progressRaw []byte
	var successors int
	if err = db.Pool.QueryRow(ctx, `SELECT result,(SELECT count(*) FROM sync_jobs successor
		WHERE successor.kind=$2 AND successor.payload->>'run_id'=$3 AND successor.id<>$1
			AND (successor.payload->>'page')::integer=1) FROM sync_jobs WHERE id=$1`,
		page.ID, ReconciliationPageJobKind, run.ID.String()).Scan(&progressRaw, &successors); err != nil {
		t.Fatal(err)
	}
	var progress reconciliationManifest
	if err = strictDiscoveryPayload(progressRaw, &progress); err != nil {
		t.Fatal(err)
	}
	if successors != 1 || progress.ResumeAfterAppID != 100 || progress.ResumePage != 1 || progress.NewAppsUsed != 1 {
		t.Fatalf("committed successor progress=%+v successors=%d", progress, successors)
	}
	if outcome, failErr := queue.Fail(ctx, *page, injected, clock); failErr != nil || outcome != jobs.FailureDead {
		t.Fatalf("failure outcome=%s err=%v", outcome, failErr)
	}
	if _, err = db.Pool.Exec(ctx, `UPDATE sync_jobs SET updated_at=$2 WHERE id=$1`, page.ID, clock.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	rearmed := claimCatalogRun(t, ctx, queue, run.ID)
	if err = scan.handleRun(ctx, *rearmed); err != nil {
		t.Fatal(err)
	}
	resumed := claimCatalogRunPage(t, ctx, queue, run.ID)
	var payload ReconciliationPagePayload
	if err = strictDiscoveryPayload(resumed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AfterAppID != 100 || payload.Page != 1 || payload.NewAppCount != 1 || !payload.ScanStartedAt.Equal(clock) {
		t.Fatalf("resumed payload=%+v", payload)
	}
	if err = scan.handleReconciliationPage(ctx, *resumed); err != nil {
		t.Fatal(err)
	}
	if len(source.afterIDs) != 2 || source.afterIDs[0] != 0 || source.afterIDs[1] != 100 {
		t.Fatalf("re-fetched processed page: cursors=%v", source.afterIDs)
	}
}

func TestReconciliationNewAppsUsedStaleGenerationReturnsLeaseLost(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	scan, err := NewSearchScanService(repo, &reconciliationSourceStub{}, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run, err := scheduler.EnqueueTestReconciliation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = scan.reconciliationNewAppsUsed(ctx, ReconciliationPagePayload{
		RunID: run.ID, RunRevision: run.Revision, PageGeneration: run.Generation + 1,
	})
	if !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale generation error=%v", err)
	}
}

func TestReconciliationTerminalReplayDrainsDurableQualificationOutbox(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	source := &reconciliationSourceStub{pages: []steam.DiscoveryPage{{
		Apps: []steam.DiscoveredApp{{AppID: 620, LastModified: now}}, NextAppID: 620,
	}}}
	scan, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scheduler.EnqueueTestReconciliation(ctx, now); err != nil {
		t.Fatal(err)
	}
	runLease := claimCatalogKind(t, ctx, queue, SearchRunJobKind)
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	pageLease := claimCatalogKind(t, ctx, queue, ReconciliationPageJobKind)
	injected := errors.New("injected crash after terminal commit")
	scan.afterReconciliationTerminalHook = func() error { return injected }
	if err = scan.handleReconciliationPage(ctx, *pageLease); !errors.Is(err, injected) {
		t.Fatalf("terminal hook err=%v", err)
	}
	scan.afterReconciliationTerminalHook = nil
	if err = scan.handleReconciliationPage(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	if err = scan.handleReconciliationPage(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	var jobsCount, pending int
	if err = db.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT count(*) FROM catalog_metadata_ingress_work WHERE status='pending')`).Scan(&jobsCount, &pending); err != nil {
		t.Fatal(err)
	}
	if jobsCount != 1 || pending != 0 {
		t.Fatalf("qualification jobs=%d pending outbox=%d", jobsCount, pending)
	}
}

func TestReconciliationWatermarkUsesPriorScanStartWithOverlap(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	started := now.Add(-7 * 24 * time.Hour)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status,started_at,completed_at,last_successful_at)
		VALUES('reconciliation',$1,'reconciliation','complete',$2,$3,$3)`, started, started.Add(time.Minute), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	queue := jobs.NewQueue(db)
	repo := NewSearchRepository(db)
	scan, err := NewSearchScanService(repo, &reconciliationSourceStub{}, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scheduler.EnqueueTestReconciliation(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(ctx, jobs.LaneCatalog, "watermark", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim=%+v err=%v", lease, err)
	}
	if err = scan.handleRun(ctx, *lease); err != nil {
		t.Fatal(err)
	}
	var payload ReconciliationPagePayload
	var raw []byte
	if err = db.Pool.QueryRow(ctx, `SELECT payload FROM sync_jobs WHERE kind=$1 ORDER BY created_at DESC LIMIT 1`, ReconciliationPageJobKind).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err = strictDiscoveryPayload(raw, &payload); err != nil {
		t.Fatal(err)
	}
	want := started.Add(time.Minute).Add(-reconciliationWatermarkOverlap)
	if !payload.Since.Equal(want) {
		t.Fatalf("watermark=%s want=%s", payload.Since, want)
	}
}

func processReconciliationRunAndPage(t *testing.T, ctx context.Context, queue *jobs.Queue, scan *SearchScanService) {
	t.Helper()
	claimKind := func(kind, worker string) *jobs.Lease {
		t.Helper()
		for {
			lease, err := queue.Claim(ctx, jobs.LaneCatalog, worker, time.Minute)
			if err != nil || lease == nil {
				t.Fatalf("claim %s=%+v err=%v", kind, lease, err)
			}
			if lease.Kind == kind {
				return lease
			}
			if err := queue.Complete(ctx, *lease); err != nil {
				t.Fatal(err)
			}
		}
	}
	runLease := claimKind(SearchRunJobKind, "cap-run")
	var err error
	if err = scan.handleRun(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *runLease); err != nil {
		t.Fatal(err)
	}
	pageLease := claimKind(ReconciliationPageJobKind, "cap-page")
	if err = scan.handleReconciliationPage(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
	if err = queue.Complete(ctx, *pageLease); err != nil {
		t.Fatal(err)
	}
}

func claimCatalogKind(t *testing.T, ctx context.Context, queue *jobs.Queue, kind string) *jobs.Lease {
	t.Helper()
	for {
		lease, err := queue.Claim(ctx, jobs.LaneCatalog, "reconciliation-focused", time.Minute)
		if err != nil || lease == nil {
			t.Fatalf("claim %s=%+v err=%v", kind, lease, err)
		}
		if lease.Kind == kind {
			return lease
		}
		if err = queue.Complete(ctx, *lease); err != nil {
			t.Fatal(err)
		}
	}
}

func claimCatalogRun(t *testing.T, ctx context.Context, queue *jobs.Queue, runID uuid.UUID) *jobs.Lease {
	t.Helper()
	for {
		lease := claimCatalogKind(t, ctx, queue, SearchRunJobKind)
		var payload SearchRunPayload
		if strictDiscoveryPayload(lease.Payload, &payload) == nil && payload.RunID == runID {
			return lease
		}
		if err := queue.Complete(ctx, *lease); err != nil {
			t.Fatal(err)
		}
	}
}

func claimCatalogRunPage(t *testing.T, ctx context.Context, queue *jobs.Queue, runID uuid.UUID) *jobs.Lease {
	t.Helper()
	for {
		lease := claimCatalogKind(t, ctx, queue, ReconciliationPageJobKind)
		var payload ReconciliationPagePayload
		if strictDiscoveryPayload(lease.Payload, &payload) == nil && payload.RunID == runID {
			return lease
		}
		if err := queue.Complete(ctx, *lease); err != nil {
			t.Fatal(err)
		}
	}
}
