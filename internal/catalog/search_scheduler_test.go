package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

func searchSchedulerFixture(t *testing.T) (*SearchScheduler, *store.Store) {
	t.Helper()
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	s, err := NewSearchScheduler(NewSearchRepository(db), jobs.NewQueue(db), cfg, func() time.Time { return searchClock() })
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func searchClock() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

func raceSchedulers(t *testing.T, schedulers ...*SearchScheduler) {
	t.Helper()
	var wg sync.WaitGroup
	for _, s := range schedulers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.EnsureScheduled(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestTwoSchedulersCreateOneOccurrence(t *testing.T) {
	s, db := searchSchedulerFixture(t)
	other, err := NewSearchScheduler(NewSearchRepository(db), jobs.NewQueue(db), s.cfg, s.now)
	if err != nil {
		t.Fatal(err)
	}
	raceSchedulers(t, s, other)
	var runs, roots int
	if err := db.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM catalog_search_runs WHERE schedule_name='daily'),(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.run' AND lane='catalog' AND max_attempts=4)`).Scan(&runs, &roots); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || roots != 3 {
		t.Fatalf("daily runs=%d catalog roots=%d", runs, roots)
	}
	var occurrence time.Time
	if err := db.Pool.QueryRow(context.Background(), `SELECT occurrence_at FROM catalog_search_runs WHERE schedule_name='daily'`).Scan(&occurrence); err != nil {
		t.Fatal(err)
	}
	if !occurrence.Equal(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Moscow occurrence=%s", occurrence)
	}
	var payload SearchRunPayload
	var encoded []byte
	if err := db.Pool.QueryRow(context.Background(), `SELECT payload FROM sync_jobs WHERE idempotency_key=$1`, "catalog.search.run:v1:daily:1792281600:retry:0").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil || payload.Version != 1 || payload.RunRevision < 1 {
		t.Fatal("invalid run payload", err)
	}
}

func TestStartupCreatesOneCatchupAndInitialRun(t *testing.T) {
	s, db := searchSchedulerFixture(t)
	ctx := context.Background()
	// Seed/wishlist provenance must not suppress initial Search backfill.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES(gen_random_uuid(),42,'Seed','seed',now())`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return searchClock().AddDate(0, 0, 10) }
	if err := s.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	var daily, initial int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE schedule_name='daily'),count(*) FILTER(WHERE schedule_name='initial') FROM catalog_search_runs`).Scan(&daily, &initial); err != nil {
		t.Fatal(err)
	}
	if daily != 2 || initial != 1 {
		t.Fatalf("daily=%d initial=%d", daily, initial)
	}
}

func TestDisabledDiscoveryStartupRecoversUserSyncIngress(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := searchClock()
	repo := NewSearchRepository(db)
	queue := jobs.NewQueue(db)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = false
	if _, _, err := repo.ensureUserSyncCandidate(ctx, MetadataIngress{Kind: "user_sync", Identity: "sync:restart"},
		RefreshTarget{AppID: 730, Priority: 100}, now); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	var ingressStatus, reservationStatus string
	var qualifications int
	if err := db.Pool.QueryRow(ctx, `SELECT w.status,r.status,
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify' AND lane='catalog')
		FROM catalog_metadata_ingress_work w
		JOIN catalog_store_reservations r USING(steam_app_id)
		WHERE w.steam_app_id=730`).Scan(&ingressStatus, &reservationStatus, &qualifications); err != nil {
		t.Fatal(err)
	}
	if ingressStatus != "reserved" || reservationStatus != "reserved" || qualifications != 1 {
		t.Fatalf("ingress=%q reservation=%q qualifications=%d, want reserved/reserved/1", ingressStatus, reservationStatus, qualifications)
	}
}

func TestDisabledDiscoveryRunRecoversRebuildAfterEarlySyncNudge(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := searchClock()
	userID, gameID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,source_snapshot,last_successful_fetch_at)
		VALUES($1,987654323,'Wishlist placeholder','steam_wishlist','{}',$2)`, gameID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at)
		VALUES($1,$2,'steam_wishlist',$3)`, userID, gameID, now); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = false
	repo := NewSearchRepository(db)
	queue := jobs.NewQueue(db)
	if count, err := repo.EnqueueUserSyncCandidates(ctx, queue, cfg, "sync:early-nudge",
		[]RefreshTarget{{AppID: 987654323, Priority: 100}}, now); err != nil || count != 1 {
		t.Fatalf("enqueued=%d err=%v", count, err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// This is the production ordering that exposed the lost wakeup: sync/root
	// completion nudges before the catalog worker commits qualification.
	if err := scheduler.Nudge(ctx); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	scheduler.recoveryInterval = 5 * time.Millisecond
	startupComplete := make(chan struct{})
	clockCalls := 0
	scheduler.now = func() time.Time {
		clockCalls++
		if clockCalls == 2 {
			close(startupComplete)
		}
		return now
	}
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(runCtx) }()
	<-startupComplete

	lease, err := queue.Claim(ctx, jobs.LaneCatalog, "disabled-recovery", time.Minute)
	if err != nil || lease == nil || lease.Kind != SearchQualificationJobKind {
		t.Fatalf("claim=%+v err=%v", lease, err)
	}
	metadata := &discoveryMetadata{details: steam.GameDetails{AppID: 987654323, Type: "game", Name: "Wishlist release",
		ShortDescription: "Wishlist metadata", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"},
		Platforms: steam.Platforms{Windows: true}}}
	service, err := NewDiscoveryService(NewDiscoveryRepository(db), metadata, queue, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	if err := service.handleSearchQualification(ctx, *lease); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var rebuilds int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs
			WHERE user_id=$1 AND kind='recommendations.rebuild' AND status='queued'`, userID).Scan(&rebuilds); err != nil {
			t.Fatal(err)
		}
		if rebuilds == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disabled scheduler did not recover the post-qualification rebuild")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDeadOccurrenceCreatesOneFencedRetryGeneration(t *testing.T) {
	s, db := searchSchedulerFixture(t)
	ctx := context.Background()
	run, err := s.EnqueueManual(ctx, searchClock())
	if err != nil {
		t.Fatal(err)
	}
	key := searchRunKey("manual", run.OccurredAt, 0)
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',updated_at=$1 WHERE idempotency_key=$2`, searchClock(), key); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	var next time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT next_retry_at FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if !next.Equal(searchClock().Add(time.Hour)) {
		t.Fatalf("retry due=%s", next)
	}
	s.now = func() time.Time { return searchClock().Add(time.Hour) }
	raceSchedulers(t, s, s)
	var revision int64
	var generation, count int
	if err := db.Pool.QueryRow(ctx, `SELECT run_revision,retry_generation,(SELECT count(*) FROM sync_jobs WHERE idempotency_key LIKE 'catalog.search.run:v1:manual:%') FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&revision, &generation, &count); err != nil {
		t.Fatal(err)
	}
	if revision != run.Revision || generation != 1 || count != 2 {
		t.Fatalf("revision=%d generation=%d jobs=%d", revision, generation, count)
	}
	var status string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE idempotency_key=$1`, key).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Fatal("historical dead row mutated")
	}
}

