package catalog

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
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
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_runs SET status='partial',next_retry_at=$2 WHERE id=$1`, run.ID, due); err != nil {
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
