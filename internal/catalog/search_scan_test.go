package catalog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
)

type searchStub struct {
	mu    sync.Mutex
	calls int
	pages map[int]steam.SearchPage
	err   error
}

type terminalSearchStub struct {
	mu       sync.Mutex
	failures int
}

func (s *terminalSearchStub) Search(_ context.Context, req steam.SearchRequest) (steam.SearchPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures < 4 {
		s.failures++
		return steam.SearchPage{}, steam.ErrInvalidResponse
	}
	return steam.SearchPage{Start: req.Start, Total: req.Start}, nil
}

func (s *searchStub) Search(_ context.Context, req steam.SearchRequest) (steam.SearchPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return steam.SearchPage{}, s.err
	}
	return s.pages[req.Start], nil
}

func searchTestRun(t *testing.T, repo *SearchRepository, cfg DiscoveryConfig, at time.Time) RunRef {
	t.Helper()
	tx, err := repo.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	run, err := repo.createRun(context.Background(), tx, cfg, "manual", RunManual, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := repo.PrepareRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func newSearchScanForTest(t *testing.T, source steam.SearchCatalog, cfg DiscoveryConfig) (*SearchScanService, *SearchRepository) {
	t.Helper()
	store := openCatalogTestStore(t)
	repo := NewSearchRepository(store)
	service, err := NewSearchScanService(repo, source, jobs.NewQueue(store), cfg, func() time.Time {
		return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

func testObservation(appID int64, release string, band steam.ReviewBand, label string) steam.SearchObservation {
	return steam.SearchObservation{AppID: appID, Name: "game", ReleaseText: release, Review: steam.ReviewSummary{Band: band, Label: label, Total: 10}, Fingerprint: release + label}
}

func TestSearchPageReplayAfterCrash(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	page := steam.SearchPage{Start: 0, Count: 1, Total: 1, HasMore: false, Items: []steam.SearchObservation{testObservation(42, "18 Oct, 2026", steam.ReviewPositive, "Positive")}}
	source := &searchStub{pages: map[int]steam.SearchPage{0: page}}
	service, repo := newSearchScanForTest(t, source, cfg)
	run := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC))
	payload := SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularNew, Start: 0, PageGeneration: 0}
	if err := service.ProcessPage(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	// This is the crash boundary: the manifest committed but queue completion did not.
	if err := service.ProcessPage(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 {
		t.Fatalf("search calls=%d, want 1", source.calls)
	}
	var pages, observations, work int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM catalog_search_pages),(SELECT count(*) FROM catalog_search_observations),(SELECT count(*) FROM catalog_search_worklist)`).Scan(&pages, &observations, &work); err != nil {
		t.Fatal(err)
	}
	if pages != 1 || observations != 1 || work != 1 {
		t.Fatalf("pages=%d observations=%d work=%d", pages, observations, work)
	}

	bad := &searchStub{pages: map[int]steam.SearchPage{0: {Start: 0, Count: 0, Total: 2, HasMore: true}}}
	badService, badRepo := newSearchScanForTest(t, bad, cfg)
	badRun := searchTestRun(t, badRepo, cfg, time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC))
	if err := badService.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: badRun.ID, Feed: steam.FeedRecent}); err == nil {
		t.Fatal("non-progressing cursor succeeded")
	}
	var cursor int
	if err := badRepo.store.Pool.QueryRow(context.Background(), `SELECT cursor_offset FROM catalog_search_feeds WHERE run_id=$1 AND feed='recent'`, badRun.ID).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != 0 {
		t.Fatalf("cursor advanced to %d", cursor)
	}

	failed := &searchStub{err: errors.New("parser failed")}
	failService, failRepo := newSearchScanForTest(t, failed, cfg)
	failRun := searchTestRun(t, failRepo, cfg, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if err := failService.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: failRun.ID, Feed: steam.FeedRecent}); err == nil {
		t.Fatal("parser failure succeeded")
	}
	if err := failRepo.store.Pool.QueryRow(context.Background(), `SELECT cursor_offset FROM catalog_search_feeds WHERE run_id=$1 AND feed='recent'`, failRun.ID).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != 0 {
		t.Fatalf("cursor advanced to %d", cursor)
	}
}

func TestOlderSearchRunCannotOverwritePublishedState(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	service, repo := newSearchScanForTest(t, &searchStub{}, cfg)
	ctx := context.Background()
	old := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC))
	newer := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC))
	if err := service.CommitPage(ctx, newer, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{testObservation(42, "18 Oct, 2026", steam.ReviewPositive, "Very Positive")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(ctx, `UPDATE catalog_search_current SET active=true WHERE steam_app_id=42`); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitPage(ctx, old, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{testObservation(42, "18 Oct, 2026", steam.ReviewMixedOrBelow, "Mixed")}}); err != nil {
		t.Fatal(err)
	}
	var revision, publishedRevision int64
	var label string
	var omissions int
	var active bool
	if err := repo.store.Pool.QueryRow(ctx, `SELECT c.run_revision,c.review_label,c.active,m.omission_count,p.published_run_revision FROM catalog_search_current c JOIN catalog_search_memberships m USING(steam_app_id) JOIN catalog_search_feed_publications p ON p.feed=m.feed WHERE c.steam_app_id=42 AND m.feed='popular_new'`).Scan(&revision, &label, &active, &omissions, &publishedRevision); err != nil {
		t.Fatal(err)
	}
	if revision != newer.Revision || publishedRevision != newer.Revision || label != "Very Positive" || !active || omissions != 0 {
		t.Fatalf("revision=%d published=%d label=%q active=%v omissions=%d", revision, publishedRevision, label, active, omissions)
	}
}

func TestPartialPopularFeedIsNeverAuthoritative(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	cfg.SearchMaxPagesPerFeed = 1
	source := &searchStub{pages: map[int]steam.SearchPage{0: {Start: 0, Count: 1, Total: 2, HasMore: true, Items: []steam.SearchObservation{testObservation(42, "18 Oct, 2026", steam.ReviewPositive, "Positive")}}}}
	service, repo := newSearchScanForTest(t, source, cfg)
	run := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC))
	if err := service.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularNew}); err != nil {
		t.Fatal(err)
	}
	var status string
	var members, work int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM catalog_search_memberships WHERE feed='popular_new'),(SELECT count(*) FROM catalog_search_worklist WHERE run_id=$1) FROM catalog_search_feeds WHERE run_id=$1 AND feed='popular_new'`, run.ID).Scan(&status, &members, &work); err != nil {
		t.Fatal(err)
	}
	if status != "partial" || members != 0 || work != 1 {
		t.Fatalf("status=%s members=%d work=%d", status, members, work)
	}
}

func TestPopularFeedsNeverBoundaryStop(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	for index, feed := range []steam.SearchFeed{steam.FeedPopularUpcoming, steam.FeedPopularNew} {
		t.Run(string(feed), func(t *testing.T) {
			source := &searchStub{pages: map[int]steam.SearchPage{
				0: {Start: 0, Count: 1, Total: 2, HasMore: true, Items: []steam.SearchObservation{testObservation(1, "1 Jan, 2020", steam.ReviewPositive, "Positive")}},
				1: {Start: 1, Count: 1, Total: 2, HasMore: false, Items: []steam.SearchObservation{testObservation(2, "1 Jan, 2019", steam.ReviewPositive, "Positive")}},
			}}
			service, repo := newSearchScanForTest(t, source, cfg)
			run := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 14+index, 0, 0, 0, time.UTC))
			if err := service.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: run.ID, Feed: feed}); err != nil {
				t.Fatal(err)
			}
			var status string
			var boundary bool
			if err := repo.store.Pool.QueryRow(context.Background(), `SELECT status,boundary_stop FROM catalog_search_feeds WHERE run_id=$1 AND feed=$2`, run.ID, feed).Scan(&status, &boundary); err != nil {
				t.Fatal(err)
			}
			if status != "running" || boundary {
				t.Fatalf("status=%s boundary=%v", status, boundary)
			}
		})
	}
}

