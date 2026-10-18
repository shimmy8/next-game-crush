package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/completion"
	"next-game-crush/internal/jobs"
)

const (
	AlgorithmVersion           = "profile-v4-achievement-completion"
	CompletionAlgorithmVersion = completion.ProjectionAlgorithmVersion
	JobKind                    = "profile.rebuild"
	JobPayloadVersion          = 1
)

type SummaryStatus string

const (
	SummaryReady       SummaryStatus = "ready"
	SummaryUnavailable SummaryStatus = "unavailable"
)

type Profile struct {
	ID               uuid.UUID     `json:"id"`
	UserID           uuid.UUID     `json:"-"`
	Version          int64         `json:"version"`
	Summary          string        `json:"summary"`
	SummaryStatus    SummaryStatus `json:"summaryStatus"`
	PositiveStrength float64       `json:"-"`
	Confidence       float64       `json:"confidence"`
	EvidenceCount    int           `json:"evidenceCount"`
	Positive         []float32     `json:"-"`
	Negative         []float32     `json:"-"`
	CreatedAt        time.Time     `json:"createdAt"`
}

func WithSummaryState(value Profile) Profile {
	if strings.TrimSpace(value.Summary) == "" {
		value.Summary = ""
		value.SummaryStatus = SummaryUnavailable
		return value
	}
	value.SummaryStatus = SummaryReady
	return value
}

type Repository interface {
	LoadSignals(context.Context, uuid.UUID) ([]Signal, error)
	SaveProfile(context.Context, uuid.UUID, Vectors, string, string) (Profile, error)
	LatestProfile(context.Context, uuid.UUID) (Profile, error)
}

type Service struct {
	repository Repository
	queue      *jobs.Queue
	now        func() time.Time
}

func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, errors.New("profile repository is required")
	}
	return &Service{repository: repository, now: time.Now}, nil
}
func (s *Service) SetQueue(queue *jobs.Queue) { s.queue = queue }
func (s *Service) Rebuild(ctx context.Context, userID uuid.UUID) (Profile, error) {
	if userID == uuid.Nil {
		return Profile{}, errors.New("user ID is required")
	}
	signals, err := s.repository.LoadSignals(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("load profile signals: %w", err)
	}
	return s.repository.SaveProfile(ctx, userID, BuildVectors(signals, s.now().UTC()), AlgorithmVersion, uuid.NewString())
}

func (s *Service) rebuildWithKey(ctx context.Context, userID uuid.UUID, buildKey string) (Profile, error) {
	signals, err := s.repository.LoadSignals(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("load profile signals: %w", err)
	}
	return s.repository.SaveProfile(ctx, userID, BuildVectors(signals, s.now().UTC()), AlgorithmVersion, buildKey)
}
func (s *Service) Latest(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return s.repository.LatestProfile(ctx, userID)
}

type JobPayload struct {
	Version int       `json:"version"`
	UserID  uuid.UUID `json:"user_id"`
}

func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	return worker.Register(JobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload JobPayload
		if json.Unmarshal(lease.Payload, &payload) != nil || payload.Version != JobPayloadVersion || payload.UserID == uuid.Nil {
			return errors.New("unsupported profile job payload schema")
		}
		built, err := s.rebuildWithKey(ctx, payload.UserID, lease.IdempotencyKey)
		if err != nil {
			return err
		}
		if s.queue != nil {
			if err := s.queue.RecordResult(ctx, lease, map[string]any{"profileVersion": built.Version, "profileId": built.ID}); err != nil {
				return err
			}
			summaryPayload, _ := json.Marshal(map[string]any{"version": 1, "profile_id": built.ID})
			if _, err := s.queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: "ai.profile_summary", Stage: "profile_summary", Required: false, IdempotencyKey: lease.IdempotencyKey + ":summary:v1", Payload: summaryPayload, MaxAttempts: 3, UserID: payload.UserID, Priority: lease.Priority}); err != nil {
				return err
			}
		}
		return nil
	})
}