func TestWatchdogResumesTerminalSearchPageWithoutRefetchingCommittedPage(t *testing.T) {
	ctx := context.Background()
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	queue := jobs.NewQueue(db)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	now := searchClock()
	source := &resumableSearchStub{requests: map[int]int{}, failStart: 1, failRemaining: 4, pages: map[int]steam.SearchPage{
		0: {Start: 0, Count: 1, Total: 2, HasMore: true, Items: []steam.SearchObservation{testObservation(71, "18 Oct, 2026", steam.ReviewPositive, "Positive")}},
		1: {Start: 1, Count: 1, Total: 2, HasMore: false, Items: []steam.SearchObservation{testObservation(72, "19 Oct, 2026", steam.ReviewPositive, "Positive")}},
	}}
	service, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run := searchTestRun(t, repo, cfg, now.Add(-time.Minute))
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status='succeeded' WHERE kind='catalog.search.run' AND payload->>'run_id'=$1`, run.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessPage(ctx, SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming}); err != nil {
		t.Fatal(err)
	}
	pagePayload := SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming, Start: 1}
	pageID, err := queue.Enqueue(ctx, searchPageSpec(pagePayload))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 4; attempt++ {
		lease, claimErr := queue.Claim(ctx, jobs.LaneCatalog, "resume-test", time.Minute)
		if claimErr != nil || lease == nil || lease.ID != pageID {
			t.Fatalf("attempt %d lease=%v err=%v", attempt, lease, claimErr)
		}
		handlerErr := service.handlePage(ctx, *lease)
		if attempt < 4 {
			if _, failErr := queue.Fail(ctx, *lease, handlerErr, now.Add(-time.Second)); failErr != nil {
				t.Fatal(failErr)
			}
		} else if !errors.Is(handlerErr, jobs.ErrDeferred) {
			t.Fatalf("terminal error=%v", handlerErr)
		}
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET updated_at=$2 WHERE id=$1`, pageID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_feeds SET status='complete',natural_end=true,completed_at=$2
		WHERE run_id=$1 AND feed<>'popular_upcoming'`, run.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Queries.FinishSearchRunIfTerminal(ctx, dbgen.FinishSearchRunIfTerminalParams{RunID: catalogUUID(run.ID), CompletedAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := scheduler.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	var generation int
	if err := db.Pool.QueryRow(ctx, `SELECT retry_generation FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessRun(ctx, SearchRunPayload{Version: 1, RunID: run.ID, RunRevision: run.Revision, Generation: generation}); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessPage(ctx, SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming, Start: 1, PageGeneration: generation}); err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, service, repo)
	if _, err := repo.EnqueueReadySearchQualifications(ctx, queue, cfg, now); err != nil {
		t.Fatal(err)
	}
	// A delayed generation-0 replay is an idempotent no-op: it neither calls
	// Steam nor creates another Store/AI qualification chain.
	if err := service.ProcessPage(ctx, SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueReadySearchQualifications(ctx, queue, cfg, now); err != nil {
		t.Fatal(err)
	}
	var committed, duplicate, observations, work, qualificationJobs, qualificationApps, reservations, aiJobs int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE start_offset=0 AND page_generation>0),
		(SELECT count(*) FROM catalog_search_observations WHERE run_id=$1 AND feed='popular_upcoming'),
		(SELECT count(*) FROM catalog_search_worklist WHERE run_id=$1),
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT count(DISTINCT payload->>'app_id') FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT count(*) FROM catalog_store_reservations),
		(SELECT count(*) FROM sync_jobs WHERE kind IN ('ai.features','ai.embedding'))
		FROM catalog_search_pages WHERE run_id=$1 AND feed='popular_upcoming'`, run.ID).
		Scan(&committed, &duplicate, &observations, &work, &qualificationJobs, &qualificationApps, &reservations, &aiJobs); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || source.requests[0] != 1 || source.requests[1] != 5 || committed != 2 || duplicate != 0 || observations != 2 || work != 2 ||
		qualificationJobs != 2 || qualificationApps != 2 || reservations != 2 || aiJobs != 0 {
		t.Fatalf("generation=%d requests=%v committed=%d duplicate=%d observations=%d work=%d qualification_jobs=%d apps=%d reservations=%d ai=%d",
			generation, source.requests, committed, duplicate, observations, work, qualificationJobs, qualificationApps, reservations, aiJobs)
	}
}

func TestSearchSafetyCapIsTerminalForLogicalRun(t *testing.T) {
	ctx := context.Background()
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	queue := jobs.NewQueue(db)
	cfg := DefaultDiscoveryConfig()
	cfg.SearchMaxPagesPerFeed = 1
	now := searchClock()
	source := &resumableSearchStub{requests: map[int]int{}, failStart: -1, pages: map[int]steam.SearchPage{
		0: {Start: 0, Count: 1, Total: 2, HasMore: true, Items: []steam.SearchObservation{testObservation(81, "18 Oct, 2026", steam.ReviewPositive, "Positive")}},
		1: {Start: 1, Count: 1, Total: 2, HasMore: false, Items: []steam.SearchObservation{testObservation(82, "19 Oct, 2026", steam.ReviewPositive, "Positive")}},
	}}
	service, err := NewSearchScanService(repo, source, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	run := searchTestRun(t, repo, cfg, now.Add(-time.Minute))
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status='succeeded' WHERE kind='catalog.search.run' AND payload->>'run_id'=$1`, run.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessPage(ctx, SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_feeds SET status='complete',natural_end=true,completed_at=$2
		WHERE run_id=$1 AND feed<>'popular_upcoming'`, run.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Queries.FinishSearchRunIfTerminal(ctx, dbgen.FinishSearchRunIfTerminalParams{RunID: catalogUUID(run.ID), CompletedAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	// Neither a dead root nor a dead replay of the already committed page may
	// reinterpret a bounded partial result as a retryable failure.
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',updated_at=$2
		WHERE kind='catalog.search.run' AND payload->>'run_id'=$1`, run.ID.String(), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	committedReplay, err := queue.Enqueue(ctx, searchPageSpec(SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',updated_at=$2 WHERE id=$1`, committedReplay, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewSearchScheduler(repo, queue, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := scheduler.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var generation, cursor, pages, committed, observations, work, retryRoots int
	if err := db.Pool.QueryRow(ctx, `SELECT status,retry_generation,
		(SELECT cursor_offset FROM catalog_search_feeds WHERE run_id=$1 AND feed='popular_upcoming'),
		(SELECT pages FROM catalog_search_feeds WHERE run_id=$1 AND feed='popular_upcoming'),
		(SELECT count(*) FROM catalog_search_pages WHERE run_id=$1 AND feed='popular_upcoming'),
		(SELECT count(*) FROM catalog_search_observations WHERE run_id=$1 AND feed='popular_upcoming'),
		(SELECT count(*) FROM catalog_search_worklist WHERE run_id=$1),
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.run' AND payload->>'run_id'=$1::text AND payload->>'generation'='1')
		FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&status, &generation, &cursor, &pages, &committed, &observations, &work, &retryRoots); err != nil {
		t.Fatal(err)
	}
	if status != "partial" || generation != 0 || cursor != 1 || pages != 1 || committed != 1 || observations != 1 || work != 1 || retryRoots != 0 || source.requests[0] != 1 || source.requests[1] != 0 {
		t.Fatalf("status=%s generation=%d cursor=%d pages=%d committed=%d observations=%d work=%d retries=%d requests=%v",
			status, generation, cursor, pages, committed, observations, work, retryRoots, source.requests)
	}
}

func TestWeeklyReconciliationUsesSameOccurrenceRules(t *testing.T) {
	s, db := searchSchedulerFixture(t)
	raceSchedulers(t, s, s)
	var count int
	var occurrence time.Time
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*),min(occurrence_at) FROM catalog_search_runs WHERE schedule_name='reconciliation'`).Scan(&count, &occurrence); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !occurrence.Equal(time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("count=%d occurrence=%s", count, occurrence)
	}
	ref, err := s.EnqueueReconciliation(context.Background(), searchClock())
	if err != nil || !ref.OccurredAt.Equal(occurrence) || ref.Kind != RunReconciliation {
		t.Fatalf("reconciliation identity=%+v err=%v", ref, err)
	}
}

func TestHarnessReconciliationDoesNotReuseWeeklyOccurrence(t *testing.T) {
	s, db := searchSchedulerFixture(t)
	ctx := context.Background()
	weekly, err := s.EnqueueReconciliation(ctx, searchClock())
	if err != nil {
		t.Fatal(err)
	}
	manualAt := searchClock().Add(37 * time.Second)
	fresh, err := s.EnqueueTestReconciliation(ctx, manualAt)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == weekly.ID || fresh.OccurredAt.Equal(weekly.OccurredAt) || !fresh.OccurredAt.Equal(manualAt.Truncate(time.Second)) {
		t.Fatalf("weekly=%+v harness=%+v", weekly, fresh)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_search_runs WHERE schedule_name='reconciliation'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reconciliation runs=%d, want 2", count)
	}
}

func TestSearchInitialDependsOnDiscoveryState(t *testing.T) {
	for _, state := range []string{"membership", "queued", "running", "complete", "failed"} {
		t.Run(state, func(t *testing.T) {
			s, db := searchSchedulerFixture(t)
			ctx := context.Background()
			if state == "membership" {
				if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_memberships(feed,steam_app_id,published_run_revision) VALUES('popular_upcoming',42,1)`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status) VALUES('daily',$1,'scheduled',$2)`, searchClock().AddDate(0, 0, -1), state); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.EnsureScheduled(ctx); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_search_runs WHERE kind='initial'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if state == "failed" {
				want = 1
			}
			if count != want {
				t.Fatalf("initial=%d want=%d", count, want)
			}
		})
	}
}

func TestSearchInitialAccountsForReconciliationRuns(t *testing.T) {
	for _, state := range []string{"queued", "running", "complete", "partial_with_success", "failed"} {
		t.Run(state, func(t *testing.T) {
			s, db := searchSchedulerFixture(t)
			ctx := context.Background()
			status := state
			var succeededAt *time.Time
			if state == "partial_with_success" {
				status = "partial"
				at := searchClock().Add(-time.Hour)
				succeededAt = &at
			}
			if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status,last_successful_at)
				VALUES('reconciliation',$1,'reconciliation',$2,$3)`, searchClock().AddDate(0, 0, -7), status, succeededAt); err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureScheduled(ctx); err != nil {
				t.Fatal(err)
			}
			var initial int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_search_runs WHERE kind='initial'`).Scan(&initial); err != nil {
				t.Fatal(err)
			}
			want := 0
			if state == "failed" {
				want = 1
			}
			if initial != want {
				t.Fatalf("initial=%d want=%d", initial, want)
			}
		})
	}
}

