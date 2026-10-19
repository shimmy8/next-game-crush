package preferences

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
)

type Service struct {
	repository *SQLRepository
	queue      *jobs.Queue
}

type OverlayJobPayload struct {
	Version int       `json:"version"`
	UserID  uuid.UUID `json:"user_id"`
}

func NewService(repository *SQLRepository, queue *jobs.Queue) (*Service, error) {
	if repository == nil || queue == nil {
		return nil, errors.New("preference repository and queue are required")
	}
	return &Service{repository: repository, queue: queue}, nil
}
func (s *Service) List(ctx context.Context, userID uuid.UUID, includeInactive bool) ([]Adjustment, error) {
	return s.repository.List(ctx, userID, includeInactive)
}
func (s *Service) Disable(ctx context.Context, userID, adjustmentID uuid.UUID) (DisableAccepted, error) {
	return s.repository.DisableAndEnqueue(ctx, userID, adjustmentID)
}
func (s *Service) EnsureCurrent(ctx context.Context, userID uuid.UUID) (Overlay, error) {
	return s.repository.RebuildOverlay(ctx, userID, "")
}
func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	if err := worker.Register(OverlayJobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload OverlayJobPayload
		if json.Unmarshal(lease.Payload, &payload) != nil || payload.Version != 1 || payload.UserID == uuid.Nil || lease.UserID != payload.UserID {
			return errors.New("unsupported preference overlay job payload schema")
		}
		overlay, err := s.repository.OverlayByBuildKey(ctx, payload.UserID, lease.IdempotencyKey)
		if errors.Is(err, pgx.ErrNoRows) {
			overlay, err = s.repository.RebuildOverlay(ctx, payload.UserID, lease.IdempotencyKey)
		}
		if err != nil {
			return err
		}
		return s.queue.RecordResult(ctx, lease, map[string]any{"preferenceOverlayId": overlay.ID, "preferenceOverlayVersion": overlay.Version, "inputContentHash": overlay.InputContentHash})
	}); err != nil {
		return err
	}
	return worker.Register(DisableJobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload DisablePayload
		if json.Unmarshal(lease.Payload, &payload) != nil || payload.Version != 1 || payload.AdjustmentID == uuid.Nil {
			return errors.New("unsupported preference job payload schema")
		}
		workflow, err := s.repository.DisableWorkflow(ctx, payload.AdjustmentID)
		if err != nil {
			return err
		}
		if workflow.JobID != lease.ID {
			return errors.New("preference workflow mismatch")
		}
		childPayload, _ := json.Marshal(map[string]any{"version": 1, "user_id": workflow.UserID})
		prefix := "adjustment:" + workflow.AdjustmentID.String() + ":disable:"
		for _, stage := range []struct{ kind, stage, key string }{{"profile.rebuild", "profile", "profile:v1"}, {"recommendations.rebuild", "recommendations", "ranking:v1"}} {
			ready, err := s.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Lane: jobs.LaneInteractive, Kind: stage.kind, Stage: stage.stage, UserID: workflow.UserID, Priority: 100, Required: true, IdempotencyKey: prefix + stage.key, Payload: childPayload, MaxAttempts: 3}})
			if err != nil {
				return err
			}
			if !ready {
				return jobs.ErrDeferred
			}
		}
		version, err := s.repository.DisableResult(ctx, workflow)
		if err != nil {
			return err
		}
		return s.queue.RecordResult(ctx, lease, map[string]any{"rankingVersion": version, "previousRankingVersion": workflow.PreviousRankingVersion})
	})
}
