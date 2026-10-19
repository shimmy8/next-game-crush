package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/store"
)

type DiscoveryRepository struct{ store *store.Store }

type LegacyCutoverResult struct {
	Cancelled     int64
	RunningFenced int64
}

func NewDiscoveryRepository(s *store.Store) *DiscoveryRepository {
	return &DiscoveryRepository{store: s}
}

// CutOverLegacyDiscovery remains through the rollout release so databases
// touched by the immediately previous binary cannot strand legacy jobs.
func (r *DiscoveryRepository) CutOverLegacyDiscovery(ctx context.Context) (LegacyCutoverResult, error) {
	result := LegacyCutoverResult{}
	if r == nil || r.store == nil {
		return result, errors.New("legacy discovery persistence is required")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	state, err := q.LockCatalogWorkflowState(ctx)
	if err != nil {
		return result, err
	}
	if state.LegacyEnqueueEnabled {
		if _, err := q.CloseLegacyCatalogWorkflow(ctx, pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}); err != nil {
			return result, err
		}
	}
	cancelled, err := q.DeadLetterQueuedLegacyDiscovery(ctx)
	if err != nil {
		return result, err
	}
	result.Cancelled = int64(len(cancelled))
	if len(cancelled) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE job_attempts SET status='dead',finished_at=now(),error='superseded_catalog_workflow'
			WHERE job_id=ANY($1::uuid[]) AND finished_at IS NULL`, cancelled); err != nil {
			return result, err
		}
	}
	running, err := q.FenceRunningLegacyDiscovery(ctx)
	if err != nil {
		return result, err
	}
	result.RunningFenced = int64(len(running))
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func (r *DiscoveryRepository) CountNonterminalLegacyJobs(ctx context.Context) (int64, error) {
	if r == nil || r.store == nil {
		return 0, errors.New("legacy discovery persistence is required")
	}
	return r.store.Queries.CountNonterminalLegacyDiscovery(ctx)
}

func (r *DiscoveryRepository) SearchPresentationStatus(ctx context.Context, enabled bool, now time.Time) (PresentationStatus, error) {
	result := PresentationStatus{Status: "disabled"}
	if !enabled {
		return result, nil
	}
	var complete *time.Time
	var active bool
	if err := r.store.Pool.QueryRow(ctx, `SELECT
		max(completed_at) FILTER(WHERE kind IN ('initial','scheduled','manual') AND status='complete'),
		count(*) FILTER(WHERE kind IN ('initial','scheduled','manual') AND status IN ('queued','running'))>0
		FROM catalog_search_runs`).Scan(&complete, &active); err != nil {
		return result, err
	}
	if err := r.store.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_search_current WHERE active`).Scan(&result.ActiveDiscoveredGames); err != nil {
		return result, err
	}
	result.LastSuccessfulScanAt = complete
	result.BackfillCompletedAt = complete
	switch {
	case active:
		result.Status = "updating"
	case complete == nil:
		result.Status = "stale"
	case complete.Before(now.Add(-48 * time.Hour)):
		result.Status = "stale"
	default:
		result.Status = "ready"
	}
	var failed bool
	if err := r.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_search_runs
		WHERE kind IN ('initial','scheduled','manual') AND status IN ('failed','partial')
		AND run_revision=(SELECT max(run_revision) FROM catalog_search_runs WHERE kind IN ('initial','scheduled','manual')))`).Scan(&failed); err != nil {
		return result, err
	}
	if failed {
		result.Status = "degraded"
	}
	return result, nil
}

func (r *DiscoveryRepository) SearchDiscoveryDiagnostics(ctx context.Context, enabled bool, now time.Time) (DiscoveryDiagnostics, error) {
	status, err := r.SearchPresentationStatus(ctx, enabled, now)
	if err != nil {
		return DiscoveryDiagnostics{}, err
	}
	result := DiscoveryDiagnostics{PresentationStatus: status, ActiveBySource: map[string]int64{"seed": 0, "steam_discovery": 0}}
	var seed, discovered int64
	if err := r.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE source='seed'),count(*) FILTER(WHERE source='steam_discovery')
		FROM catalog_entry_sources WHERE active`).Scan(&seed, &discovered); err != nil {
		return result, err
	}
	result.ActiveBySource["seed"], result.ActiveBySource["steam_discovery"] = seed, discovered
	if err := r.store.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_metadata_ingress_work WHERE status IN ('pending','reserved','deferred')`).Scan(&result.PendingQualifications); err != nil {
		return result, err
	}
	if status.Status == "degraded" {
		result.Reason = "upstream_unavailable"
	} else if status.Status == "stale" {
		result.Reason = "stale"
	} else if status.Status == "updating" {
		result.Reason = "backfill_in_progress"
	} else if status.Status == "disabled" {
		result.Reason = "disabled"
	}
	return result, nil
}

func discoveryTimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}
