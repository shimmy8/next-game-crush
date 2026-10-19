package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	dbgen "next-game-crush/internal/store/sqlc"
)

type ScanPayload struct {
	Version        int           `json:"version"`
	Mode           DiscoveryMode `json:"mode"`
	ScanStartedAt  time.Time     `json:"scan_started_at"`
	AfterAppID     uint32        `json:"after_app_id"`
	BudgetHour     time.Time     `json:"budget_hour"`
	PageGeneration int           `json:"page_generation"`
}
type PageManifest struct {
	Version       int           `json:"version"`
	Mode          DiscoveryMode `json:"mode"`
	ScanStartedAt time.Time     `json:"scan_started_at"`
	AfterAppID    uint32        `json:"after_app_id"`
	NextAppID     uint32        `json:"next_app_id"`
	HasMore       bool          `json:"has_more"`
	Items         []PageItem    `json:"items"`
}
type PageItem struct {
	App             DiscoveredApp `json:"app"`
	Ordinal         int           `json:"ordinal"`
	JobID           uuid.UUID     `json:"job_id"`
	RetryGeneration int           `json:"retry_generation"`
	Invalid         bool          `json:"invalid"`
}

var errDiscoveryBudget = errors.New("discovery hourly budget exhausted")

func scanKey(p ScanPayload) string {
	return fmt.Sprintf("catalog.discover.%s:v1:%d:after:%d:retry:%d", p.Mode, p.ScanStartedAt.Unix(), p.AfterAppID, p.PageGeneration)
}
func scanPriority(mode DiscoveryMode) int {
	if mode == DiscoveryBackfill {
		return 10
	}
	return 50
}
func validScanPayload(p ScanPayload) bool {
	return p.Version == 1 && validDiscoveryMode(p.Mode) && !p.ScanStartedAt.IsZero() && !p.BudgetHour.IsZero() && p.BudgetHour.Equal(p.BudgetHour.UTC().Truncate(time.Hour)) && p.PageGeneration >= 0 && p.PageGeneration <= math.MaxInt32
}

