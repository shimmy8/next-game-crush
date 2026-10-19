package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/steam"
)

const (
	QualifyJobKind     = "catalog.qualify"
	TickJobKind        = "catalog.discovery.tick"
	IncrementalJobKind = "catalog.discover.incremental"
	BackfillJobKind    = "catalog.discover.backfill"
	RebuildWakeJobKind = "catalog.rebuild_wakeup"
)

type QualificationPayload struct {
	Version         int           `json:"version"`
	App             DiscoveredApp `json:"app"`
	RetryGeneration int           `json:"retry_generation"`
	BudgetHour      time.Time     `json:"budget_hour"`
	Priority        int           `json:"priority"`
}
type DiscoveryEnrichmentPlanner interface {
	PlanGameFeatureJob(context.Context, uuid.UUID, string, int) (jobs.Spec, error)
}
type DiscoveryService struct {
	repo          *DiscoveryRepository
	source        DiscoverySource
	metadata      steam.Client
	queue         *jobs.Queue
	cfg           DiscoveryConfig
	planner       DiscoveryEnrichmentPlanner
	searchPlanner SearchDiscoveryEnrichmentPlanner
	now           func() time.Time
	logger        *observability.Logger
}

func NewDiscoveryService(repo *DiscoveryRepository, source DiscoverySource, metadata steam.Client, queue *jobs.Queue, cfg DiscoveryConfig, logger *observability.Logger) (*DiscoveryService, error) {
	if repo == nil || repo.store == nil || source == nil || metadata == nil || queue == nil {
		return nil, errors.New("discovery dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = observability.New(nil)
	}
	return &DiscoveryService{repo: repo, source: source, metadata: metadata, queue: queue, cfg: cfg, logger: logger, now: time.Now}, nil
}
func (s *DiscoveryService) SetEnrichmentPlanner(p DiscoveryEnrichmentPlanner) { s.planner = p }
func (s *DiscoveryService) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("discovery worker is required")
	}
	for _, h := range []struct {
		kind    string
		handler jobs.Handler
	}{{QualifyJobKind, s.handleQualification}, {TickJobKind, s.handleTick}, {IncrementalJobKind, s.handleScan}, {BackfillJobKind, s.handleScan}, {RebuildWakeJobKind, s.handleRebuildWakeup}} {
		if err := worker.Register(h.kind, h.handler); err != nil {
			return err
		}
	}
	return nil
}
func strictDiscoveryPayload(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return errors.New("invalid discovery payload")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("invalid discovery payload")
	}
	return nil
}
func validDiscoveredApp(a DiscoveredApp) bool {
	return a.AppID > 0 && a.AppID <= math.MaxUint32 && !a.LastModified.IsZero() && a.LastModified.Unix() > 0 && a.LastModified.Unix() <= math.MaxUint32 && a.LastModified.Equal(a.LastModified.Truncate(time.Second))
}
func qualificationKey(app DiscoveredApp, generation int) string {
	return fmt.Sprintf("catalog.qualify:v1:%d:%d:retry:%d", app.AppID, app.LastModified.Unix(), generation)
}
func qualificationSpec(payload QualificationPayload, parent uuid.UUID) jobs.Spec {
	raw, _ := json.Marshal(payload)
	return jobs.Spec{Lane: jobs.LaneInteractive, Kind: QualifyJobKind, Stage: "catalog_qualify", IdempotencyKey: qualificationKey(payload.App, payload.RetryGeneration), Payload: raw, Priority: payload.Priority, ParentJobID: parent, Optional: true, Required: false, MaxAttempts: 3}
}
func (s *DiscoveryService) handleQualification(ctx context.Context, lease jobs.Lease) error {
	if !s.cfg.Enabled {
		return nil
	}
	var p QualificationPayload
	if strictDiscoveryPayload(lease.Payload, &p) != nil || p.Version != 1 || !validDiscoveredApp(p.App) || p.RetryGeneration < 0 || p.RetryGeneration > math.MaxInt32 || p.BudgetHour.IsZero() || !p.BudgetHour.Equal(p.BudgetHour.UTC().Truncate(time.Hour)) || lease.IdempotencyKey != qualificationKey(p.App, p.RetryGeneration) || (p.Priority != 10 && p.Priority != 50 && p.Priority != 80) {
		return errors.New("invalid qualification payload")
	}
	details, err := s.metadata.GameDetails(ctx, p.App.AppID)
	status := ""
	if errors.Is(err, steam.ErrUnavailable) {
		status = "unavailable"
	} else if err != nil {
		return errors.New("discovery metadata request failed")
	}
	if status == "" && (details.AppID != p.App.AppID || strings.TrimSpace(details.Name) == "" || strings.TrimSpace(details.Type) == "" || (!details.ReleaseDate.ComingSoon && strings.TrimSpace(details.ReleaseDate.Raw) == "")) {
		return errors.New("invalid discovery metadata")
	}
	qualification := Qualification{Status: status}
	if status == "" {
		qualification = Qualify(p.App, details, s.now().UTC(), s.cfg)
		if qualification.Status == "failed" {
			return errors.New("invalid discovery metadata")
		}
	}
	var deck *deckCompatibilityObservation
	if qualification.Status == "admitted" {
		observation := fetchDeckCompatibility(ctx, s.metadata, p.App.AppID)
		deck = &observation
	}
	if err := s.repo.persistQualification(ctx, lease, p, details, qualification, deck, s.now().UTC(), s.planner); err != nil {
		return err
	}
	return s.ScheduleRebuilds(ctx)
}