func TestSearchManualIdentityAndSnapshotSurviveReplay(t *testing.T) {
	s, db := searchSchedulerFixture(t)
	ctx := context.Background()
	first, err := s.EnqueueManual(ctx, searchClock().Add(123*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot []byte
	if err := db.Pool.QueryRow(ctx, `SELECT config_snapshot FROM catalog_search_runs WHERE id=$1`, first.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	s.cfg.LookbackDays = 45
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',priority=1 WHERE idempotency_key=$1`, searchRunKey("manual", first.OccurredAt, 0)); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.EnqueueManual(ctx, searchClock().Add(999*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatalf("replay=%+v first=%+v", replayed, first)
	}
	var priority int
	var unchanged bool
	if err := db.Pool.QueryRow(ctx, `SELECT priority,(SELECT config_snapshot=$2::jsonb FROM catalog_search_runs WHERE id=$3) FROM sync_jobs WHERE idempotency_key=$1`, searchRunKey("manual", first.OccurredAt, 0), snapshot, first.ID).Scan(&priority, &unchanged); err != nil {
		t.Fatal(err)
	}
	if priority != 1 || !unchanged {
		t.Fatalf("dead priority=%d snapshot unchanged=%v", priority, unchanged)
	}
	later, err := s.EnqueueManual(ctx, searchClock().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if later.Revision <= first.Revision || later.ID == first.ID {
		t.Fatal("new occurrence did not allocate a newer revision")
	}
}

func TestSearchOccurrenceWatchdogBackoffAndDisabled(t *testing.T) {
	for generation, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour} {
		if got := occurrenceRetryDelay(generation); got != want {
			t.Fatalf("generation=%d delay=%s", generation, got)
		}
	}
	s, db := searchSchedulerFixture(t)
	s.cfg.Enabled = false
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,
		release_text,precision,release_date,coming_soon,review_band,review_label,positive_percent,review_total,active,next_eligibility_at)
		VALUES(991202,1,1,'17 Sep, 2026','day','2026-09-17',false,'positive','Positive',90,100,true,$1)`, searchClock()); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM catalog_search_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("disabled scheduler enqueued runs")
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='recommendations.rebuild' AND lane='interactive'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("disabled scheduler rebuild jobs=%d", count)
	}
}

func TestDeadOccurrencePreservesPersistedRetryDueTime(t *testing.T) {
	s, db := searchSchedulerFixture(t)
	ctx := context.Background()
	run, err := s.EnqueueManual(ctx, searchClock())
	if err != nil {
		t.Fatal(err)
	}
	due := searchClock().Add(2 * time.Hour)
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_runs SET status='failed',next_retry_at=$2 WHERE id=$1`, run.ID, due); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',updated_at=$2 WHERE idempotency_key=$1`, searchRunKey("manual", run.OccurredAt, 0), searchClock()); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return due }
	raceSchedulers(t, s, s)
	var generation int
	if err := db.Pool.QueryRow(ctx, `SELECT retry_generation FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("generation=%d", generation)
	}
}

func TestSearchLatestOccurrenceUsesDSTIdentity(t *testing.T) {
	for _, tc := range []struct{ expression, now, want string }{
		{"30 2 * * *", "2026-03-08T12:00:00Z", "2026-03-07T07:30:00Z"},
		{"30 2 * * *", "2026-03-09T12:00:00Z", "2026-03-09T06:30:00Z"},
		{"30 1 * * *", "2026-11-01T06:00:00Z", "2026-11-01T05:30:00Z"},
		{"30 1 * * *", "2026-11-01T07:00:00Z", "2026-11-01T06:30:00Z"},
	} {
		_, schedule, err := ParseSchedule(tc.expression, "America/New_York")
		if err != nil {
			t.Fatal(err)
		}
		now, _ := time.Parse(time.RFC3339, tc.now)
		want, _ := time.Parse(time.RFC3339, tc.want)
		got, err := latestOccurrence(schedule, now)
		if err != nil || !got.Equal(want) {
			t.Fatalf("at %s got=%s want=%s err=%v", now, got, want, err)
		}
	}
}
