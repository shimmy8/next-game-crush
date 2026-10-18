package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/preferences"
)

const (
	ActionLike                  = "like"
	ActionDislike               = "dislike"
	ActionRemind                = "remind_me"
	JobKind                     = "feedback.classify"
	JobPayloadVersion           = 1
	ClassificationSchemaVersion = 2
)

var (
	ErrInvalidCommand      = errors.New("invalid feedback command")
	ErrIdempotencyConflict = errors.New("feedback idempotency key was already used for another command")
)

var taxonomy = map[string]bool{"genre": true, "setting": true, "multiplayer": true, "single-player": true, "difficulty": true, "length": true, "early-access": true, "already-known": true, "not-now": true, "other": true}

var taxonomyOrder = []string{"genre", "setting", "multiplayer", "single-player", "difficulty", "length", "early-access", "already-known", "not-now", "other"}

type Command struct {
	UserID           uuid.UUID
	RecommendationID uuid.UUID
	Action           string
	Reason           string
	Reasons          []string
	FreeText         string
	IdempotencyKey   string
}

type Accepted struct {
	EventID                uuid.UUID `json:"eventId"`
	RebuildJobID           uuid.UUID `json:"rebuildJobId"`
	PreviousRankingVersion int64     `json:"previousRankingVersion"`
}

type Event struct {
	ID                                       uuid.UUID
	UserID                                   uuid.UUID
	RecommendationID                         uuid.UUID
	GameID                                   uuid.UUID
	Action, Reason, FreeText, IdempotencyKey string
	Reasons                                  []string
	Snapshot                                 json.RawMessage
	PreviousRankingVersion                   int64
	ResultingRankingVersion                  int64
	RebuildJobID                             uuid.UUID
}

type Classification struct {
	FeedbackID       uuid.UUID
	Taxonomy         string
	Polarity         string
	Confidence       float64
	Actionable       bool
	Model            string
	InputContentHash string
	SchemaVersion    int
	Preference       *preferences.Target
}

type RankingDiff struct {
	Adjustment      *AdjustmentSummary    `json:"adjustment,omitempty"`
	RebuildStatus   string                `json:"rebuildStatus"`
	PreviousVersion int64                 `json:"previousVersion"`
	CurrentVersion  int64                 `json:"currentVersion"`
	Added           []uuid.UUID           `json:"added"`
	Removed         []uuid.UUID           `json:"removed"`
	Reordered       []uuid.UUID           `json:"reordered"`
	Classification  *ClassificationResult `json:"classification,omitempty"`
}

type AdjustmentSummary struct {
	ID     uuid.UUID `json:"id"`
	Label  string    `json:"label"`
	Effect string    `json:"effect"`
	Active bool      `json:"active"`
}

type ClassificationResult struct {
	Taxonomy   string  `json:"taxonomy"`
	Polarity   string  `json:"polarity"`
	Confidence float64 `json:"confidence"`
	Actionable bool    `json:"actionable"`
}

type Repository interface {
	Record(context.Context, Command) (Event, bool, error)
	LinkRebuildJob(context.Context, uuid.UUID, uuid.UUID) error
	Event(context.Context, uuid.UUID) (Event, error)
	Classification(context.Context, uuid.UUID) (Classification, error)
	SaveClassification(context.Context, Classification) error
	FinalizeRanking(context.Context, uuid.UUID) (int64, error)
	RankingDiff(context.Context, uuid.UUID, int64, uuid.UUID) (RankingDiff, error)
}

type Classifier interface {
	ClassifyFeedback(context.Context, ai.FeedbackText) (ai.FeedbackClassification, error)
}
type Scheduler interface {
	Enqueue(context.Context, jobs.Spec) (uuid.UUID, error)
}
type workflowQueue interface {
	Scheduler
	AwaitChildren(context.Context, jobs.Lease, []jobs.Spec) (bool, error)
	RecordResult(context.Context, jobs.Lease, any) error
}

type Service struct {
	repository Repository
	classifier Classifier
	model      string
	scheduler  Scheduler
}

func NewService(repository Repository, classifier Classifier, model string, scheduler Scheduler) (*Service, error) {
	if repository == nil || classifier == nil || strings.TrimSpace(model) == "" || scheduler == nil {
		return nil, errors.New("feedback repository, classifier, model, and scheduler are required")
	}
	return &Service{repository: repository, classifier: classifier, model: model, scheduler: scheduler}, nil
}