// Price, release window, platform, availability and observed timestamps are not
// semantic evidence. Their refresh must not create another feature identity.
func discoveryContentHash(details steam.GameDetails) string {
	return steam.GameContentHash(details)
}

type tickPayload struct {
	Version     int       `json:"version"`
	ScheduledAt time.Time `json:"scheduled_at"`
}

func tickSpec(at time.Time) jobs.Spec {
	at = at.UTC().Truncate(time.Hour)
	raw, _ := json.Marshal(tickPayload{1, at})
	// Migration 17 retains retired v1 keys on the catalog lane. A separate
	// generation keeps the hourly interactive chain usable without changing
	// those audit rows or bypassing the queue's lane-conflict protection.
	return jobs.Spec{Lane: jobs.LaneInteractive, Kind: TickJobKind, Stage: "catalog_discovery_tick", IdempotencyKey: fmt.Sprintf("catalog.discovery.tick:legacy-interactive:v2:%d", at.Unix()), Payload: raw, AvailableAt: at, Priority: 50, MaxAttempts: 3}
}

// Every coordinator derives the same interval slot, including nudges in the
// intervening hours. Successors therefore form one durable chain on restart.
func discoveryTickSlot(now time.Time, intervalHours int) time.Time {
	return now.UTC().Truncate(time.Duration(intervalHours) * time.Hour)
}
func (s *DiscoveryService) EnsureScheduled(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	slot := discoveryTickSlot(s.now(), s.cfg.IntervalHours)
	_, err := s.enqueueLegacy(ctx, tickSpec(slot), tickSpec(slot.Add(time.Duration(s.cfg.IntervalHours)*time.Hour)))
	return err
}
func (s *DiscoveryService) Nudge(ctx context.Context) error { return s.EnsureScheduled(ctx) }

