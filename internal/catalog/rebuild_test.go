package catalog

import (
	"context"
	"fmt"
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
	if err := service.repo.MarkCandidateSetDirtyAt(context.Background(), tx, EligibilityChange{GameID: uuid.New(), Kind: EligibilityChangeMembership}, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMarkCandidateSetDirtyAcceptsOnlyRankingKinds(t *testing.T) {
	service, _ := discoveryFixture(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	tx, err := service.repo.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []EligibilityChange{
		{GameID: uuid.New(), Kind: "review_observation"},
		{GameID: uuid.New(), Kind: EligibilityChangeMembership},
		{GameID: uuid.New(), Kind: EligibilityChangeSemanticContent},
		{GameID: uuid.New(), Kind: EligibilityChangeReviewEligibility},
	} {
		if err := service.repo.MarkCandidateSetDirtyAt(ctx, tx, change, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT dirty_revision FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 3 {
		t.Fatalf("dirty revision=%d want=3", revision)
	}
}

func TestScheduleDueRebuildsUpgradesLegacyDirtySinceRow(t *testing.T) {
	service, _ := discoveryFixture(t)
	ctx := context.Background()
	userID := uuid.New()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO catalog_rebuild_state(user_id,dirty_revision,enqueued_revision,dirty_since,first_dirty_at)
		VALUES($1,1,0,$2,NULL)`, userID, base); err != nil {
		t.Fatal(err)
	}
	if err := service.ScheduleDueRebuilds(ctx, base.Add(catalogRebuildDebounce)); err != nil {
		t.Fatal(err)
	}
	var key string
	var enqueued int64
	var firstDirty *time.Time
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT job.idempotency_key,state.enqueued_revision,state.first_dirty_at
		FROM catalog_rebuild_state state JOIN sync_jobs job ON job.id=state.pending_job_id WHERE state.user_id=$1`, userID).
		Scan(&key, &enqueued, &firstDirty); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(key, ":1:retry:0") || enqueued != 1 || firstDirty != nil {
		t.Fatalf("legacy row key=%q enqueued=%d firstDirty=%v", key, enqueued, firstDirty)
	}
}

func TestScheduleDueRebuildsReplacesTerminalLegacyPendingWithoutCompletingRevision(t *testing.T) {
	for _, terminalStatus := range []string{"succeeded", "dead"} {
		t.Run(terminalStatus, func(t *testing.T) {
			service, _ := discoveryFixture(t)
			ctx := context.Background()
			base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			userID, profileID, lkgID := uuid.New(), uuid.New(), uuid.New()
			if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, profileID, userID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published)
		VALUES($1,$2,$3,1,'ranking-v1','legacy-published',true)`, lkgID, userID, profileID); err != nil {
				t.Fatal(err)
			}
			legacyKey := fmt.Sprintf("catalog:recommendations.rebuild:v1:%s:1", userID)
			legacyID, err := service.queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: catalogRecommendationRebuildJobKind,
				Stage: "recommendations", IdempotencyKey: legacyKey, UserID: userID, MaxAttempts: 3})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status=$2 WHERE id=$1`, legacyID, terminalStatus); err != nil {
				t.Fatal(err)
			}
			if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO catalog_rebuild_state(user_id,dirty_revision,enqueued_revision,dirty_since,pending_job_id)
		VALUES($1,1,1,NULL,$2)`, userID, legacyID); err != nil {
				t.Fatal(err)
			}
			if err := service.ScheduleDueRebuilds(ctx, base); err != nil {
				t.Fatal(err)
			}
			var key string
			var completed int64
			var lkgPublished bool
			if err := service.repo.store.Pool.QueryRow(ctx, `SELECT job.idempotency_key,state.last_completed_revision,
		(SELECT published FROM recommendation_runs WHERE id=$2)
		FROM catalog_rebuild_state state JOIN sync_jobs job ON job.id=state.pending_job_id WHERE state.user_id=$1`, userID, lkgID).
				Scan(&key, &completed, &lkgPublished); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(key, ":1:retry:0") || completed != 0 || !lkgPublished {
				t.Fatalf("terminal legacy upgrade key=%q completed=%d lkg=%v", key, completed, lkgPublished)
			}
		})
	}
}

func TestScheduleDueRebuildsWaitsForActiveLegacyPendingThenContinuesV2(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			service, _ := discoveryFixture(t)
			ctx := context.Background()
			base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			clock := base
			service.now = func() time.Time { return clock }
			userID := uuid.New()
			if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
				t.Fatal(err)
			}
			legacyKey := fmt.Sprintf("catalog:recommendations.rebuild:v1:%s:1", userID)
			legacyID, err := service.queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: catalogRecommendationRebuildJobKind,
				Stage: "recommendations", IdempotencyKey: legacyKey, UserID: userID, MaxAttempts: 3})
			if err != nil {
				t.Fatal(err)
			}
			if status == "running" {
				if _, err := service.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='running',attempts=1,worker_id='legacy',
					fencing_token=$2,lease_until=$3 WHERE id=$1`, legacyID, uuid.New(), base.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO catalog_rebuild_state(user_id,dirty_revision,enqueued_revision,dirty_since,pending_job_id)
				VALUES($1,1,1,NULL,$2)`, userID, legacyID); err != nil {
				t.Fatal(err)
			}
			if err := service.ScheduleDueRebuilds(ctx, base); err != nil {
				t.Fatal(err)
			}
			var pending uuid.UUID
			var v2Count int
			if err := service.repo.store.Pool.QueryRow(ctx, `SELECT pending_job_id,
				(SELECT count(*) FROM sync_jobs WHERE idempotency_key LIKE 'catalog:recommendations.rebuild:v2:%')
				FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&pending, &v2Count); err != nil {
				t.Fatal(err)
			}
			if pending != legacyID || v2Count != 0 {
				t.Fatalf("active legacy pending=%s want=%s v2=%d", pending, legacyID, v2Count)
			}
			if _, err := service.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='succeeded',worker_id=NULL,fencing_token=NULL,lease_until=NULL WHERE id=$1`, legacyID); err != nil {
				t.Fatal(err)
			}
			if err := service.ScheduleDueRebuilds(ctx, base); err != nil {
				t.Fatal(err)
			}
			var firstKey string
			if err := service.repo.store.Pool.QueryRow(ctx, `SELECT job.idempotency_key FROM catalog_rebuild_state state
				JOIN sync_jobs job ON job.id=state.pending_job_id WHERE state.user_id=$1`, userID).Scan(&firstKey); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(firstKey, ":1:retry:0") {
				t.Fatalf("first v2 key=%q", firstKey)
			}
			if err := service.CatalogRebuildFinished(ctx, userID, firstKey, true); err != nil {
				t.Fatal(err)
			}
			clock = base.Add(2 * time.Hour)
			markCatalogDirty(t, service, clock)
			if err := service.ScheduleDueRebuilds(ctx, clock); err != nil {
				t.Fatal(err)
			}
			var latest string
			if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs
				WHERE idempotency_key LIKE 'catalog:recommendations.rebuild:v2:%' ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&latest); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(latest, ":2:retry:0") {
				t.Fatalf("later dirty key=%q", latest)
			}
		})
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
	if err := service.ScheduleDueRebuilds(ctx, base); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(4 * time.Minute) }
	markCatalogDirty(t, service, base.Add(4*time.Minute))
	if err := service.ScheduleDueRebuilds(ctx, base.Add(4*time.Minute)); err != nil {
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
	var key string
	var lane string
	var detached bool
	var maxAttempts int
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key,lane,parent_job_id IS NULL,max_attempts
		FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&key, &lane, &detached, &maxAttempts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(key, "catalog:recommendations.rebuild:v2:") || !strings.HasSuffix(key, ":1:retry:0") || lane != "interactive" || !detached || maxAttempts != 1 {
		t.Fatalf("unexpected catalog rebuild key=%q lane=%q detached=%v maxAttempts=%d", key, lane, detached, maxAttempts)
	}
	if err := service.ScheduleDueRebuilds(ctx, base.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("noop jobs=%d err=%v", count, err)
	}

	if _, err := service.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status='running',attempts=1,worker_id='test',fencing_token=$1,lease_until=now()+interval '1 hour' WHERE kind='recommendations.rebuild'`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	markCatalogDirty(t, service, base.Add(20*time.Minute))
	if err := service.ScheduleDueRebuilds(ctx, base.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1`, RebuildWakeJobKind).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy wakeups=%d err=%v", count, err)
	}
	if err := service.CatalogRebuildFinished(ctx, userID, key, true); err != nil {
		t.Fatal(err)
	}
	if err := service.ScheduleDueRebuilds(ctx, base.Add(70*time.Minute)); err != nil {
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

func TestCatalogRebuildFailureRetriesAreBoundedAndKeepPublishedRanking(t *testing.T) {
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
	markCatalogDirty(t, service, base)
	if err := service.ScheduleDueRebuilds(ctx, base.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for generation := 0; generation <= catalogRebuildMaxRetryGenerations; generation++ {
		var key string
		if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs WHERE kind='recommendations.rebuild' ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&key); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(key, fmt.Sprintf(":1:retry:%d", generation)) {
			t.Fatalf("generation %d key=%q", generation, key)
		}
		if err := service.CatalogRebuildFinished(ctx, userID, key, false); err != nil {
			t.Fatal(err)
		}
		if generation < catalogRebuildMaxRetryGenerations {
			if err := service.ScheduleDueRebuilds(ctx, base.Add(time.Duration(generation+2)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := service.ScheduleDueRebuilds(ctx, base.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var jobsCount int
	var completed int64
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sync_jobs WHERE kind='recommendations.rebuild'),last_completed_revision
		FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&jobsCount, &completed); err != nil {
		t.Fatal(err)
	}
	if jobsCount != catalogRebuildMaxRetryGenerations+1 || completed != 0 {
		t.Fatalf("jobs=%d completed=%d", jobsCount, completed)
	}
}

func TestNewDirtyRevisionDuringFailedPendingBuildStartsGenerationZero(t *testing.T) {
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
	markCatalogDirty(t, service, base)
	if err := service.ScheduleDueRebuilds(ctx, base.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var firstKey string
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&firstKey); err != nil {
		t.Fatal(err)
	}
	markCatalogDirty(t, service, base.Add(11*time.Minute))
	if err := service.CatalogRebuildFinished(ctx, userID, firstKey, false); err != nil {
		t.Fatal(err)
	}
	var secondKey string
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs WHERE kind='recommendations.rebuild' ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&secondKey); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(secondKey, ":2:retry:0") {
		t.Fatalf("new dirty revision inherited stale retry generation: %q", secondKey)
	}
}

func TestClockDrivenEligibilityRunsWhenDiscoveryDisabledAndResolvesAfterPublish(t *testing.T) {
	service, _ := discoveryFixture(t)
	service.cfg.Enabled = false
	ctx := context.Background()
	userID, expiredID, enteringID, laterExpiredID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := now
	service.now = func() time.Time { return clock }
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	for _, game := range []struct {
		id         uuid.UUID
		appID      int64
		name, raw  string
		release    string
		comingSoon bool
		active     bool
		deadline   time.Time
	}{
		{expiredID, 991201, "Expired", "17 Sep, 2026", "2026-09-17", false, true, now},
		{enteringID, 991202, "Entering", "17 Nov, 2026", "2026-11-17", true, false, now},
		{laterExpiredID, 991203, "Later expired", "17 Sep, 2026", "2026-09-17", false, true, now.Add(time.Hour)},
	} {
		if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,$2,$3,'steam',$4)`, game.id, game.appID, game.name, now); err != nil {
			t.Fatal(err)
		}
		if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO catalog_entry_sources(game_id,source,active,first_seen_at,last_seen_at) VALUES($1,'steam_discovery',$2,$3,$3)`, game.id, game.active, now); err != nil {
			t.Fatal(err)
		}
		if _, err := service.repo.store.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,release_text,precision,release_date,coming_soon,review_band,review_label,positive_percent,review_total,last_successful_appdetails_at,active,next_eligibility_at)
			VALUES($1,1,1,$2,'day',$3,$4,'positive','Positive',90,100,$5,$6,$7)`, game.appID, game.raw, game.release, game.comingSoon, now, game.active, game.deadline); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.ScheduleDueRebuilds(ctx, now); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	var expiredActive, enteringActive bool
	var expiredDue, enteringDue *time.Time
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT
		(SELECT active FROM catalog_search_current WHERE steam_app_id=991201),
		(SELECT active FROM catalog_search_current WHERE steam_app_id=991202),
		(SELECT next_eligibility_at FROM catalog_search_current WHERE steam_app_id=991201),
		(SELECT next_eligibility_at FROM catalog_search_current WHERE steam_app_id=991202)`).
		Scan(&expiredActive, &enteringActive, &expiredDue, &enteringDue); err != nil {
		t.Fatal(err)
	}
	if expiredActive || !enteringActive || expiredDue == nil || enteringDue == nil {
		t.Fatalf("projection/deadlines before publication: expired=%v entering=%v due=%v/%v", expiredActive, enteringActive, expiredDue, enteringDue)
	}
	if err := service.CatalogRebuildFinished(ctx, userID, key, false); err != nil {
		t.Fatal(err)
	}
	var retryKey string
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs WHERE kind='recommendations.rebuild' ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&retryKey); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(retryKey, ":1:retry:1") {
		t.Fatalf("retry key=%q", retryKey)
	}
	clock = now.Add(time.Hour)
	if err := service.ScheduleDueRebuilds(ctx, clock); err != nil {
		t.Fatal(err)
	}
	var revision int64
	var cutoff time.Time
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT dirty_revision,due_at FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&revision, &cutoff); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || !cutoff.Equal(now) {
		t.Fatalf("mid-build boundary revision=%d cutoff=%s", revision, cutoff)
	}
	if err := service.CatalogRebuildFinished(ctx, userID, retryKey, true); err != nil {
		t.Fatal(err)
	}
	var expiredCleared, laterRetained bool
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT
		(SELECT next_eligibility_at IS NULL FROM catalog_search_current WHERE steam_app_id=991201),
		(SELECT next_eligibility_at IS NOT NULL FROM catalog_search_current WHERE steam_app_id=991203)`).Scan(&expiredCleared, &laterRetained); err != nil {
		t.Fatal(err)
	}
	if !expiredCleared || !laterRetained {
		t.Fatalf("first cutoff advanced wrong deadlines: expiredCleared=%v laterRetained=%v", expiredCleared, laterRetained)
	}
	var successorKey string
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs WHERE kind='recommendations.rebuild' ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&successorKey); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(successorKey, ":2:retry:0") {
		t.Fatalf("successor key=%q", successorKey)
	}
	if err := service.CatalogRebuildFinished(ctx, userID, successorKey, true); err != nil {
		t.Fatal(err)
	}
	var laterCleared, expiredSource, enteringSource bool
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT
		(SELECT next_eligibility_at IS NULL FROM catalog_search_current WHERE steam_app_id=991203),
		(SELECT active FROM catalog_entry_sources WHERE game_id=$1 AND source='steam_discovery'),
		(SELECT active FROM catalog_entry_sources WHERE game_id=$2 AND source='steam_discovery')`, expiredID, enteringID).
		Scan(&laterCleared, &expiredSource, &enteringSource); err != nil {
		t.Fatal(err)
	}
	if !laterCleared || expiredSource || !enteringSource {
		t.Fatalf("final clock state: laterCleared=%v expiredSource=%v enteringSource=%v", laterCleared, expiredSource, enteringSource)
	}
}

func TestRebuildLockOrderQualificationAgainstSchedulerAndCompletion(t *testing.T) {
	const appID int64 = 991211
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) (*DiscoveryService, uuid.UUID) {
		t.Helper()
		service, _ := discoveryFixture(t)
		service.cfg.Enabled = false
		service.now = func() time.Time { return now }
		ctx := context.Background()
		userID, gameID := uuid.New(), uuid.New()
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
			{`INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, []any{uuid.New(), userID}},
			{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,$2,'Lock Order','steam',$3)`, []any{gameID, appID, now}},
			{`INSERT INTO catalog_entry_sources(game_id,source,active,first_seen_at,last_seen_at) VALUES($1,'steam_discovery',true,$2,$2)`, []any{gameID, now}},
			{`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,release_text,precision,release_date,
				coming_soon,review_band,review_label,positive_percent,review_total,last_successful_appdetails_at,active,next_eligibility_at)
				VALUES($1,1,1,'17 Sep, 2026','day','2026-09-17',false,'positive','Positive',90,100,$2,true,$2)`, []any{appID, now}},
		} {
			if _, err := service.repo.store.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		return service, userID
	}
	wait := func(t *testing.T, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("rebuild lock-order barrier timed out")
		}
	}

	t.Run("scheduler", func(t *testing.T) {
		service, userID := setup(t)
		ctx := context.Background()
		tx, err := service.repo.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		q := service.repo.store.Queries.WithTx(tx)
		if err := q.LockMetadataApp(ctx, appID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.LockCurrentMetadataCandidate(ctx, appID); err != nil {
			t.Fatal(err)
		}
		started, done := make(chan struct{}), make(chan error, 1)
		go func() {
			close(started)
			done <- service.ScheduleDueRebuilds(ctx, now)
		}()
		<-started
		if err := service.repo.MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: uuid.New(), Kind: EligibilityChangeSemanticContent}, now); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wait(t, done)
		var revision int64
		var key string
		if err := service.repo.store.Pool.QueryRow(ctx, `SELECT state.dirty_revision,job.idempotency_key FROM catalog_rebuild_state state
			JOIN sync_jobs job ON job.id=state.pending_job_id WHERE state.user_id=$1`, userID).Scan(&revision, &key); err != nil {
			t.Fatal(err)
		}
		if revision != 2 || !strings.HasSuffix(key, ":2:retry:0") {
			t.Fatalf("scheduler race revision=%d key=%q", revision, key)
		}
	})

	t.Run("completion", func(t *testing.T) {
		service, userID := setup(t)
		ctx := context.Background()
		if err := service.ScheduleDueRebuilds(ctx, now); err != nil {
			t.Fatal(err)
		}
		var firstKey string
		if err := service.repo.store.Pool.QueryRow(ctx, `SELECT idempotency_key FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&firstKey); err != nil {
			t.Fatal(err)
		}
		tx, err := service.repo.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		q := service.repo.store.Queries.WithTx(tx)
		if err := q.LockMetadataApp(ctx, appID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.LockCurrentMetadataCandidate(ctx, appID); err != nil {
			t.Fatal(err)
		}
		started, done := make(chan struct{}), make(chan error, 1)
		go func() {
			close(started)
			done <- service.CatalogRebuildFinished(ctx, userID, firstKey, true)
		}()
		<-started
		if err := service.repo.MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: uuid.New(), Kind: EligibilityChangeSemanticContent}, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wait(t, done)
		if err := service.ScheduleDueRebuilds(ctx, now.Add(catalogRebuildMinimumInterval)); err != nil {
			t.Fatal(err)
		}
		var dirty, completed int64
		var successor string
		var cleared bool
		if err := service.repo.store.Pool.QueryRow(ctx, `SELECT state.dirty_revision,state.last_completed_revision,job.idempotency_key,
			(SELECT next_eligibility_at IS NULL FROM catalog_search_current WHERE steam_app_id=$2)
			FROM catalog_rebuild_state state JOIN sync_jobs job ON job.id=state.pending_job_id WHERE state.user_id=$1`, userID, appID).
			Scan(&dirty, &completed, &successor, &cleared); err != nil {
			t.Fatal(err)
		}
		if dirty != 2 || completed != 1 || !strings.HasSuffix(successor, ":2:retry:0") || !cleared {
			t.Fatalf("completion race dirty=%d completed=%d successor=%q cleared=%v", dirty, completed, successor, cleared)
		}
	})
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
	if err := service.ScheduleDueRebuilds(ctx, time.Now().UTC()); err != nil {
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
