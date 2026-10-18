package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

const JobPayloadVersion = 1

const (
	FeaturesJobKind       = "ai.features"
	EmbeddingJobKind      = "ai.embedding"
	ProfileSummaryJobKind = "ai.profile_summary"
)

type JobPayload struct {
	Version   int       `json:"version"`
	GameID    uuid.UUID `json:"game_id,omitempty"`
	ProfileID uuid.UUID `json:"profile_id,omitempty"`
}

type StoredGameEvidence struct {
	Name           string
	Description    string
	Genres         []string
	Tags           []string
	Categories     []string
	StoredFeatures json.RawMessage
}

type Repository interface {
	ListGameIDs(context.Context, uuid.UUID) ([]uuid.UUID, error)
	LoadGameEvidence(context.Context, uuid.UUID, string) (StoredGameEvidence, error)
	HasGameFeatures(context.Context, uuid.UUID, string, string) (bool, error)
	SaveGameFeatures(context.Context, uuid.UUID, string, string, GameFeatures) error
	HasGameEmbedding(context.Context, uuid.UUID, string, string) (bool, error)
	SaveGameEmbedding(context.Context, uuid.UUID, string, string, []float32) error
	LoadProfileEvidence(context.Context, uuid.UUID) (ProfileEvidence, error)
	HasProfileSummary(context.Context, uuid.UUID, string, string) (bool, error)
	SaveProfileSummary(context.Context, uuid.UUID, string, string, ProfileSummary) error
}

type Service struct {
	repository            Repository
	client                Client
	model, embeddingModel string
	queue                 *jobs.Queue
}

func NewService(repository Repository, client Client, model, embeddingModel string) (*Service, error) {
	if repository == nil || client == nil {
		return nil, errors.New("AI repository and client are required")
	}
	if strings.TrimSpace(model) == "" || strings.TrimSpace(embeddingModel) == "" {
		return nil, errors.New("AI model IDs are required")
	}
	return &Service{repository: repository, client: client, model: model, embeddingModel: embeddingModel}, nil
}

func (service *Service) SetQueue(queue *jobs.Queue) { service.queue = queue }

func (service *Service) PlanFeatureJobs(ctx context.Context, userID uuid.UUID, rootKey string) ([]jobs.Spec, error) {
	if userID == uuid.Nil || strings.TrimSpace(rootKey) == "" {
		return nil, errors.New("AI job user and root key are required")
	}
	gameIDs, err := service.repository.ListGameIDs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list AI games: %w", err)
	}
	specs := make([]jobs.Spec, 0, len(gameIDs))
	for _, gameID := range gameIDs {
		payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
		specs = append(specs, jobs.Spec{Kind: FeaturesJobKind, Stage: "ai_features", Required: true, IdempotencyKey: rootKey + ":ai.features:v1:" + gameID.String(), Payload: payload, MaxAttempts: 3})
	}
	return specs, nil
}

func (service *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	if err := worker.Register(FeaturesJobKind, service.handleFeatures); err != nil {
		return err
	}
	if err := worker.Register(EmbeddingJobKind, service.handleEmbedding); err != nil {
		return err
	}
	return worker.Register(ProfileSummaryJobKind, service.handleProfileSummary)
}

func (service *Service) handleFeatures(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodeJobPayload(lease.Payload, true, false)
	if err != nil {
		return err
	}
	if err := service.processFeatures(ctx, payload.GameID); err != nil {
		return err
	}
	if service.queue == nil {
		return nil
	}
	embeddingPayload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: payload.GameID})
	ready, err := service.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Kind: EmbeddingJobKind, Stage: "ai_embedding", Required: true, IdempotencyKey: lease.IdempotencyKey + ":embedding:v1", Payload: embeddingPayload, MaxAttempts: 3}})
	if err != nil {
		return err
	}
	if !ready {
		return jobs.ErrDeferred
	}
	return nil
}
func (service *Service) handleEmbedding(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodeJobPayload(lease.Payload, true, false)
	if err != nil {
		return err
	}
	return service.processEmbedding(ctx, payload.GameID)
}
func (service *Service) handleProfileSummary(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodeJobPayload(lease.Payload, false, true)
	if err != nil {
		return err
	}
	return service.processProfileSummary(ctx, payload.ProfileID)
}

func (service *Service) processFeatures(ctx context.Context, gameID uuid.UUID) error {
	stored, err := service.repository.LoadGameEvidence(ctx, gameID, service.model)
	if err != nil {
		return fmt.Errorf("load game evidence: %w", err)
	}
	evidence := gameEvidence(stored)
	_, evidence, err = normalizeGameEvidence(evidence)
	if err != nil {
		return err
	}
	hash, err := ContentHash(FeaturesJobKind, SchemaVersion, FeaturePromptVersion, service.model, evidence)
	if err != nil {
		return err
	}
	cached, err := service.repository.HasGameFeatures(ctx, gameID, service.model, hash)
	if err != nil {
		return err
	}
	if cached {
		return nil
	}
	output, err := service.client.ExtractGameFeatures(ctx, evidence)
	if errors.Is(err, ErrCapabilityDisabled) {
		return nil
	}
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(evidence.Evidence))
	for _, item := range evidence.Evidence {
		allowed[item.Key] = struct{}{}
	}
	if err := validateFeatureOutput(output); err != nil {
		return err
	}
	if err := validateCitations(output.EvidenceKeys, allowed); err != nil {
		return err
	}
	return service.repository.SaveGameFeatures(ctx, gameID, service.model, hash, output)
}

