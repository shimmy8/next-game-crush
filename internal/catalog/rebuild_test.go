package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

func markCatalogDirty(t *testing.T, service *DiscoveryService, at time.Time) {
	t.Helper()
	tx, err := service.repo.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := service.repo.MarkCandidatesChanged(context.Background(), tx, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRebuildDebounceAndNoop(t *testing.T) {
	service, _ := discoveryFixture(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return base }
	markCatalogDirty(t, service, base)
	if err := service.ScheduleRebuilds(ctx); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(4 * time.Minute) }
	markCatalogDirty(t, service, base.Add(4*time.Minute))
	if err := service.ScheduleRebuilds(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	var available time.Time
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT count(*),min(available_at) FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&count, &available); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !available.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("jobs=%d available=%s", count, available)
	}
	if err := service.ScheduleRebuilds(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("noop jobs=%d err=%v", count, err)
	}

	if _, err := service.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='running',attempts=1,worker_id='test',fencing_token=$1,lease_until=now()+interval '1 hour' WHERE kind='recommendations.rebuild'`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(20 * time.Minute) }
	markCatalogDirty(t, service, base.Add(20*time.Minute))
	if err := service.ScheduleRebuilds(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1`, RebuildWakeJobKind).Scan(&count); err != nil || count != 1 {
		t.Fatalf("running dirty wakeups=%d err=%v", count, err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='succeeded',worker_id=NULL,fencing_token=NULL,lease_until=NULL WHERE kind='recommendations.rebuild'`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET available_at=now() WHERE kind=$1`, RebuildWakeJobKind); err != nil {
		t.Fatal(err)
	}
	wake, err := service.queue.Claim(ctx, jobs.LaneInteractive, "rebuild-wakeup-test", time.Minute)
	if err != nil || wake == nil || wake.Kind != RebuildWakeJobKind {
		t.Fatalf("wake claim=%+v err=%v", wake, err)
	}
	if err := service.handleRebuildWakeup(ctx, *wake); err != nil {
		t.Fatal(err)
	}
	var second time.Time
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT count(*),max(available_at) FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&count, &second); err != nil {
		t.Fatal(err)
	}
	if count != 2 || second.Before(base.Add(70*time.Minute)) {
		t.Fatalf("second jobs=%d available=%s", count, second)
	}
}

func TestCatalogRebuildCannotResurrectDeletedUser(t *testing.T) {
	service, _ := discoveryFixture(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	markCatalogDirty(t, service, time.Now().UTC())
	if _, err := service.repo.store.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	if err := service.ScheduleRebuilds(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs, states int
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sync_jobs WHERE user_id=$1),(SELECT count(*) FROM catalog_rebuild_state WHERE user_id=$1)`, userID).Scan(&jobs, &states); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || states != 0 {
		t.Fatalf("deleted user jobs=%d states=%d", jobs, states)
	}
}
