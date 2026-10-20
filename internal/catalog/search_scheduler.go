package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"
	"next-game-crush/internal/jobs"
)

const catalogRecoverySweepInterval = time.Minute

type SearchScheduler struct {
	repo                  *SearchRepository
	queue                 *jobs.Queue
	cfg                   DiscoveryConfig
	now                   func() time.Time
	daily, reconciliation cron.Schedule
	recoveryInterval      time.Duration
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
	return &SearchScheduler{repo: repo, queue: queue, cfg: cfg, now: now, daily: daily, reconciliation: reconciliation,
		recoveryInterval: catalogRecoverySweepInterval}, nil
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
	now := s.now().UTC()
	if err := s.repo.ScheduleDueRebuilds(ctx, s.cfg, now); err != nil {
		return err
	}
	if _, err := s.repo.EnqueueReadyIngressQualifications(ctx, s.queue, s.cfg, now); err != nil {
		return err
	}
	if !s.cfg.Enabled {
		return nil
	}
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

// Nudge is the user-sync seam. It only publishes durable scheduled work and
// never performs Steam I/O on the interactive worker.
func (s *SearchScheduler) Nudge(ctx context.Context) error { return s.EnsureScheduled(ctx) }

// Run owns only schedule timing. Work execution remains isolated in the
// catalog worker. The cheap recovery sweep remains active when Search is
// disabled so a qualification committed after an early sync nudge cannot
// strand durable rebuild state until process restart.
func (s *SearchScheduler) Run(ctx context.Context) error {
	if s == nil {
		return errors.New("search scheduler is required")
	}
	// User-sync ingress remains recoverable even when Search discovery is
	// disabled, so startup always performs one durable outbox sweep.
	if err := s.EnsureScheduled(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return nil
		}
		return err
	}
	for {
		now := s.now().UTC()
		next := now.Add(s.recoveryInterval)
		if s.cfg.Enabled {
			if candidate := s.daily.Next(now); candidate.Before(next) {
				next = candidate
			}
			if candidate := s.reconciliation.Next(now); candidate.Before(next) {
				next = candidate
			}
		}
		delay := next.Sub(now)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-timer.C:
			if err := s.EnsureScheduled(ctx); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
					return nil
				}
				return err
			}
		}
	}
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

// EnqueueTestReconciliation is a harness-only seam. Unlike the production
// weekly scheduler it uses the requested instant, so a fixture reset cannot
// accidentally reuse the current cron occurrence and skip upstream I/O.
func (s *SearchScheduler) EnqueueTestReconciliation(ctx context.Context, at time.Time) (RunRef, error) {
	return s.enqueueOccurrence(ctx, at, "reconciliation", RunReconciliation)
}
