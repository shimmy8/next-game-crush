package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	dbgen "next-game-crush/internal/store/sqlc"
)

func (r *DiscoveryRepository) scheduleScan(ctx context.Context, mode DiscoveryMode, now time.Time, cfg DiscoveryConfig) (bool, error) {
	if !cfg.Enabled {
		return false, nil
	}
	if !validDiscoveryMode(mode) || cfg.Validate() != nil {
		return false, errors.New("invalid discovery scan configuration")
	}
	hour := now.UTC().Truncate(time.Hour)
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	state, err := q.LockCatalogDiscoveryState(ctx, string(mode))
	if err != nil {
		return false, err
	}
	if err = q.EnsureDiscoveryBudget(ctx, discoveryTimestamp(hour)); err != nil {
		return false, err
	}
	budget, err := q.LockDiscoveryBudget(ctx, discoveryTimestamp(hour))
	if err != nil {
		return false, err
	}
	if int(budget.Used) >= cfg.HourlyBudget {
		return false, tx.Commit(ctx)
	}
	complete := !state.ScanStartedAt.Valid || (state.ScanCompletedAt.Valid && !state.ScanCompletedAt.Time.Before(state.ScanStartedAt.Time))
	if mode == DiscoveryIncremental && complete && state.ScanStartedAt.Valid && now.Before(state.ScanStartedAt.Time.Add(time.Duration(cfg.IntervalHours)*time.Hour)) {
		return false, tx.Commit(ctx)
	}
	if mode == DiscoveryBackfill {
		var already bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_jobs WHERE kind=$1 AND (payload->>'budget_hour')::timestamptz=$2 AND status<>'dead')`, BackfillJobKind, hour).Scan(&already); err != nil {
			return false, err
		}
		if already {
			return false, tx.Commit(ctx)
		}
	}
	if complete {
		state, err = q.BeginCatalogDiscoveryScan(ctx, dbgen.BeginCatalogDiscoveryScanParams{Mode: string(mode), StartedAt: discoveryTimestamp(now.UTC().Truncate(time.Second))})
		if err != nil {
			return false, err
		}
	}
	p := ScanPayload{Version: 1, Mode: mode, ScanStartedAt: state.ScanStartedAt.Time, AfterAppID: uint32(state.CursorAppID), BudgetHour: hour}
	var lastGeneration int
	var status string
	var priorResult []byte
	err = tx.QueryRow(ctx, `SELECT COALESCE((payload->>'page_generation')::integer,0),status,result FROM sync_jobs WHERE kind=$1 AND payload->>'scan_started_at'=$2 AND (payload->>'after_app_id')::bigint=$3 ORDER BY (payload->>'page_generation')::integer DESC LIMIT 1`, "catalog.discover."+string(mode), p.ScanStartedAt.Format(time.RFC3339Nano), int64(p.AfterAppID)).Scan(&lastGeneration, &status, &priorResult)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if status != "dead" {
			return false, tx.Commit(ctx)
		}
		p.PageGeneration = lastGeneration + 1
	}
	raw, _ := json.Marshal(p)
	id, err := jobs.EnqueueInTx(ctx, q, jobs.Spec{Lane: jobs.LaneInteractive, Kind: "catalog.discover." + string(mode), Stage: "catalog_discover", IdempotencyKey: scanKey(p), Payload: raw, Priority: scanPriority(mode), MaxAttempts: 3})
	if err != nil {
		return false, err
	}
	// A dead page may already have committed children. Reuse precisely its
	// manifest rather than fetching a moving source page under a new generation.
	if len(priorResult) > 0 && string(priorResult) != "{}" {
		if _, err = tx.Exec(ctx, `UPDATE sync_jobs SET result=$2 WHERE id=$1`, id, priorResult); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (r *DiscoveryRepository) ScheduleDueRetries(ctx context.Context, now time.Time, cfg DiscoveryConfig) (int, error) {
	if !cfg.Enabled {
		return 0, nil
	}
	if cfg.Validate() != nil {
		return 0, errors.New("invalid discovery retry configuration")
	}
	hour := now.UTC().Truncate(time.Hour)
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	// Keep hourly accounting bounded while retaining every row still referenced
	// by unfinished work, including a page paused across process restarts.
	if _, err = tx.Exec(ctx, `DELETE FROM catalog_discovery_budget b WHERE b.hour_at<$1 AND NOT EXISTS(SELECT 1 FROM sync_jobs j WHERE j.kind IN ('catalog.qualify','catalog.discover.incremental','catalog.discover.backfill') AND j.status NOT IN ('succeeded','dead') AND (j.payload->>'budget_hour')::timestamptz=b.hour_at)`, now.UTC().AddDate(0, 0, -7)); err != nil {
		return 0, err
	}
	if err = q.EnsureDiscoveryBudget(ctx, discoveryTimestamp(hour)); err != nil {
		return 0, err
	}
	budget, err := q.LockDiscoveryBudget(ctx, discoveryTimestamp(hour))
	if err != nil {
		return 0, err
	}
	remaining := cfg.HourlyBudget - int(budget.Used)
	if remaining <= 0 {
		return 0, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT steam_app_id,last_modified,retry_generation FROM catalog_discovery_observations WHERE source='steam' AND status='failed' AND next_retry_at<=$1 ORDER BY next_retry_at,steam_app_id,last_modified LIMIT $2 FOR UPDATE SKIP LOCKED`, now, remaining)
	if err != nil {
		return 0, err
	}
	type retry struct {
		app        DiscoveredApp
		generation int
	}
	var due []retry
	for rows.Next() {
		var item retry
		if err = rows.Scan(&item.app.AppID, &item.app.LastModified, &item.generation); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range due {
		oldKey := qualificationKey(item.app, item.generation)
		var oldStatus string
		err = tx.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE idempotency_key=$1`, oldKey).Scan(&oldStatus)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		if err == nil && oldStatus != "dead" {
			continue
		}
		if item.generation >= 2147483647 {
			return 0, errors.New("discovery retry generation exhausted")
		}
		item.generation++
		ok, e := r.reserveInTx(ctx, tx, hour, qualificationKey(item.app, item.generation), cfg.HourlyBudget)
		if e != nil {
			return 0, e
		}
		if !ok {
			break
		}
		p := QualificationPayload{Version: 1, App: item.app, RetryGeneration: item.generation, BudgetHour: hour, Priority: 50}
		if _, err = jobs.EnqueueInTx(ctx, q, qualificationSpec(p, uuid.Nil)); err != nil {
			return 0, err
		}
		// Keep failed history but claim its next generation exactly once. Its
		// next_retry_at is populated again only if that new generation dies.
		if _, err = tx.Exec(ctx, `UPDATE catalog_discovery_observations SET retry_generation=$3,next_retry_at=NULL WHERE source='steam' AND steam_app_id=$1 AND last_modified=$2`, item.app.AppID, item.app.LastModified, item.generation); err != nil {
			return 0, err
		}
		count++
	}
	// A due retry is not a page child, so a watchdog reconciles exhausted root
	// qualifications too. This uses the durable payload, never raw error text.
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func (r *DiscoveryRepository) reconcileDeadRetries(ctx context.Context, now time.Time) error {
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT j.payload,j.attempts FROM sync_jobs j WHERE j.kind='catalog.qualify' AND j.status='dead' AND j.parent_job_id IS NULL`)
	if err != nil {
		return err
	}
	type dead struct {
		p        QualificationPayload
		attempts int
	}
	var items []dead
	for rows.Next() {
		var raw []byte
		var d dead
		if err = rows.Scan(&raw, &d.attempts); err != nil {
			rows.Close()
			return err
		}
		if strictDiscoveryPayload(raw, &d.p) != nil || d.p.Version != 1 || !validDiscoveredApp(d.p.App) {
			rows.Close()
			return fmt.Errorf("invalid durable discovery retry")
		}
		items = append(items, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range items {
		if _, err = tx.Exec(ctx, `UPDATE catalog_discovery_observations SET next_retry_at=$4,safe_error='retry_exhausted',attempts=attempts+$5 WHERE source='steam' AND steam_app_id=$1 AND last_modified=$2 AND retry_generation=$3 AND status='failed' AND next_retry_at IS NULL`, d.p.App.AppID, d.p.App.LastModified, d.p.RetryGeneration, now.Add(discoveryRetryDelay(d.p.RetryGeneration)), d.attempts); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