func (s *DiscoveryService) handleScan(ctx context.Context, lease jobs.Lease) error {
	if !s.cfg.Enabled {
		return nil
	}
	var p ScanPayload
	if strictDiscoveryPayload(lease.Payload, &p) != nil || !validScanPayload(p) || lease.IdempotencyKey != scanKey(p) || (p.Mode == DiscoveryIncremental && lease.Kind != IncrementalJobKind) || (p.Mode == DiscoveryBackfill && lease.Kind != BackfillJobKind) {
		return errors.New("invalid discovery scan payload")
	}
	var stored []byte
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT result FROM sync_jobs WHERE id=$1`, lease.ID).Scan(&stored); err != nil {
		return err
	}
	var manifest PageManifest
	if string(stored) != "{}" && len(stored) > 0 {
		if strictDiscoveryPayload(stored, &manifest) != nil || manifest.Version != 1 {
			return errors.New("invalid discovery page manifest")
		}
	} else {
		now := s.now().UTC()
		if p.BudgetHour.Before(now.Truncate(time.Hour)) {
			p.BudgetHour = now.Truncate(time.Hour)
		}
		remaining, err := s.repo.remainingBudget(ctx, p.BudgetHour, s.cfg.HourlyBudget)
		if err != nil {
			return err
		}
		if remaining == 0 { // No request is spent after another coordinator used the slot.
			if err = s.deferLegacy(ctx, lease, nextDiscoveryHour(now)); err != nil {
				return err
			}
			return jobs.ErrDeferred
		}
		limit := min(s.cfg.PageSize, remaining)
		state, err := s.repo.State(ctx, p.Mode)
		if err != nil {
			return err
		}
		var since time.Time
		if p.Mode == DiscoveryIncremental {
			if state.WatermarkAt == nil {
				return errors.New("missing discovery watermark")
			}
			since = state.WatermarkAt.Add(-5 * time.Minute)
		}
		page, err := s.source.ListChangedGames(ctx, since, p.AfterAppID, limit)
		if err != nil {
			if markErr := s.repo.markScanFailure(ctx, lease, p); markErr != nil {
				return markErr
			}
			return errors.New("discovery source request failed")
		}
		if len(page.Apps) > limit {
			return errors.New("invalid discovery page size")
		}
		manifest, err = s.repo.schedulePage(ctx, lease, p, page, s.cfg, s.planner)
		if errors.Is(err, errDiscoveryBudget) {
			if err = s.deferLegacy(ctx, lease, nextDiscoveryHour(now)); err != nil {
				return err
			}
			return jobs.ErrDeferred
		}
		if err != nil {
			return err
		}
	}
	ready, _, err := s.repo.ReconcilePage(ctx, lease, manifest, s.now().UTC())
	if err != nil {
		return err
	}
	if !ready {
		if err = s.deferLegacy(ctx, lease, time.Now().UTC().Add(5*time.Second)); err != nil {
			return err
		}
		return jobs.ErrDeferred
	}
	if p.Mode == DiscoveryIncremental {
		mode := DiscoveryIncremental
		if !manifest.HasMore {
			mode = DiscoveryBackfill
		}
		_, err = s.repo.scheduleScan(ctx, mode, s.now().UTC(), s.cfg)
	}
	return err
}
func nextDiscoveryHour(now time.Time) time.Time {
	at := now.UTC().Truncate(time.Hour).Add(time.Hour)
	if !at.After(time.Now()) {
		at = time.Now().UTC().Add(time.Hour)
	}
	return at
}

func (r *DiscoveryRepository) reserveInTx(ctx context.Context, tx pgx.Tx, hour time.Time, identity string, limit int) (bool, error) {
	q := r.store.Queries.WithTx(tx)
	hour = hour.UTC().Truncate(time.Hour)
	at := discoveryTimestamp(hour)
	if err := q.EnsureDiscoveryBudget(ctx, at); err != nil {
		return false, err
	}
	row, err := q.LockDiscoveryBudget(ctx, at)
	if err != nil {
		return false, err
	}
	var reservations map[string]bool
	if json.Unmarshal(row.Reservations, &reservations) != nil {
		return false, errors.New("invalid discovery budget state")
	}
	hash := sha256.Sum256([]byte(identity))
	key := hex.EncodeToString(hash[:])
	if reservations[key] {
		return true, nil
	}
	if int(row.Used) >= limit {
		return false, nil
	}
	return true, q.ReserveDiscoveryBudget(ctx, dbgen.ReserveDiscoveryBudgetParams{HourAt: at, IdentityHash: key})
}
func (r *DiscoveryRepository) remainingBudget(ctx context.Context, hour time.Time, limit int) (int, error) {
	var used int
	err := r.store.Pool.QueryRow(ctx, `SELECT used FROM catalog_discovery_budget WHERE hour_at=$1`, hour.UTC().Truncate(time.Hour)).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return limit, nil
	}
	return max(0, limit-used), err
}

func (r *DiscoveryRepository) SchedulePage(ctx context.Context, lease jobs.Lease, p ScanPayload, page DiscoveryPage, cfg DiscoveryConfig) (PageManifest, error) {
	return r.schedulePage(ctx, lease, p, page, cfg, nil)
}

func (r *DiscoveryRepository) schedulePage(ctx context.Context, lease jobs.Lease, p ScanPayload, page DiscoveryPage, cfg DiscoveryConfig, planner DiscoveryEnrichmentPlanner) (PageManifest, error) {
	manifest := PageManifest{Version: 1, Mode: p.Mode, ScanStartedAt: p.ScanStartedAt, AfterAppID: p.AfterAppID, NextAppID: page.NextAppID, HasMore: page.HasMore, Items: []PageItem{}}
	if !validScanPayload(p) || cfg.Validate() != nil || len(page.Apps) > cfg.PageSize || page.NextAppID < p.AfterAppID || (page.HasMore && (len(page.Apps) == 0 || page.NextAppID <= p.AfterAppID)) {
		return manifest, errors.New("invalid discovery page")
	}
	for _, a := range page.Apps {
		if a.AppID > int64(page.NextAppID) && page.HasMore {
			return manifest, errors.New("invalid discovery cursor")
		}
	}
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return manifest, err
	}
	defer tx.Rollback(ctx)
	allowed, err := r.LegacyEnqueueAllowedForUpdate(ctx, tx)
	if err != nil {
		return manifest, err
	}
	if !allowed {
		return manifest, legacySupersededError{}
	}
	stored, err := lockDiscoveryLease(ctx, tx, lease)
	if err != nil {
		return manifest, err
	}
	if len(stored) > 0 && string(stored) != "{}" {
		if strictDiscoveryPayload(stored, &manifest) != nil || manifest.Version != 1 {
			return manifest, errors.New("invalid discovery page manifest")
		}
		return manifest, commitDiscoveryLease(ctx, tx, lease)
	}
	q := r.store.Queries.WithTx(tx)
	state, err := q.LockCatalogDiscoveryState(ctx, string(p.Mode))
	if err != nil {
		return manifest, err
	}
	if !state.ScanStartedAt.Valid || !state.ScanStartedAt.Time.Equal(p.ScanStartedAt) || uint32(state.CursorAppID) != p.AfterAppID {
		return manifest, errors.New("discovery checkpoint changed")
	}
	// Lock the hourly row before observation rows to make retry/page reservations
	// use a single lock order across concurrent coordinators.
	if err = q.EnsureDiscoveryBudget(ctx, discoveryTimestamp(p.BudgetHour.UTC().Truncate(time.Hour))); err != nil {
		return manifest, err
	}
	if _, err = q.LockDiscoveryBudget(ctx, discoveryTimestamp(p.BudgetHour.UTC().Truncate(time.Hour))); err != nil {
		return manifest, err
	}
	for ordinal, app := range page.Apps {
		app.Name = "" // Item names never enter durable queue diagnostics/manifests.
		item := PageItem{App: app, Ordinal: ordinal, Invalid: !validDiscoveredApp(app)}
		if item.Invalid {
			manifest.Items = append(manifest.Items, item)
			continue
		}
		// Do not hold the observation lock while idempotent enqueue may wait on
		// a running qualification: that writer owns its job before observation.
		observation, e := q.GetDiscoveryObservation(ctx, dbgen.GetDiscoveryObservationParams{SteamAppID: app.AppID, LastModified: discoveryTimestamp(app.LastModified.Truncate(time.Second))})
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return manifest, e
		}
		if e == nil {
			item.RetryGeneration = int(observation.RetryGeneration)
			if observation.Status == "failed" { // The coordinator owns durable retry generations.
				manifest.Items = append(manifest.Items, item)
				continue
			}
			policyNow := Qualify(app, steam.GameDetails{AppID: app.AppID, Type: "game", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: observation.RawReleaseText}}, p.ScanStartedAt, cfg)
			policySensitive := observation.Status == "unavailable" || (observation.Status == "outside_horizon" && policyNow.Status == "admitted") || (observation.Status == "admitted" && policyNow.Status != "admitted")
			if p.Mode != DiscoveryBackfill || !policySensitive || !observation.LastAttemptAt.Time.Before(p.ScanStartedAt) {
				if observation.Status == "admitted" && policyNow.Status == "admitted" && planner != nil {
					if err = r.enqueueCachedDiscoveryFeatures(ctx, tx, q, planner, app, scanPriority(p.Mode)); err != nil {
						return manifest, err
					}
				}
				manifest.Items = append(manifest.Items, item)
				continue
			}
			item.RetryGeneration++
		}
		payload := QualificationPayload{Version: 1, App: app, RetryGeneration: item.RetryGeneration, BudgetHour: p.BudgetHour, Priority: scanPriority(p.Mode)}
		ok, e := r.reserveInTx(ctx, tx, p.BudgetHour, qualificationKey(app, item.RetryGeneration), cfg.HourlyBudget)
		if e != nil {
			return manifest, e
		}
		if !ok {
			return manifest, errDiscoveryBudget
		}
		item.JobID, err = jobs.EnqueueInTx(ctx, q, qualificationSpec(payload, lease.ID))
		if err != nil {
			return manifest, err
		}
		manifest.Items = append(manifest.Items, item)
	}
	raw, _ := json.Marshal(manifest)
	payload, _ := json.Marshal(p)
	if _, err = tx.Exec(ctx, `UPDATE sync_jobs SET result=$2,payload=$3 WHERE id=$1`, lease.ID, raw, payload); err != nil {
		return manifest, err
	}
	return manifest, commitDiscoveryLease(ctx, tx, lease)
}

func (r *DiscoveryRepository) ReconcilePage(ctx context.Context, lease jobs.Lease, manifest PageManifest, now time.Time) (bool, bool, error) {
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	allowed, err := r.LegacyEnqueueAllowedForUpdate(ctx, tx)
	if err != nil {
		return false, false, err
	}
	if !allowed {
		return false, false, legacySupersededError{}
	}
	stored, err := lockDiscoveryLease(ctx, tx, lease)
	if err != nil {
		return false, false, err
	}
	// Only the committed manifest is authoritative, including foreign-parent IDs.
	var persisted PageManifest
	if strictDiscoveryPayload(stored, &persisted) != nil || persisted.Version != 1 {
		return false, false, errors.New("invalid discovery page manifest")
	}
	a, _ := json.Marshal(manifest)
	b, _ := json.Marshal(persisted)
	if string(a) != string(b) {
		return false, false, errors.New("discovery page manifest mismatch")
	}
	q := r.store.Queries.WithTx(tx)
	state, err := q.LockCatalogDiscoveryState(ctx, string(manifest.Mode))
	if err != nil {
		return false, false, err
	}
	if !state.ScanStartedAt.Valid || !state.ScanStartedAt.Time.Equal(manifest.ScanStartedAt) {
		return false, false, errors.New("discovery checkpoint changed")
	}
	if state.ScanCompletedAt.Valid && !state.ScanCompletedAt.Time.Before(manifest.ScanStartedAt) || uint32(state.CursorAppID) != manifest.AfterAppID {
		return true, false, commitDiscoveryLease(ctx, tx, lease)
	}
	var budgetHour time.Time
	if err = tx.QueryRow(ctx, `SELECT (payload->>'budget_hour')::timestamptz FROM sync_jobs WHERE id=$1`, lease.ID).Scan(&budgetHour); err != nil {
		return false, false, err
	}
	if _, err = q.LockDiscoveryBudget(ctx, discoveryTimestamp(budgetHour.UTC().Truncate(time.Hour))); err != nil {
		return false, false, err
	}
	for _, item := range manifest.Items {
		if item.Invalid || item.JobID == uuid.Nil {
			continue
		}
		var status string
		var attempts int
		if err = tx.QueryRow(ctx, `SELECT status,attempts FROM sync_jobs WHERE id=$1 FOR UPDATE`, item.JobID).Scan(&status, &attempts); err != nil {
			return false, false, err
		}
		if status == "running" {
			tag, e := tx.Exec(ctx, `UPDATE sync_jobs SET status='dead',worker_id=NULL,fencing_token=NULL,lease_until=NULL,last_error='retry_exhausted',updated_at=now() WHERE id=$1 AND lease_until<=now() AND attempts>=max_attempts`, item.JobID)
			if e != nil {
				return false, false, e
			}
			if tag.RowsAffected() > 0 {
				status = "dead"
				if _, e = tx.Exec(ctx, `UPDATE job_attempts SET status='expired',finished_at=now(),error='lease expired' WHERE job_id=$1 AND status='running'`, item.JobID); e != nil {
					return false, false, e
				}
			}
		}
		if status != "succeeded" && status != "dead" {
			return false, false, commitDiscoveryLease(ctx, tx, lease)
		}
		observation, e := q.LockDiscoveryObservation(ctx, dbgen.LockDiscoveryObservationParams{SteamAppID: item.App.AppID, LastModified: discoveryTimestamp(item.App.LastModified.Truncate(time.Second))})
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return false, false, e
		}
		if e == nil && int(observation.RetryGeneration) >= item.RetryGeneration && observation.Status != "failed" {
			continue
		}
		if e == nil && observation.Status == "failed" && int(observation.RetryGeneration) == item.RetryGeneration && observation.NextRetryAt.Valid {
			continue
		}
		next := now.Add(discoveryRetryDelay(item.RetryGeneration))
		if e == nil {
			attempts += int(observation.Attempts)
		}
		if err = q.UpsertDiscoveryObservation(ctx, dbgen.UpsertDiscoveryObservationParams{SteamAppID: item.App.AppID, LastModified: discoveryTimestamp(item.App.LastModified.Truncate(time.Second)), Status: "failed", Attempts: int32(max(1, attempts)), RetryGeneration: int32(item.RetryGeneration), NextRetryAt: discoveryTimestamp(next), SafeError: "retry_exhausted"}); err != nil {
			return false, false, err
		}
	}
	invalidItems := 0
	for _, item := range manifest.Items {
		if item.Invalid {
			invalidItems++
		}
	}
	cursor := manifest.NextAppID
	if !manifest.HasMore {
		cursor = 0
	}
	if _, err = tx.Exec(ctx, `UPDATE catalog_discovery_state SET cursor_app_id=$2,scan_completed_at=CASE WHEN $3 THEN scan_completed_at ELSE $4 END,watermark_at=CASE WHEN mode='incremental' AND NOT $3 THEN scan_started_at ELSE watermark_at END,availability='available',last_error='',updated_at=$4 WHERE source='steam' AND mode=$1`, string(manifest.Mode), int64(cursor), manifest.HasMore, now); err != nil {
		return false, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE catalog_discovery_budget SET counts=jsonb_set(
		jsonb_set(counts,'{evaluated}',to_jsonb(COALESCE((counts->>'evaluated')::integer,0)+$2::integer)),
		'{skipped_invalid}',to_jsonb(COALESCE((counts->>'skipped_invalid')::integer,0)+$3::integer)
	) WHERE hour_at=$1`, budgetHour.UTC().Truncate(time.Hour), len(manifest.Items), invalidItems); err != nil {
		return false, false, err
	}
	return true, true, commitDiscoveryLease(ctx, tx, lease)
}
func (r *DiscoveryRepository) markScanFailure(ctx context.Context, lease jobs.Lease, p ScanPayload) error {
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = lockDiscoveryLease(ctx, tx, lease); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE catalog_discovery_state SET availability='request_failed',last_error='request_failed',updated_at=now() WHERE source='steam' AND mode=$1 AND scan_started_at=$2 AND cursor_app_id=$3`, string(p.Mode), p.ScanStartedAt, int64(p.AfterAppID)); err != nil {
		return err
	}
	return commitDiscoveryLease(ctx, tx, lease)
}
