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
	AlgorithmVersion           = "activity-profile-v5"
	CompletionAlgorithmVersion = completion.ProjectionAlgorithmVersion
	JobKind                    = "profile.rebuild"
	JobPayloadVersion          = 1
)

type PublicationState string

const (
	PublicationBuilding     PublicationState = "building"
	PublicationPublished    PublicationState = "published"
	PublicationFailed       PublicationState = "failed"
	PublicationInsufficient PublicationState = "insufficient"
)

type SummarySource string

const (
	SummarySourceOpenAI        SummarySource = "openai"
	SummarySourceDeterministic SummarySource = "deterministic"
	SummarySourceLegacy        SummarySource = "legacy"
)

type SummaryStatus string

const (
	SummaryReady       SummaryStatus = "ready"
	SummaryUnavailable SummaryStatus = "unavailable"
)

type Profile struct {
	ID               uuid.UUID        `json:"id"`
	UserID           uuid.UUID        `json:"-"`
	Version          int64            `json:"version"`
	Summary          string           `json:"summary"`
	SummaryStatus    SummaryStatus    `json:"summaryStatus"`
	SummarySource    SummarySource    `json:"summarySource,omitempty"`
	PublicationState PublicationState `json:"publicationState"`
	PublishedAt      *time.Time       `json:"publishedAt,omitempty"`
	Updating         bool             `json:"updating"`
	PositiveStrength float64          `json:"-"`
	Confidence       float64          `json:"confidence"`
	EvidenceCount    int              `json:"evidenceCount"`
	SignalCounts     map[string]int   `json:"-"`
	Positive         []float32        `json:"-"`
	// Negative is a read-compatibility field for legacy profiles. New activity
	// profile builds leave it nil and readers ignore negative embedding rows.
	Negative  []float32 `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
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

type BuildRequest struct {
	UserID           uuid.UUID
	SourceWorkflowID uuid.UUID
	BuildKey         string
}

type BuildResult struct {
	Profile         Profile
	SummaryRequired bool
}

type Repository interface {
	BuildDraft(context.Context, BuildRequest, time.Time, string) (Profile, bool, error)
	LatestProfile(context.Context, uuid.UUID) (Profile, error)
	LatestPublishedProfile(context.Context, uuid.UUID) (Profile, error)
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

func (s *Service) Build(ctx context.Context, request BuildRequest) (BuildResult, error) {
	if request.UserID == uuid.Nil {
		return BuildResult{}, errors.New("user ID is required")
	}
	if request.SourceWorkflowID == uuid.Nil {
		return BuildResult{}, errors.New("source workflow ID is required")
	}
	if strings.TrimSpace(request.BuildKey) == "" {
		return BuildResult{}, errors.New("profile build key is required")
	}
	profile, _, err := s.repository.BuildDraft(ctx, request, s.now().UTC(), AlgorithmVersion)
	if err != nil {
		return BuildResult{}, fmt.Errorf("build activity profile draft: %w", err)
	}
	return BuildResult{Profile: profile, SummaryRequired: profile.PublicationState == PublicationBuilding}, nil
}

// Rebuild is the compatibility entry point for synchronous callers. Queued
// workflows use Build with the durable root workflow identity.
func (s *Service) Rebuild(ctx context.Context, userID uuid.UUID) (Profile, error) {
	workflowID := uuid.New()
	result, err := s.Build(ctx, BuildRequest{UserID: userID, SourceWorkflowID: workflowID, BuildKey: workflowID.String()})
	return result.Profile, err
}

func (s *Service) rebuildWithKey(ctx context.Context, userID uuid.UUID, buildKey string) (Profile, error) {
	workflowID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(buildKey))
	result, err := s.Build(ctx, BuildRequest{UserID: userID, SourceWorkflowID: workflowID, BuildKey: buildKey})
	return result.Profile, err
}

func (s *Service) Latest(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return s.repository.LatestPublishedProfile(ctx, userID)
}

type JobPayload struct {
	Version          int       `json:"version"`
	UserID           uuid.UUID `json:"user_id"`
	SourceWorkflowID uuid.UUID `json:"source_workflow_id,omitempty"`
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
		workflowID := payload.SourceWorkflowID
		if workflowID == uuid.Nil {
			workflowID = lease.WorkflowID
		}
		if workflowID == uuid.Nil {
			return errors.New("profile source workflow ID is required")
		}
		result, err := s.Build(ctx, BuildRequest{UserID: payload.UserID, SourceWorkflowID: workflowID, BuildKey: lease.IdempotencyKey})
		if err != nil {
			return err
		}
		if s.queue == nil {
			return nil
		}
		if err := s.queue.RecordResult(ctx, lease, map[string]any{
			"profileId": result.Profile.ID, "profileVersion": result.Profile.Version,
			"publicationState": result.Profile.PublicationState, "summaryRequired": result.SummaryRequired,
		}); err != nil {
			return err
		}
		if !result.SummaryRequired {
			return nil
		}
		summaryPayload, _ := json.Marshal(map[string]any{"version": 1, "profile_id": result.Profile.ID})
		_, err = s.queue.Enqueue(ctx, jobs.Spec{
			Lane: jobs.LaneInteractive, Kind: "ai.profile_summary", Stage: "profile_summary", Required: false,
			IdempotencyKey: lease.IdempotencyKey + ":summary:v1", Payload: summaryPayload,
			MaxAttempts: 3, UserID: payload.UserID, Priority: lease.Priority,
		})
		return err
	})
}
