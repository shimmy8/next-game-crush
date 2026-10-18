package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
)

const (
	SearchQualificationJobKind        = "catalog.search.qualify"
	SearchQualificationPayloadVersion = 1
)

type SearchQualificationPayload struct {
	Version           int               `json:"version"`
	AppID             int64             `json:"app_id"`
	CandidateRevision CandidateRevision `json:"candidate_revision"`
	ReservationID     uuid.UUID         `json:"reservation_id"`
	ReservationToken  uuid.UUID         `json:"reservation_token"`
	RetryGeneration   int               `json:"retry_generation"`
	Priority          int               `json:"priority"`
}

type SearchDiscoveryEnrichmentPlanner interface {
	PlanCatalogGameFeatureJob(context.Context, uuid.UUID, string, int) (jobs.Spec, error)
}

func SearchQualificationKey(payload SearchQualificationPayload) string {
	return fmt.Sprintf("catalog.search.qualify:v1:%d:run:%d:observation:%d:reservation:%s:retry:%d",
		payload.AppID, payload.CandidateRevision.RunRevision, payload.CandidateRevision.ObservationOrdinal,
		payload.ReservationID, payload.RetryGeneration)
}

func validSearchQualificationPayload(payload SearchQualificationPayload) bool {
	return payload.Version == SearchQualificationPayloadVersion && payload.AppID > 0 && payload.AppID <= math.MaxUint32 &&
		payload.CandidateRevision.RunRevision > 0 && payload.CandidateRevision.ObservationOrdinal > 0 &&
		payload.ReservationID != uuid.Nil && payload.ReservationToken != uuid.Nil && payload.RetryGeneration >= 0 &&
		payload.Priority >= 1 && payload.Priority <= 100
}

func NewSearchQualificationSpec(payload SearchQualificationPayload, parent uuid.UUID) (jobs.Spec, error) {
	if !validSearchQualificationPayload(payload) {
		return jobs.Spec{}, errors.New("invalid search qualification payload")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return jobs.Spec{}, errors.New("invalid search qualification payload")
	}
	return jobs.Spec{
		Lane: jobs.LaneCatalog, Kind: SearchQualificationJobKind, Stage: "catalog_search_qualify",
		IdempotencyKey: SearchQualificationKey(payload), Payload: raw, Priority: payload.Priority,
		ParentJobID: parent, Optional: true, Required: false, MaxAttempts: 4,
	}, nil
}

func (s *DiscoveryService) SetSearchEnrichmentPlanner(planner SearchDiscoveryEnrichmentPlanner) {
	s.searchPlanner = planner
}

// ReserveCatalogFeature is the execution-time AI admission gate. It spends a
// current-hour semantic identity only for the worker that still owns the
// catalog feature lease; callers defer the same job when the bucket is full.
func (s *DiscoveryService) ReserveCatalogFeature(ctx context.Context, lease jobs.Lease, identity string, now time.Time) (bool, time.Time, error) {
	if s == nil || s.repo == nil || s.repo.store == nil || lease.Lane != jobs.LaneCatalog || lease.ID == uuid.Nil ||
		lease.FencingToken == uuid.Nil || strings.TrimSpace(identity) == "" || len(identity) > 512 || now.IsZero() {
		return false, time.Time{}, errors.New("invalid catalog AI admission")
	}
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, time.Time{}, err
	}
	defer tx.Rollback(ctx)
	var owned uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE id=$1 AND lane='catalog' AND status='running'
		AND worker_id=$2 AND fencing_token=$3 AND lease_until>clock_timestamp() FOR UPDATE`,
		lease.ID, lease.WorkerID, lease.FencingToken).Scan(&owned); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, jobs.ErrLeaseLost
		}
		return false, time.Time{}, err
	}
	granted, next, err := reserveBudgetWithQueries(ctx, s.repo.store.Queries.WithTx(tx), "ai", identity, s.cfg.AIHourlyBudget, now.UTC())
	if err != nil {
		return false, time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, time.Time{}, err
	}
	return granted, next, nil
}

func (s *DiscoveryService) RegisterSearchHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("search qualification worker is required")
	}
	return worker.Register(SearchQualificationJobKind, s.handleSearchQualification)
}

func (s *DiscoveryService) handleSearchQualification(ctx context.Context, lease jobs.Lease) error {
	var payload SearchQualificationPayload
	if strictDiscoveryPayload(lease.Payload, &payload) != nil || !validSearchQualificationPayload(payload) ||
		lease.IdempotencyKey != SearchQualificationKey(payload) || lease.Lane != jobs.LaneCatalog || lease.MaxAttempts != 4 {
		return errors.New("invalid search qualification payload")
	}
	details, err := s.metadata.GameDetails(ctx, payload.AppID)
	if err != nil {
		reason := "request_failed"
		if errors.Is(err, steam.ErrUnavailable) {
			reason = "unavailable"
		}
		if persistErr := s.repo.persistSearchQualificationFailure(ctx, lease, payload, reason, s.now().UTC()); persistErr != nil {
			return persistErr
		}
		return jobs.NewCatalogRetryError(err, reason, retryAfterFromError(err))
	}
	if details.AppID != payload.AppID || strings.TrimSpace(details.Name) == "" || strings.TrimSpace(details.Type) == "" ||
		(!details.ReleaseDate.ComingSoon && strings.TrimSpace(details.ReleaseDate.Raw) == "") {
		if err := s.repo.persistSearchQualificationFailure(ctx, lease, payload, "malformed", s.now().UTC()); err != nil {
			return err
		}
		return jobs.NewCatalogRetryError(steam.ErrInvalidResponse, "malformed", nil)
	}
	return s.repo.persistSearchQualification(ctx, lease, payload, details, s.now().UTC(), s.cfg, s.searchPlanner)
}

func retryAfterFromError(err error) *time.Duration {
	type hinted interface{ RetryAfter() (time.Duration, bool) }
	var value hinted
	if errors.As(err, &value) {
		if delay, ok := value.RetryAfter(); ok && delay > 0 {
			return &delay
		}
	}
	var request *steam.RequestError
	if errors.As(err, &request) && request.RetryAfter > 0 {
		return &request.RetryAfter
	}
	return nil
}

// SemanticContentHash is the canonical AI identity. Product phase, release,
// reviews, price, artwork and fetch state deliberately do not affect it.
func SemanticContentHash(details steam.GameDetails) string { return steam.GameContentHash(details) }