func (s *Service) Record(ctx context.Context, command Command) (Accepted, error) {
	command.Action = strings.TrimSpace(command.Action)
	command.FreeText = strings.TrimSpace(command.FreeText)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	var err error
	command, err = normalizeCommand(command)
	if err != nil {
		return Accepted{}, err
	}
	if err := validateCommand(command); err != nil {
		return Accepted{}, err
	}
	event, _, err := s.repository.Record(ctx, command)
	if err != nil {
		return Accepted{}, err
	}
	if event.UserID != command.UserID || event.RecommendationID != command.RecommendationID || event.Action != command.Action || event.Reason != command.Reason || !slices.Equal(event.Reasons, command.Reasons) || event.FreeText != command.FreeText {
		return Accepted{}, ErrIdempotencyConflict
	}
	payload, _ := json.Marshal(jobPayload{Version: JobPayloadVersion, EventID: event.ID})
	jobID, err := s.scheduler.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: JobKind, Stage: "feedback", UserID: event.UserID, Priority: 100, Required: true, IdempotencyKey: "feedback:" + event.ID.String() + ":workflow:v1", Payload: payload, MaxAttempts: 3})
	if err != nil {
		return Accepted{}, fmt.Errorf("enqueue feedback workflow: %w", err)
	}
	if err := s.repository.LinkRebuildJob(ctx, event.ID, jobID); err != nil {
		return Accepted{}, fmt.Errorf("link feedback workflow: %w", err)
	}
	return Accepted{EventID: event.ID, RebuildJobID: jobID, PreviousRankingVersion: event.PreviousRankingVersion}, nil
}

func normalizeCommand(command Command) (Command, error) {
	command.Reason = strings.TrimSpace(command.Reason)
	if command.Reason != "" && len(command.Reasons) > 0 {
		return Command{}, ErrInvalidCommand
	}
	input := command.Reasons
	if command.Reason != "" {
		input = []string{command.Reason}
	}
	seen := make(map[string]bool, len(input))
	for _, raw := range input {
		reason := strings.TrimSpace(raw)
		if reason == "" || !taxonomy[reason] || seen[reason] {
			return Command{}, ErrInvalidCommand
		}
		seen[reason] = true
	}
	command.Reasons = command.Reasons[:0]
	for _, reason := range taxonomyOrder {
		if seen[reason] {
			command.Reasons = append(command.Reasons, reason)
		}
	}
	command.Reason = ""
	if len(command.Reasons) > 0 {
		command.Reason = command.Reasons[0]
	}
	return command, nil
}

func validateCommand(command Command) error {
	if command.UserID == uuid.Nil || command.RecommendationID == uuid.Nil || !oneOf(command.Action, ActionLike, ActionDislike, ActionRemind) || command.IdempotencyKey == "" || len(command.IdempotencyKey) > 200 || !utf8.ValidString(command.FreeText) || utf8.RuneCountInString(command.FreeText) > 500 || len(command.Reasons) > len(taxonomyOrder) || command.Reason != "" && !taxonomy[command.Reason] {
		return ErrInvalidCommand
	}
	if command.Action == ActionRemind && (len(command.Reasons) > 0 || command.FreeText != "") {
		return ErrInvalidCommand
	}
	return nil
}

func (s *Service) ProcessClassification(ctx context.Context, eventID uuid.UUID) error {
	if cached, err := s.repository.Classification(ctx, eventID); err == nil {
		return s.repository.SaveClassification(ctx, cached)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load feedback classification: %w", err)
	}
	event, err := s.repository.Event(ctx, eventID)
	if err != nil {
		return fmt.Errorf("load feedback event: %w", err)
	}
	classification := deterministicClassification(event)
	if event.FreeText != "" {
		classification = Classification{Taxonomy: "other", Polarity: "neutral", Model: "deterministic-fallback"}
		output, classifyErr := s.classifier.ClassifyFeedback(ctx, ai.FeedbackText{EvidenceKey: "feedback-text", Text: event.FreeText})
		if classifyErr == nil && ai.ValidateFeedbackClassification(output, "feedback-text") == nil {
			classification.Taxonomy, classification.Polarity, classification.Confidence = output.Taxonomy, output.Polarity, output.Confidence
			classification.Actionable = output.Actionable && output.Confidence >= .7
			classification.Model = s.model
			if event.Action == ActionDislike {
				classification.Preference = output.Preference
			}
		} else if errors.Is(classifyErr, ai.ErrCapabilityDisabled) || errors.Is(classifyErr, ai.ErrHTTP) || errors.Is(classifyErr, context.DeadlineExceeded) {
			classification = deterministicClassification(event)
		}
	}
	classification.FeedbackID = event.ID
	classification.SchemaVersion = ClassificationSchemaVersion
	hashModel := classification.Model
	if hashModel == "deterministic-fallback" {
		hashModel = "deterministic-fallback-v2"
	}
	reasonsJSON, _ := json.Marshal(event.Reasons)
	hash, err := ai.ContentHash(JobKind, ai.FeedbackSchemaVersion, ai.FeedbackPromptVersion, hashModel, map[string]string{"text": event.FreeText, "action": event.Action, "reasons": string(reasonsJSON)})
	if err != nil {
		return err
	}
	classification.InputContentHash = hash
	return s.repository.SaveClassification(ctx, classification)
}

