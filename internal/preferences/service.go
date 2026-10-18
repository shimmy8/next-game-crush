package preferences

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

type Service struct {
	repository *SQLRepository
	queue      *jobs.Queue
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
func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
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
			ready, err := s.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Kind: stage.kind, Stage: stage.stage, UserID: workflow.UserID, Priority: 100, Required: true, IdempotencyKey: prefix + stage.key, Payload: childPayload, MaxAttempts: 3}})
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