func (service *Service) processEmbedding(ctx context.Context, gameID uuid.UUID) error {
	stored, err := service.repository.LoadGameEvidence(ctx, gameID, service.model)
	if err != nil {
		return fmt.Errorf("load embedding evidence: %w", err)
	}
	canonical := canonicalEmbeddingText(stored)
	hash, err := ContentHash(EmbeddingJobKind, SchemaVersion, "canonical-game-embedding-v1", service.embeddingModel, canonical)
	if err != nil {
		return err
	}
	cached, err := service.repository.HasGameEmbedding(ctx, gameID, service.embeddingModel, hash)
	if err != nil {
		return err
	}
	if cached {
		return nil
	}
	vectors, err := service.client.Embed(ctx, []string{canonical})
	if errors.Is(err, ErrCapabilityDisabled) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(vectors) != 1 || len(vectors[0]) != EmbeddingDimensions {
		return ErrInvalidEmbedding
	}
	for _, value := range vectors[0] {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return ErrInvalidEmbedding
		}
	}
	return service.repository.SaveGameEmbedding(ctx, gameID, service.embeddingModel, hash, vectors[0])
}

func (service *Service) processProfileSummary(ctx context.Context, profileID uuid.UUID) error {
	evidence, err := service.repository.LoadProfileEvidence(ctx, profileID)
	if err != nil {
		return fmt.Errorf("load profile evidence: %w", err)
	}
	_, evidence, err = normalizeProfileEvidence(evidence)
	if err != nil {
		return err
	}
	hash, err := ContentHash(ProfileSummaryJobKind, SchemaVersion, ProfilePromptVersion, service.model, evidence)
	if err != nil {
		return err
	}
	cached, err := service.repository.HasProfileSummary(ctx, profileID, service.model, hash)
	if err != nil {
		return err
	}
	if cached {
		return nil
	}
	output, err := service.client.SummarizeProfile(ctx, evidence)
	if errors.Is(err, ErrCapabilityDisabled) {
		return nil
	}
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(evidence.Evidence))
	for _, item := range evidence.Evidence {
		allowed[item.Key] = struct{}{}
	}
	output, err = normalizeAndValidateProfileOutput(output, allowed)
	if err != nil {
		return err
	}
	return service.repository.SaveProfileSummary(ctx, profileID, service.model, hash, output)
}

func decodeJobPayload(raw json.RawMessage, needGame, needProfile bool) (JobPayload, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var payload JobPayload
	if err := decoder.Decode(&payload); err != nil || payload.Version != JobPayloadVersion || (needGame && payload.GameID == uuid.Nil) || (needProfile && payload.ProfileID == uuid.Nil) {
		return JobPayload{}, errors.New("unsupported AI job payload schema")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return JobPayload{}, errors.New("AI job payload contains trailing data")
	}
	if (needGame && payload.ProfileID != uuid.Nil) || (needProfile && payload.GameID != uuid.Nil) {
		return JobPayload{}, errors.New("AI job payload contains an unexpected target")
	}
	return payload, nil
}

func gameEvidence(stored StoredGameEvidence) GameEvidence {
	items := []EvidenceItem{{Key: "title", Value: stored.Name}}
	if stored.Description != "" {
		items = append(items, EvidenceItem{Key: "description", Value: stored.Description})
	}
	for i, value := range stored.Genres {
		items = append(items, EvidenceItem{Key: fmt.Sprintf("genre-%02d", i+1), Value: value})
	}
	for i, value := range stored.Tags {
		items = append(items, EvidenceItem{Key: fmt.Sprintf("tag-%02d", i+1), Value: value})
	}
	for i, value := range stored.Categories {
		items = append(items, EvidenceItem{Key: fmt.Sprintf("category-%02d", i+1), Value: value})
	}
	return GameEvidence{EvidenceKey: "game-evidence", Name: stored.Name, Evidence: items}
}

func canonicalEmbeddingText(stored StoredGameEvidence) string {
	parts := []string{stored.Name, stored.Description}
	parts = append(parts, stored.Genres...)
	parts = append(parts, stored.Tags...)
	parts = append(parts, stored.Categories...)
	if len(stored.StoredFeatures) > 0 {
		parts = append(parts, string(stored.StoredFeatures))
	}
	for i := range parts {
		parts[i] = sanitizeText(parts[i])
	}
	return strings.Join(parts, "\n")
}