func deterministicClassification(event Event) Classification {
	polarity := "neutral"
	if event.Action == ActionLike {
		polarity = "positive"
	}
	if event.Action == ActionDislike {
		polarity = "negative"
	}
	primary := event.Reason
	if len(event.Reasons) > 0 {
		primary = event.Reasons[0]
		if slices.Contains(event.Reasons, "multiplayer") {
			primary = "multiplayer"
		}
	}
	if taxonomy[primary] {
		value := Classification{Taxonomy: primary, Polarity: polarity, Confidence: 1, Actionable: event.Action != ActionRemind, Model: "deterministic-fallback"}
		if event.Action == ActionDislike && (primary == "multiplayer" || slices.Contains(event.Reasons, "multiplayer")) {
			value.Preference = &preferences.Target{Dimension: "mode", Target: preferences.ModeMultiplayer, Direction: "avoid"}
		}
		return value
	}
	return Classification{Taxonomy: "other", Polarity: "neutral", Confidence: 0, Actionable: false, Model: "deterministic-fallback"}
}

func (s *Service) Diff(ctx context.Context, userID uuid.UUID, version int64, jobID uuid.UUID) (RankingDiff, error) {
	if userID == uuid.Nil || version < 1 {
		return RankingDiff{}, ErrInvalidCommand
	}
	return s.repository.RankingDiff(ctx, userID, version, jobID)
}

type jobPayload struct {
	Version int       `json:"version"`
	EventID uuid.UUID `json:"event_id"`
}

func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	queue, ok := s.scheduler.(workflowQueue)
	if !ok {
		return errors.New("feedback scheduler does not support workflows")
	}
	if worker == nil {
		return errors.New("worker is required")
	}
	return worker.Register(JobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload jobPayload
		if json.Unmarshal(lease.Payload, &payload) != nil || payload.Version != JobPayloadVersion || payload.EventID == uuid.Nil {
			return errors.New("unsupported feedback job payload schema")
		}
		if err := s.ProcessClassification(ctx, payload.EventID); err != nil {
			return err
		}
		event, err := s.repository.Event(ctx, payload.EventID)
		if err != nil {
			return err
		}
		if event.Action == ActionRemind {
			if _, err := s.repository.FinalizeRanking(ctx, event.ID); err != nil {
				return err
			}
			diff, err := s.repository.RankingDiff(ctx, event.UserID, event.PreviousRankingVersion, lease.ID)
			if err != nil {
				return err
			}
			return queue.RecordResult(ctx, lease, diff)
		}
		profilePayload, _ := json.Marshal(map[string]any{"version": 1, "user_id": event.UserID})
		ready, err := queue.AwaitChildren(ctx, lease, []jobs.Spec{{Lane: jobs.LaneInteractive, Kind: "profile.rebuild", Stage: "profile", UserID: event.UserID, Priority: 100, Required: true, IdempotencyKey: "feedback:" + event.ID.String() + ":profile:v1", Payload: profilePayload, MaxAttempts: 3}})
		if err != nil {
			return err
		}
		if !ready {
			return jobs.ErrDeferred
		}
		rankingPayload, _ := json.Marshal(map[string]any{"version": 1, "user_id": event.UserID})
		ready, err = queue.AwaitChildren(ctx, lease, []jobs.Spec{{Lane: jobs.LaneInteractive, Kind: "recommendations.rebuild", Stage: "recommendations", UserID: event.UserID, Priority: 100, Required: true, IdempotencyKey: "feedback:" + event.ID.String() + ":ranking:v1", Payload: rankingPayload, MaxAttempts: 3}})
		if err != nil {
			return err
		}
		if !ready {
			return jobs.ErrDeferred
		}
		if _, err := s.repository.FinalizeRanking(ctx, event.ID); err != nil {
			return err
		}
		diff, err := s.repository.RankingDiff(ctx, event.UserID, event.PreviousRankingVersion, lease.ID)
		if err != nil {
			return err
		}
		return queue.RecordResult(ctx, lease, diff)
	})
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