func TestOrdinaryFeedsStopOnlyAfterMonotonicValidation(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	for _, tc := range []struct {
		name          string
		items         []steam.SearchObservation
		wantStatus    string
		wantMonotonic bool
	}{
		{"monotonic", []steam.SearchObservation{testObservation(1, "17 Sep, 2026", steam.ReviewPositive, "Positive"), testObservation(2, "16 Sep, 2026", steam.ReviewPositive, "Positive")}, "complete", true},
		{"regression", []steam.SearchObservation{testObservation(1, "16 Sep, 2026", steam.ReviewPositive, "Positive"), testObservation(2, "17 Sep, 2026", steam.ReviewPositive, "Positive")}, "running", false},
		{"missing", []steam.SearchObservation{testObservation(1, "Coming Soon", steam.ReviewPositive, "Positive")}, "running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &searchStub{pages: map[int]steam.SearchPage{0: {Start: 0, Count: len(tc.items), Total: len(tc.items) + 1, HasMore: true, Items: tc.items}}}
			service, repo := newSearchScanForTest(t, source, cfg)
			run := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 15, 0, 0, 0, time.UTC))
			if err := service.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedRecent}); err != nil {
				t.Fatal(err)
			}
			var status string
			var monotonic, boundary bool
			if err := repo.store.Pool.QueryRow(context.Background(), `SELECT status,monotonic,boundary_stop FROM catalog_search_feeds WHERE run_id=$1 AND feed='recent'`, run.ID).Scan(&status, &monotonic, &boundary); err != nil {
				t.Fatal(err)
			}
			if status != tc.wantStatus || monotonic != tc.wantMonotonic || boundary != (tc.wantStatus == "complete") {
				t.Fatalf("status=%s monotonic=%v boundary=%v", status, monotonic, boundary)
			}
		})
	}
	t.Run("regression across pages", func(t *testing.T) {
		source := &searchStub{pages: map[int]steam.SearchPage{
			0: {Start: 0, Count: 1, Total: 3, HasMore: true, Items: []steam.SearchObservation{testObservation(1, "18 Oct, 2026", steam.ReviewPositive, "Positive")}},
			1: {Start: 1, Count: 1, Total: 3, HasMore: true, Items: []steam.SearchObservation{testObservation(2, "19 Oct, 2026", steam.ReviewPositive, "Positive")}},
		}}
		service, repo := newSearchScanForTest(t, source, cfg)
		run := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 18, 0, 0, 0, time.UTC))
		if err := service.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedRecent}); err != nil {
			t.Fatal(err)
		}
		if err := service.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedRecent, Start: 1}); err != nil {
			t.Fatal(err)
		}
		var status string
		var monotonic, boundary bool
		if err := repo.store.Pool.QueryRow(context.Background(), `SELECT status,monotonic,boundary_stop FROM catalog_search_feeds WHERE run_id=$1 AND feed='recent'`, run.ID).Scan(&status, &monotonic, &boundary); err != nil {
			t.Fatal(err)
		}
		if status != "running" || monotonic || boundary {
			t.Fatalf("status=%s monotonic=%v boundary=%v", status, monotonic, boundary)
		}
	})
}

