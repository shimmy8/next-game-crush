package catalog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

func TestCatalogRebuildCoalescesEligibilityChanges(t *testing.T) {
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	state := rebuildCoordinatorState{UserID: uuid.New()}

	if changed := state.applyEligibilityChange(EligibilityChange{GameID: uuid.New(), Kind: "review_observation"}, base); changed {
		t.Fatal("review-only change dirtied the ranking")
	}
	for offset, kind := range []string{EligibilityChangeMembership, EligibilityChangeSemanticContent, EligibilityChangeReviewEligibility} {
		if changed := state.applyEligibilityChange(EligibilityChange{GameID: uuid.New(), Kind: kind}, base.Add(time.Duration(offset)*time.Minute)); !changed {
			t.Fatalf("meaningful change %q was ignored", kind)
		}
	}
	if state.DirtyRevision != 3 || state.FirstDirtyAt == nil || !state.FirstDirtyAt.Equal(base) {
		t.Fatalf("coalesced state = %+v", state)
	}
	if _, ok := state.nextWake(base.Add(9 * time.Minute)); ok {
		t.Fatal("wakeup scheduled before debounce elapsed")
	}

	wake, ok := state.nextWake(base.Add(10 * time.Minute))
	if !ok {
		t.Fatal("missing wakeup after debounce")
	}
	if wake.Revision != 3 || wake.RetryGeneration != 0 || !wake.AvailableAt.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("first wake = %+v", wake)
	}
	if !strings.Contains(wake.IdempotencyKey, state.UserID.String()) || !strings.Contains(wake.IdempotencyKey, ":3:retry:0") {
		t.Fatalf("unstable wake key %q", wake.IdempotencyKey)
	}
	if !state.recordWake(wake) {
		t.Fatal("failed to record first wake")
	}

	if changed := state.applyEligibilityChange(EligibilityChange{GameID: uuid.New(), Kind: EligibilityChangeMembership}, base.Add(11*time.Minute)); !changed {
		t.Fatal("later revision was ignored")
	}
	if _, ok := state.nextWake(base.Add(30 * time.Minute)); ok {
		t.Fatal("scheduled a second wake while one is pending")
	}
	if !state.finishWake(wake, true) {
		t.Fatal("successful wake did not pass its fence")
	}

	if _, ok := state.nextWake(base.Add(30 * time.Minute)); ok {
		t.Fatal("hour-capped successor fired early")
	}
	dueAt, ok := state.wakeDueAt()
	if !ok || !dueAt.Equal(base.Add(70*time.Minute)) {
		t.Fatalf("successor due=%s ok=%v", dueAt, ok)
	}
	successor, ok := state.nextWake(dueAt)
	if !ok || successor.Revision != 4 || !successor.AvailableAt.Equal(base.Add(70*time.Minute)) {
		t.Fatalf("hour-capped successor = %+v", successor)
	}
	if !state.recordWake(successor) || !state.finishWake(successor, false) {
		t.Fatal("failed wake did not pass its fence")
	}
	if state.RetryGeneration != 1 || state.LastCompletedRevision != 3 {
		t.Fatalf("failed wake mutated completion state: %+v", state)
	}
	retry, ok := state.nextWake(base.Add(3 * time.Hour))
	if !ok || retry.Revision != 4 || retry.RetryGeneration != 1 || !strings.HasSuffix(retry.IdempotencyKey, ":4:retry:1") {
		t.Fatalf("retry wake = %+v ok=%v", retry, ok)
	}
	for generation := 1; generation <= catalogRebuildMaxRetryGenerations; generation++ {
		if retry.RetryGeneration != generation || !state.recordWake(retry) || !state.finishWake(retry, false) {
			t.Fatalf("retry generation %d was not fenced: wake=%+v state=%+v", generation, retry, state)
		}
		if generation < catalogRebuildMaxRetryGenerations {
			retry, ok = state.nextWake(retry.AvailableAt.Add(2 * time.Hour))
			if !ok {
				t.Fatalf("missing retry generation %d", generation+1)
			}
		}
	}
	if _, ok := state.nextWake(retry.AvailableAt.Add(24 * time.Hour)); ok {
		t.Fatal("retry generations were not bounded")
	}

	newer := rebuildCoordinatorState{UserID: uuid.New()}
	newer.applyEligibilityChange(EligibilityChange{GameID: uuid.New(), Kind: EligibilityChangeMembership}, base)
	first, ok := newer.nextWake(base.Add(catalogRebuildDebounce))
	if !ok || !newer.recordWake(first) {
		t.Fatal("failed to prepare concurrent-change case")
	}
	newer.applyEligibilityChange(EligibilityChange{GameID: uuid.New(), Kind: EligibilityChangeSemanticContent}, base.Add(11*time.Minute))
	if !newer.finishWake(first, false) || newer.DirtyRevision != 2 || newer.RetryGeneration != 0 {
		t.Fatalf("new revision inherited stale retry generation: %+v", newer)
	}
}

func TestClockDrivenEligibilitySchedulesSuccessor(t *testing.T) {
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	due := base.Add(time.Hour)
	next := due.Add(24 * time.Hour)
	gameID := uuid.New()

	if _, _, ok := clockDrivenEligibilityChange(gameID, base, due, &next); ok {
		t.Fatal("future eligibility deadline fired early")
	}
	change, fence, ok := clockDrivenEligibilityChange(gameID, due, due, &next)
	if !ok || change.Kind != EligibilityChangeReviewEligibility || change.NextChangeAt == nil || !change.NextChangeAt.Equal(next) {
		t.Fatalf("due change=%+v fence=%+v ok=%v", change, fence, ok)
	}

	currentDue := due
	if updated, applied := fence.resolve(&currentDue, false); applied || updated == nil || !updated.Equal(due) {
		t.Fatalf("failed rebuild advanced due state: updated=%v applied=%v", updated, applied)
	}
	staleDue := due.Add(time.Minute)
	if updated, applied := fence.resolve(&staleDue, true); applied || updated == nil || !updated.Equal(staleDue) {
		t.Fatalf("stale success overwrote a newer deadline: updated=%v applied=%v", updated, applied)
	}
	updated, applied := fence.resolve(&currentDue, true)
	if !applied || updated == nil || !updated.Equal(next) {
		t.Fatalf("fenced success did not schedule successor: updated=%v applied=%v", updated, applied)
	}
}

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
