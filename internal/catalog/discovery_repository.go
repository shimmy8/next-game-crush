package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type DiscoveryRepository struct{ store *store.Store }

type LegacyCutoverResult struct {
	Cancelled     int64
	RunningFenced int64
}

type legacySupersededError struct{}

func (legacySupersededError) Error() string   { return "superseded_catalog_workflow" }
func (legacySupersededError) Retryable() bool { return false }

func NewDiscoveryRepository(s *store.Store) *DiscoveryRepository {
	return &DiscoveryRepository{store: s}
}

// LegacyEnqueueAllowedForUpdate serializes every legacy producer with cutover.
// Callers must invoke it before locking or mutating any job row.
func (r *DiscoveryRepository) LegacyEnqueueAllowedForUpdate(ctx context.Context, tx pgx.Tx) (bool, error) {
	if r == nil || r.store == nil || tx == nil {
		return false, errors.New("legacy discovery persistence is required")
	}
	state, err := r.store.Queries.WithTx(tx).LockCatalogWorkflowState(ctx)
	if err != nil {
		return false, err
	}
	return state.LegacyEnqueueEnabled, nil
}

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

func validDiscoveryMode(mode DiscoveryMode) bool {
	return mode == DiscoveryIncremental || mode == DiscoveryBackfill
}
func (r *DiscoveryRepository) State(ctx context.Context, mode DiscoveryMode) (ScanState, error) {
	if !validDiscoveryMode(mode) {
		return ScanState{}, errors.New("invalid discovery mode")
	}
	row, err := r.store.Queries.GetCatalogDiscoveryState(ctx, string(mode))
	return scanState(row), err
}
func (r *DiscoveryRepository) BeginScan(ctx context.Context, mode DiscoveryMode, now time.Time) (ScanState, error) {
	if !validDiscoveryMode(mode) || now.IsZero() {
		return ScanState{}, errors.New("invalid discovery scan")
	}
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return ScanState{}, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	row, err := q.LockCatalogDiscoveryState(ctx, string(mode))
	if err != nil {
		return ScanState{}, err
	}
	if !row.ScanStartedAt.Valid || (row.ScanCompletedAt.Valid && !row.ScanCompletedAt.Time.Before(row.ScanStartedAt.Time)) {
		row, err = q.BeginCatalogDiscoveryScan(ctx, dbgen.BeginCatalogDiscoveryScanParams{Mode: string(mode), StartedAt: discoveryTimestamp(now)})
		if err != nil {
			return ScanState{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return ScanState{}, err
	}
	return scanState(row), nil
}

func (r *DiscoveryRepository) SaveObservation(ctx context.Context, o Observation) error {
	if o.AppID < 1 || o.AppID > math.MaxUint32 || o.LastModified.IsZero() || o.LastModified.Unix() < 1 || o.LastModified.Unix() > math.MaxUint32 || o.Attempts < 1 || o.Attempts > math.MaxInt32 || o.RetryGeneration < 0 || o.RetryGeneration > math.MaxInt32 || !utf8.ValidString(o.Window.Raw) || utf8.RuneCountInString(o.Window.Raw) > 512 {
		return errors.New("invalid discovery observation")
	}
	switch o.Status {
	case "admitted", "outside_horizon", "released", "not_game", "unavailable", "failed":
	default:
		return errors.New("invalid discovery observation status")
	}
	switch o.Window.Precision {
	case "", PrecisionDay, PrecisionMonth, PrecisionQuarter, PrecisionYear, PrecisionComingSoon, PrecisionUnknown:
	default:
		return errors.New("invalid discovery release precision")
	}
	if o.Window.Date != nil && o.Window.Precision != PrecisionDay {
		return errors.New("invalid discovery release date")
	}
	args := dbgen.UpsertDiscoveryObservationParams{SteamAppID: o.AppID, LastModified: discoveryTimestamp(o.LastModified.UTC().Truncate(time.Second)), Status: o.Status, Precision: pgtype.Text{String: string(o.Window.Precision), Valid: o.Window.Precision != ""}, RawReleaseText: o.Window.Raw, Attempts: int32(o.Attempts), RetryGeneration: int32(o.RetryGeneration)}
	if o.Window.Date != nil {
		args.ReleaseDate = pgtype.Date{Time: o.Window.Date.UTC(), Valid: true}
	}
	if o.Status == "failed" {
		args.SafeError = safeDiscoveryError(o.SafeError)
		if o.NextRetryAt != nil {
			args.NextRetryAt = discoveryTimestamp(*o.NextRetryAt)
		}
	}
	return r.store.Queries.UpsertDiscoveryObservation(ctx, args)
}

func (r *DiscoveryRepository) Reserve(ctx context.Context, hour time.Time, identity string, limit int) (bool, error) {
	if hour.IsZero() || identity == "" || len(identity) > 1024 || limit < 1 || limit > 1000 {
		return false, errors.New("invalid discovery reservation")
	}
	hour = hour.UTC().Truncate(time.Hour)
	sum := sha256.Sum256([]byte(identity))
	key := hex.EncodeToString(sum[:])
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	at := discoveryTimestamp(hour)
	if err = q.EnsureDiscoveryBudget(ctx, at); err != nil {
		return false, err
	}
	row, err := q.LockDiscoveryBudget(ctx, at)
	if err != nil {
		return false, err
	}
	reservations := map[string]bool{}
	if err = json.Unmarshal(row.Reservations, &reservations); err != nil {
		return false, errors.New("invalid discovery budget state")
	}
	if reservations[key] {
		return true, tx.Commit(ctx)
	}
	if int(row.Used) >= limit {
		return false, tx.Commit(ctx)
	}
	if err = q.ReserveDiscoveryBudget(ctx, dbgen.ReserveDiscoveryBudgetParams{HourAt: at, IdentityHash: key}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (r *DiscoveryRepository) PresentationStatus(ctx context.Context, enabled bool, now time.Time, interval time.Duration) (PresentationStatus, error) {
	result := PresentationStatus{Status: "disabled"}
	if interval <= 0 {
		return result, errors.New("invalid discovery status interval")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	result, err = presentationStatusInSnapshot(ctx, q, enabled, now, interval)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func presentationStatusInSnapshot(ctx context.Context, q *dbgen.Queries, enabled bool, now time.Time, interval time.Duration) (PresentationStatus, error) {
	result := PresentationStatus{Status: "disabled"}
	incremental, err := q.GetCatalogDiscoveryState(ctx, string(DiscoveryIncremental))
	if err != nil {
		return result, err
	}
	backfill, err := q.GetCatalogDiscoveryState(ctx, string(DiscoveryBackfill))
	if err != nil {
		return result, err
	}
	result.ActiveDiscoveredGames, err = q.CountDiscoveryProvenance(ctx)
	if err != nil {
		return result, err
	}
	result.LastSuccessfulScanAt = discoveryTimePointer(incremental.ScanCompletedAt)
	result.BackfillCompletedAt = discoveryTimePointer(backfill.ScanCompletedAt)
	if enabled {
		latest := incremental
		if backfill.UpdatedAt.Time.After(incremental.UpdatedAt.Time) {
			latest = backfill
		}
		active := func(row dbgen.CatalogDiscoveryState) bool {
			return row.ScanStartedAt.Valid && (!row.ScanCompletedAt.Valid || row.ScanStartedAt.Time.After(row.ScanCompletedAt.Time))
		}
		stale := incremental.ScanCompletedAt.Valid && incremental.ScanCompletedAt.Time.Before(now.Add(-2*interval))
		if !incremental.ScanCompletedAt.Valid && incremental.ScanStartedAt.Valid && incremental.ScanStartedAt.Time.Before(now.Add(-2*interval)) {
			stale = true
		}
		switch {
		case latest.Availability == "request_failed" || latest.Availability == "unavailable":
			result.Status = "degraded"
		case stale:
			result.Status = "stale"
		case !backfill.ScanCompletedAt.Valid || active(incremental) || active(backfill):
			result.Status = "updating"
		case !incremental.ScanCompletedAt.Valid:
			result.Status = "stale"
		default:
			result.Status = "ready"
		}
	}
	return result, nil
}

func (r *DiscoveryRepository) Diagnostics(ctx context.Context, enabled bool, now time.Time, interval time.Duration) (DiscoveryDiagnostics, error) {
	if interval <= 0 {
		return DiscoveryDiagnostics{}, errors.New("invalid discovery status interval")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DiscoveryDiagnostics{}, err
	}
	defer tx.Rollback(ctx)
	status, err := presentationStatusInSnapshot(ctx, r.store.Queries.WithTx(tx), enabled, now, interval)
	if err != nil {
		return DiscoveryDiagnostics{}, err
	}
	result := DiscoveryDiagnostics{PresentationStatus: status, ActiveBySource: map[string]int64{"seed": 0, "steam_discovery": 0}}
	var cursor int64
	if err = tx.QueryRow(ctx, `SELECT cursor_app_id FROM catalog_discovery_state WHERE source='steam' AND mode='backfill'`).Scan(&cursor); err != nil {
		return DiscoveryDiagnostics{}, err
	}
	result.BackfillCursor = uint32(cursor)
	rows, err := tx.Query(ctx, `SELECT source,count(DISTINCT game_id) FROM catalog_entry_sources WHERE active GROUP BY source`)
	if err != nil {
		return DiscoveryDiagnostics{}, err
	}
	for rows.Next() {
		var source string
		var count int64
		if err = rows.Scan(&source, &count); err != nil {
			rows.Close()
			return DiscoveryDiagnostics{}, err
		}
		if source == "seed" || source == "steam_discovery" {
			result.ActiveBySource[source] = count
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return DiscoveryDiagnostics{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='catalog.qualify' AND status IN ('queued','running','waiting','retryable')`).Scan(&result.PendingQualifications); err != nil {
		return DiscoveryDiagnostics{}, err
	}
	var latestHour *time.Time
	if err = tx.QueryRow(ctx, `SELECT max(hour_at) FROM catalog_discovery_budget`).Scan(&latestHour); err != nil {
		return DiscoveryDiagnostics{}, err
	}
	if latestHour != nil {
		var invalidSkipped int64
		if err = tx.QueryRow(ctx, `SELECT used,COALESCE((counts->>'evaluated')::bigint,0),COALESCE((counts->>'skipped_invalid')::bigint,0) FROM catalog_discovery_budget WHERE hour_at=$1`, *latestHour).Scan(&result.LatestRun.Discovered, &result.LatestRun.Evaluated, &invalidSkipped); err != nil {
			return DiscoveryDiagnostics{}, err
		}
		if err = tx.QueryRow(ctx, `WITH run_jobs AS (
			SELECT DISTINCT ON ((payload#>>'{app,AppID}')::bigint,(payload#>>'{app,LastModified}')::timestamptz)
				(payload#>>'{app,AppID}')::bigint AS app_id,
				(payload#>>'{app,LastModified}')::timestamptz AS last_modified,
				(payload->>'retry_generation')::integer AS retry_generation
			FROM sync_jobs
			WHERE kind='catalog.qualify' AND status IN ('succeeded','dead') AND (payload->>'budget_hour')::timestamptz=$1
			ORDER BY (payload#>>'{app,AppID}')::bigint,(payload#>>'{app,LastModified}')::timestamptz,(payload->>'retry_generation')::integer DESC
		), outcomes AS (
			SELECT observation.status FROM run_jobs job
			JOIN catalog_discovery_observations observation
			  ON observation.source='steam' AND observation.steam_app_id=job.app_id
			 AND observation.last_modified=job.last_modified
			 AND observation.retry_generation=job.retry_generation
		)
		SELECT
			count(*) FILTER(WHERE status='admitted'),
			count(*) FILTER(WHERE status IN ('outside_horizon','released','not_game','unavailable')),
			count(*) FILTER(WHERE status='failed') FROM outcomes`, *latestHour).Scan(&result.LatestRun.Admitted, &result.LatestRun.Skipped, &result.LatestRun.Failed); err != nil {
			return DiscoveryDiagnostics{}, err
		}
		result.LatestRun.Skipped += invalidSkipped
	}
	switch status.Status {
	case "degraded":
		result.Reason = "upstream_unavailable"
	case "stale":
		result.Reason = "stale"
	case "updating":
		result.Reason = "backfill_in_progress"
	case "disabled":
		result.Reason = "disabled"
	}
	if err = tx.Commit(ctx); err != nil {
		return DiscoveryDiagnostics{}, err
	}
	return result, nil
}

func scanState(row dbgen.CatalogDiscoveryState) ScanState {
	return ScanState{Source: row.Source, Mode: DiscoveryMode(row.Mode), CursorAppID: uint32(row.CursorAppID), WatermarkAt: discoveryTimePointer(row.WatermarkAt), ScanStartedAt: discoveryTimePointer(row.ScanStartedAt), ScanCompletedAt: discoveryTimePointer(row.ScanCompletedAt), Availability: row.Availability, LastError: safeDiscoveryError(row.LastError)}
}
func discoveryTimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}
func discoveryTimePointer(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	at := t.Time.UTC()
	return &at
}
func safeDiscoveryError(value string) string {
	switch value {
	case "", "unavailable", "request_failed", "invalid_response", "rate_limited", "retry_exhausted":
		return value
	default:
		return "request_failed"
	}
}