func TestOrdinaryMembershipNeedsTwoCompleteOmissions(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	service, repo := newSearchScanForTest(t, &searchStub{}, cfg)
	ctx := context.Background()
	first := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 16, 0, 0, 0, time.UTC))
	if err := service.CommitPage(ctx, first, steam.FeedRecent, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{testObservation(42, "18 Oct, 2026", steam.ReviewPositive, "Positive")}}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		run := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 16+i, 0, 0, 0, time.UTC))
		if err := service.CommitPage(ctx, run, steam.FeedRecent, 0, 0, steam.SearchPage{Start: 0, Total: 0}); err != nil {
			t.Fatal(err)
		}
		var active bool
		var omissions int
		if err := repo.store.Pool.QueryRow(ctx, `SELECT active,omission_count FROM catalog_search_memberships WHERE feed='recent' AND steam_app_id=42`).Scan(&active, &omissions); err != nil {
			t.Fatal(err)
		}
		if omissions != i || active != (i == 1) {
			t.Fatalf("pass=%d active=%v omissions=%d", i, active, omissions)
		}
	}
}

func TestSearchPagePayloadRejectsWrongRun(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	service, _ := newSearchScanForTest(t, &searchStub{}, cfg)
	if err := service.ProcessPage(context.Background(), SearchPagePayload{Version: 1, RunID: uuid.New(), Feed: steam.FeedRecent}); err == nil {
		t.Fatal("missing run succeeded")
	}
}