func (s *DiscoveryService) enqueueLegacy(ctx context.Context, specs ...jobs.Spec) (bool, error) {
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	allowed, err := s.repo.LegacyEnqueueAllowedForUpdate(ctx, tx)
	if err != nil || !allowed {
		if err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	q := s.repo.store.Queries.WithTx(tx)
	for _, spec := range specs {
		if _, err := jobs.EnqueueInTx(ctx, q, spec); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (s *DiscoveryService) deferLegacy(ctx context.Context, lease jobs.Lease, at time.Time) error {
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	allowed, err := s.repo.LegacyEnqueueAllowedForUpdate(ctx, tx)
	if err != nil {
		return err
	}
	if !allowed {
		return legacySupersededError{}
	}
	if err := s.queue.DeferUntilInTx(ctx, tx, lease, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *DiscoveryService) DiscoveryStatus(ctx context.Context) (PresentationStatus, error) {
	return s.repo.PresentationStatus(ctx, s.cfg.Enabled, s.now().UTC(), time.Duration(s.cfg.IntervalHours)*time.Hour)
}
func (s *DiscoveryService) Diagnostics(ctx context.Context) (DiscoveryDiagnostics, error) {
	return s.repo.Diagnostics(ctx, s.cfg.Enabled, s.now().UTC(), time.Duration(s.cfg.IntervalHours)*time.Hour)
}
func (s *DiscoveryService) handleTick(ctx context.Context, lease jobs.Lease) error {
	startedAt := time.Now()
	if !s.cfg.Enabled {
		return nil
	}
	var p tickPayload
	if strictDiscoveryPayload(lease.Payload, &p) != nil || p.Version != 1 || p.ScheduledAt.IsZero() || !p.ScheduledAt.Equal(p.ScheduledAt.UTC().Truncate(time.Hour)) || lease.IdempotencyKey != tickSpec(p.ScheduledAt).IdempotencyKey {
		return errors.New("invalid discovery tick payload")
	}
	now := s.now().UTC()
	next := discoveryTickSlot(now, s.cfg.IntervalHours).Add(time.Duration(s.cfg.IntervalHours) * time.Hour)
	allowed, err := s.enqueueLegacy(ctx, tickSpec(next))
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	if err := s.repo.reconcileDeadRetries(ctx, now); err != nil {
		return err
	}
	if _, err := s.repo.ScheduleDueRetries(ctx, now, s.cfg); err != nil {
		return err
	}
	if err := s.ScheduleRebuilds(ctx); err != nil {
		return err
	}
	scheduled, err := s.repo.scheduleScan(ctx, DiscoveryIncremental, now, s.cfg)
	if err != nil {
		return err
	}
	if !scheduled {
		state, e := s.repo.State(ctx, DiscoveryIncremental)
		if e != nil {
			return e
		}
		if state.ScanStartedAt != nil && state.ScanCompletedAt != nil && !state.ScanCompletedAt.Before(*state.ScanStartedAt) {
			_, err = s.repo.scheduleScan(ctx, DiscoveryBackfill, now, s.cfg)
		}
	}
	if err != nil {
		return err
	}
	if diagnostics, diagnosticErr := s.Diagnostics(ctx); diagnosticErr == nil {
		state, stateErr := s.repo.State(ctx, DiscoveryIncremental)
		if stateErr == nil {
			reason := observability.Reason("")
			if diagnostics.Status == "degraded" {
				reason = observability.ReasonUpstreamUnavailable
			}
			s.logger.CatalogDiscovery(ctx, observability.CatalogDiscoveryEvent{
				Correlation: observability.CorrelationFromContext(ctx), Source: "steam", Mode: string(DiscoveryIncremental), Availability: state.Availability,
				Discovered: int(diagnostics.LatestRun.Discovered), Evaluated: int(diagnostics.LatestRun.Evaluated), Admitted: int(diagnostics.LatestRun.Admitted),
				Skipped: int(diagnostics.LatestRun.Skipped), Failed: int(diagnostics.LatestRun.Failed), Pending: int(diagnostics.PendingQualifications), Duration: time.Since(startedAt), Reason: reason,
			})
		}
	}
	return nil
}

func discoveryRetryDelay(generation int) time.Duration {
	if generation >= 5 {
		return 24 * time.Hour
	}
	if generation < 0 {
		generation = 0
	}
	delay := time.Hour * time.Duration(1<<generation)
	if delay > 24*time.Hour {
		return 24 * time.Hour
	}
	return delay
}
