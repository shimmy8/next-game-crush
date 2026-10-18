package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"
	"next-game-crush/internal/jobs"
)

type SearchScheduler struct {
	repo                  *SearchRepository
	queue                 *jobs.Queue
	cfg                   DiscoveryConfig
	now                   func() time.Time
	daily, reconciliation cron.Schedule
}

func NewSearchScheduler(repo *SearchRepository, queue *jobs.Queue, cfg DiscoveryConfig, now func() time.Time) (*SearchScheduler, error) {
	if repo == nil || repo.store == nil || repo.store.Pool == nil || queue == nil {
		return nil, errors.New("search scheduler dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	_, daily, err := ParseSchedule(cfg.RefreshCron, cfg.RefreshTimezone)
	if err != nil {
		return nil, err
	}
	_, reconciliation, err := ParseSchedule(cfg.ReconciliationCron, cfg.RefreshTimezone)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &SearchScheduler{repo: repo, queue: queue, cfg: cfg, now: now, daily: daily, reconciliation: reconciliation}, nil
}

// latestOccurrence asks cron for real instants rather than subtracting a day,
// preserving skipped and repeated local hours across timezone transitions.
func latestOccurrence(schedule cron.Schedule, now time.Time) (time.Time, error) {
	for lookback := time.Minute; lookback <= 8*366*24*time.Hour; lookback *= 2 {
		next := schedule.Next(now.UTC().Truncate(time.Minute).Add(-lookback))
		if next.IsZero() || next.After(now) {
			continue
		}
		for {
			after := schedule.Next(next)
			if after.IsZero() || after.After(now) {
				return next.UTC(), nil
			}
			next = after
		}
	}
	return time.Time{}, errors.New("catalog schedule has no recent occurrence")
}

func (s *SearchScheduler) EnsureScheduled(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	now := s.now().UTC()
	daily, err := latestOccurrence(s.daily, now)
	if err != nil {
		return err
	}
	reconciliation, err := latestOccurrence(s.reconciliation, now)
	if err != nil {
		return err
	}
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.repo.store.Queries.WithTx(tx)
	// A persistent singleton serializes competing startup/initial coordinators.
	// This lock does not change the legacy cutover fence.
	if _, err := q.LockCatalogWorkflowState(ctx); err != nil {
		return err
	}
	if err := s.repo.rearmTerminalRuns(ctx, tx, now); err != nil {
		return err
	}
	fresh, err := q.SearchCatalogNeedsInitial(ctx)
	if err != nil {
		return err
	}
	if fresh {
		// A stable lifetime identity prevents repeated startup failures from
		// creating independent initial runs. Failed initial runs use generations.
		if _, err := s.repo.createRun(ctx, tx, s.cfg, "initial", RunInitial, time.Unix(0, 0)); err != nil {
			return err
		}
	}
	if _, err := s.repo.createRun(ctx, tx, s.cfg, "daily", RunScheduled, daily); err != nil {
		return err
	}
	if _, err := s.repo.createRun(ctx, tx, s.cfg, "reconciliation", RunReconciliation, reconciliation); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *SearchScheduler) enqueueOccurrence(ctx context.Context, at time.Time, schedule string, kind RunKind) (RunRef, error) {
	if at.IsZero() {
		return RunRef{}, errors.New("catalog occurrence time is required")
	}
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RunRef{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := s.repo.store.Queries.WithTx(tx).LockCatalogWorkflowState(ctx); err != nil {
		return RunRef{}, err
	}
	ref, err := s.repo.createRun(ctx, tx, s.cfg, schedule, kind, at)
	if err != nil {
		return RunRef{}, err
	}
	return ref, tx.Commit(ctx)
}

func (s *SearchScheduler) EnqueueManual(ctx context.Context, at time.Time) (RunRef, error) {
	return s.enqueueOccurrence(ctx, at, "manual", RunManual)
}

func (s *SearchScheduler) EnqueueReconciliation(ctx context.Context, at time.Time) (RunRef, error) {
	occurrence, err := latestOccurrence(s.reconciliation, at)
	if err != nil {
		return RunRef{}, err
	}
	return s.enqueueOccurrence(ctx, occurrence, "reconciliation", RunReconciliation)
}