func TestTerminalSearchPageFailureAdvancesRunAndPreservesAuthority(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	source := &terminalSearchStub{}
	service, repo := newSearchScanForTest(t, source, cfg)
	ctx := context.Background()

	previous := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC))
	if err := service.CommitPage(ctx, previous, steam.FeedPopularUpcoming, 0, 0, steam.SearchPage{
		Start: 0, Count: 1, Total: 1,
		Items: []steam.SearchObservation{testObservation(42, "Coming Soon", steam.ReviewUnknown, "")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',worker_id=NULL,fencing_token=NULL,lease_until=NULL WHERE status IN ('queued','retryable')`); err != nil {
		t.Fatal(err)
	}

	run := searchTestRun(t, repo, cfg, time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC))
	if _, err := repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',worker_id=NULL,fencing_token=NULL,lease_until=NULL WHERE kind='catalog.search.run' AND status='queued'`); err != nil {
		t.Fatal(err)
	}
	payload := SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming}
	pageJob, err := service.queue.Enqueue(ctx, searchPageSpec(payload))
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 4; attempt++ {
		lease, err := service.queue.Claim(ctx, jobs.LaneCatalog, "catalog-test", time.Minute)
		if err != nil || lease == nil {
			t.Fatalf("attempt %d claim=%v err=%v", attempt, lease, err)
		}
		if lease.ID != pageJob || lease.Attempts != attempt {
			t.Fatalf("attempt %d got job=%s attempts=%d", attempt, lease.ID, lease.Attempts)
		}
		handlerErr := service.handlePage(ctx, *lease)
		if attempt < 4 {
			if handlerErr == nil || errors.Is(handlerErr, jobs.ErrDeferred) {
				t.Fatalf("attempt %d error=%v", attempt, handlerErr)
			}
			if outcome, err := service.queue.Fail(ctx, *lease, handlerErr, time.Now().Add(-time.Second)); err != nil || outcome != jobs.FailureRetryScheduled {
				t.Fatalf("attempt %d outcome=%s err=%v", attempt, outcome, err)
			}
		} else if !errors.Is(handlerErr, jobs.ErrDeferred) {
			t.Fatalf("terminal handler error=%v", handlerErr)
		}
	}

	var jobStatus, feedStatus, safeError string
	var active bool
	var publishedRevision int64
	if err := repo.store.Pool.QueryRow(ctx, `SELECT j.status,f.status,f.safe_error,m.active,p.published_run_revision
		FROM sync_jobs j JOIN catalog_search_feeds f ON f.run_id=$2 AND f.feed='popular_upcoming'
		JOIN catalog_search_memberships m ON m.feed=f.feed AND m.steam_app_id=42
		JOIN catalog_search_feed_publications p ON p.feed=f.feed WHERE j.id=$1`, pageJob, run.ID).Scan(&jobStatus, &feedStatus, &safeError, &active, &publishedRevision); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "dead" || feedStatus != "failed" || safeError != "invalid_response" || !active || publishedRevision != previous.Revision {
		t.Fatalf("job=%s feed=%s error=%s active=%v published=%d", jobStatus, feedStatus, safeError, active, publishedRevision)
	}

	for _, wantFeed := range []steam.SearchFeed{steam.FeedPopularNew, steam.FeedRecent, steam.FeedComingSoon} {
		lease, err := service.queue.Claim(ctx, jobs.LaneCatalog, "catalog-test", time.Minute)
		if err != nil || lease == nil {
			t.Fatalf("claim %s: %v,%v", wantFeed, lease, err)
		}
		var next SearchPagePayload
		if err := strictDiscoveryPayload(lease.Payload, &next); err != nil {
			t.Fatal(err)
		}
		if next.Feed != wantFeed {
			t.Fatalf("feed=%s want=%s", next.Feed, wantFeed)
		}
		if err := service.handlePage(ctx, *lease); err != nil {
			t.Fatalf("handle %s: %v", wantFeed, err)
		}
		if err := service.queue.Complete(ctx, *lease); err != nil {
			t.Fatalf("complete %s: %v", wantFeed, err)
		}
	}
	var runStatus string
	if err := repo.store.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_runs WHERE id=$1`, run.ID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" {
		t.Fatalf("run status=%s", runStatus)
	}
}
