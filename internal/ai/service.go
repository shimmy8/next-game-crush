package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
)

const JobPayloadVersion = 1

const (
	FeaturesJobKind       = "ai.features"
	EmbeddingJobKind      = "ai.embedding"
	ProfileSummaryJobKind = "ai.profile_summary"
)

type JobPayload struct {
	Version   int            `json:"version"`
	GameID    uuid.UUID      `json:"game_id,omitempty"`
	ProfileID uuid.UUID      `json:"profile_id,omitempty"`
	Execution *GameExecution `json:"execution,omitempty"`
}

type StoredGameEvidence struct {
	Name             string
	Description      string
	Genres           []string
	Tags             []string
	Categories       []string
	StoredFeatures   json.RawMessage
	ContentHash      string
	FeatureInputHash string
}

type Repository interface {
	ListGameIDs(context.Context, uuid.UUID) ([]uuid.UUID, error)
	LoadGameEvidence(context.Context, uuid.UUID, string) (StoredGameEvidence, error)
	HasGameFeatures(context.Context, uuid.UUID, string, string) (bool, error)
	SaveGameFeatures(context.Context, uuid.UUID, string, string, GameFeatures) error
	HasGameEmbedding(context.Context, uuid.UUID, string, string) (bool, error)
	SaveGameEmbedding(context.Context, uuid.UUID, string, string, []float32) error
	LoadProfileEvidence(context.Context, uuid.UUID) (ProfileEvidence, error)
	LoadCachedProfileSummary(context.Context, uuid.UUID, string, string) (ProfileSummary, bool, error)
	PublishProfileSummary(context.Context, jobs.Lease, uuid.UUID, string, string, ProfileSummary, string) error
	HandleTerminalProfileSummaryFailure(context.Context, jobs.Lease, uuid.UUID, string) (bool, error)
}

type CatalogFeatureAdmission interface {
	ReserveCatalogFeature(context.Context, jobs.Lease, string, time.Time) (bool, time.Time, error)
}

type Service struct {
	repository            Repository
	client                Client
	model, embeddingModel string
	queue                 *jobs.Queue
	catalogAdmission      CatalogFeatureAdmission
	catalogChangedHook    func(context.Context, uuid.UUID) error
	now                   func() time.Time
	logger                *observability.Logger
}

func NewService(repository Repository, client Client, model, embeddingModel string) (*Service, error) {
	if repository == nil || client == nil {
		return nil, errors.New("AI repository and client are required")
	}
	if strings.TrimSpace(model) == "" || strings.TrimSpace(embeddingModel) == "" {
		return nil, errors.New("AI model IDs are required")
	}
	return &Service{repository: repository, client: client, model: model, embeddingModel: embeddingModel, now: time.Now, logger: observability.New(nil)}, nil
}

func (service *Service) SetQueue(queue *jobs.Queue) { service.queue = queue }
func (service *Service) SetLogger(logger *observability.Logger) {
	if logger == nil {
		logger = observability.New(nil)
	}
	service.logger = logger
}
func (service *Service) SetCatalogFeatureAdmission(admission CatalogFeatureAdmission) {
	service.catalogAdmission = admission
}
func (service *Service) SetCatalogChangedHook(hook func(context.Context, uuid.UUID) error) {
	service.catalogChangedHook = hook
}

// PlanGameFeatureJob is independent of a user workflow. Both model versions
// participate in the identity so a new embedding model can refresh vectors
// while the feature handler reuses its existing content guard.
func (service *Service) PlanGameFeatureJob(_ context.Context, gameID uuid.UUID, contentHash string, priority int) (jobs.Spec, error) {
	return service.planGameFeatureJob(gameID, contentHash, priority, jobs.LaneInteractive, 3)
}

// PlanCatalogGameFeatureJob keeps the semantic execution identity shared with
// the legacy planner while isolating bulk enrichment on the catalog lane.
func (service *Service) PlanCatalogGameFeatureJob(_ context.Context, gameID uuid.UUID, contentHash string, priority int) (jobs.Spec, error) {
	return service.planGameFeatureJob(gameID, contentHash, priority, jobs.LaneCatalog, 4)
}

func (service *Service) planGameFeatureJob(gameID uuid.UUID, contentHash string, priority int, lane jobs.Lane, maxAttempts int) (jobs.Spec, error) {
	if gameID == uuid.Nil || strings.TrimSpace(contentHash) == "" || priority < 1 || priority > 100 {
		return jobs.Spec{}, errors.New("game enrichment identity is required")
	}
	key, err := featureLogicalKey(gameID, contentHash, service.model, service.embeddingModel)
	if err != nil {
		return jobs.Spec{}, err
	}
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID, Execution: &GameExecution{ContentHash: contentHash, TextModel: service.model, EmbeddingModel: service.embeddingModel, Key: key}})
	return jobs.Spec{Lane: lane, Kind: FeaturesJobKind, Stage: "ai_features", Priority: priority, IdempotencyKey: key, Payload: payload, MaxAttempts: maxAttempts}, nil
}

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
		specs = append(specs, jobs.Spec{Lane: jobs.LaneInteractive, Kind: FeaturesJobKind, Stage: "ai_features", Required: true, IdempotencyKey: rootKey + ":ai.features:v1:" + gameID.String(), Payload: payload, MaxAttempts: 3, Priority: 100, UserID: userID})
	}
	return specs, nil
}

func (service *Service) RegisterHandlers(worker *jobs.Worker) error {
	if err := service.registerGameHandlers(worker); err != nil {
		return err
	}
	return worker.Register(ProfileSummaryJobKind, service.handleProfileSummary)
}

// RegisterCatalogHandlers deliberately excludes user profile work. Catalog
// workers may execute only the game feature pipeline and its embedding child.
func (service *Service) RegisterCatalogHandlers(worker *jobs.Worker) error {
	return service.registerGameHandlers(worker)
}

func (service *Service) registerGameHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	if err := worker.Register(FeaturesJobKind, service.handleFeatures); err != nil {
		return err
	}
	if err := worker.Register(EmbeddingJobKind, service.handleEmbedding); err != nil {
		return err
	}
	return nil
}

func (service *Service) handleFeatures(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodeJobPayload(lease.Payload, true, false)
	if err != nil {
		return err
	}
	plan, current, err := service.executionForJob(payload, lease)
	if err != nil || !current {
		return err
	}
	changed, current, _, err := service.processFeaturesForExecution(ctx, payload.GameID, plan, &lease)
	if err != nil {
		return service.catalogExecutionFailure(payload, plan, lease, err)
	}
	if !current {
		return nil
	}
	if changed {
		if err := service.recordCatalogChangePending(ctx, lease, true); err != nil {
			return err
		}
	}
	if service.queue == nil {
		return service.notifyCatalogChanged(ctx, lease, payload.GameID, "feature", changed || catalogChangePending(lease.Result))
	}
	embeddingPayload, _ := json.Marshal(payload)
	ready, err := service.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Kind: EmbeddingJobKind, Stage: "ai_embedding", Required: true, IdempotencyKey: lease.IdempotencyKey + ":embedding:v1", Payload: embeddingPayload, MaxAttempts: lease.MaxAttempts, Priority: lease.Priority}})
	if err != nil {
		return service.catalogExecutionFailure(payload, plan, lease, err)
	}
	if !ready {
		return jobs.ErrDeferred
	}
	return service.notifyCatalogChanged(ctx, lease, payload.GameID, "feature", changed || catalogChangePending(lease.Result))
}
func (service *Service) handleEmbedding(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodeJobPayload(lease.Payload, true, false)
	if err != nil {
		return err
	}
	plan, current, err := service.executionForJob(payload, lease)
	if err != nil || !current {
		return err
	}
	changed, _, persisted, err := service.processEmbeddingForExecution(ctx, payload.GameID, plan, &lease)
	if err != nil {
		return service.catalogExecutionFailure(payload, plan, lease, err)
	}
	if changed {
		if err := service.recordCatalogChangePending(ctx, lease, true); err != nil {
			return err
		}
	}
	return service.notifyCatalogChanged(ctx, lease, payload.GameID, "embedding", changed || (persisted && catalogChangePending(lease.Result)))
}

type catalogChangeResult struct {
	Pending bool `json:"catalogChangedPending"`
}

func catalogChangePending(raw json.RawMessage) bool {
	var result catalogChangeResult
	return json.Unmarshal(raw, &result) == nil && result.Pending
}

func (service *Service) recordCatalogChangePending(ctx context.Context, lease jobs.Lease, pending bool) error {
	if service.queue == nil || lease.ID == uuid.Nil {
		return nil
	}
	return service.queue.RecordResult(ctx, lease, catalogChangeResult{Pending: pending})
}

func (service *Service) notifyCatalogChanged(ctx context.Context, lease jobs.Lease, gameID uuid.UUID, stage string, pending bool) error {
	if !pending || service.catalogChangedHook == nil {
		return nil
	}
	if err := service.catalogChangedHook(ctx, gameID); err != nil {
		return service.deferCatalogChanged(ctx, lease, stage, err)
	}
	return service.recordCatalogChangePending(ctx, lease, false)
}

func (service *Service) deferCatalogChanged(ctx context.Context, lease jobs.Lease, stage string, cause error) error {
	if service.queue == nil || lease.ID == uuid.Nil {
		return fmt.Errorf("mark catalog %s change: %w", stage, cause)
	}
	if err := service.queue.DeferUntil(ctx, lease, time.Now().UTC().Add(5*time.Second)); err != nil {
		return fmt.Errorf("defer catalog %s change: %w", stage, err)
	}
	return jobs.ErrDeferred
}
func (service *Service) handleProfileSummary(ctx context.Context, lease jobs.Lease) error {
	started := service.now()
	service.logger.ProfilePublication(ctx, observability.ProfilePublicationEvent{Stage: "started"})
	payload, err := decodeJobPayload(lease.Payload, false, true)
	if err != nil {
		return err
	}
	err = service.processProfileSummary(ctx, lease, payload.ProfileID)
	if err == nil {
		if reader, ok := service.repository.(interface {
			ProfilePublicationState(context.Context, uuid.UUID) (int64, string, error)
		}); ok {
			version, source, readErr := reader.ProfilePublicationState(ctx, payload.ProfileID)
			if readErr == nil {
				service.logger.ProfilePublication(ctx, observability.ProfilePublicationEvent{Stage: "published", ProfileVersion: version, SummarySource: source, Duration: service.now().Sub(started)})
			}
		}
		return nil
	}
	if !terminalProfileSummaryAttempt(lease, err) {
		return err
	}
	published, terminalErr := service.repository.HandleTerminalProfileSummaryFailure(ctx, lease, payload.ProfileID, service.model)
	if terminalErr != nil {
		service.logger.ProfilePublication(ctx, observability.ProfilePublicationEvent{Stage: "failed", Duration: service.now().Sub(started), Reason: observability.ReasonPersistenceFailed})
		return terminalErr
	}
	if published {
		if reader, ok := service.repository.(interface {
			ProfilePublicationState(context.Context, uuid.UUID) (int64, string, error)
		}); ok {
			version, source, readErr := reader.ProfilePublicationState(ctx, payload.ProfileID)
			if readErr == nil {
				service.logger.ProfilePublication(ctx, observability.ProfilePublicationEvent{Stage: "published", ProfileVersion: version, SummarySource: source, Duration: service.now().Sub(started)})
			}
		}
		return nil
	}
	service.logger.ProfilePublication(ctx, observability.ProfilePublicationEvent{Stage: "failed", Duration: service.now().Sub(started), Reason: observability.ReasonInvalidOutput})
	return err
}

func (service *Service) processFeatures(ctx context.Context, gameID uuid.UUID) error {
	_, err := service.processFeaturesChanged(ctx, gameID)
	return err
}

func (service *Service) processFeaturesChanged(ctx context.Context, gameID uuid.UUID) (bool, error) {
	changed, _, _, err := service.processFeaturesForExecution(ctx, gameID, GameExecution{TextModel: service.model, EmbeddingModel: service.embeddingModel}, nil)
	return changed, err
}

func (service *Service) processFeaturesForExecution(ctx context.Context, gameID uuid.UUID, plan GameExecution, lease *jobs.Lease) (bool, bool, bool, error) {
	stored, current, err := service.loadGameExecution(ctx, gameID, plan, lease)
	if err != nil {
		return false, false, false, fmt.Errorf("load game evidence: %w", err)
	}
	if !current {
		return false, false, false, nil
	}
	hash, evidence, err := featureEvidenceHash(stored, service.model)
	if err != nil {
		return false, false, false, err
	}
	cached, err := service.repository.HasGameFeatures(ctx, gameID, service.model, hash)
	if err != nil {
		return false, false, false, err
	}
	if lease != nil && lease.Lane == jobs.LaneCatalog && plan.Generation > 0 {
		if service.catalogAdmission == nil || service.queue == nil {
			return false, false, false, errors.New("catalog AI admission is not configured")
		}
		identity, err := featureLogicalKey(gameID, plan.ContentHash, plan.TextModel, plan.EmbeddingModel)
		if err != nil {
			return false, false, false, err
		}
		now := service.now().UTC()
		granted, retryAt, err := service.catalogAdmission.ReserveCatalogFeature(ctx, *lease, identity, now)
		if err != nil {
			return false, false, false, err
		}
		if !granted {
			if !retryAt.After(now) {
				return false, false, false, errors.New("catalog AI admission returned an invalid retry time")
			}
			if err := service.queue.DeferUntil(ctx, *lease, retryAt); err != nil {
				return false, false, false, err
			}
			return false, false, false, jobs.ErrDeferred
		}
	}
	if cached {
		return false, true, true, nil
	}
	output, err := service.client.ExtractGameFeatures(ctx, evidence)
	if errors.Is(err, ErrCapabilityDisabled) {
		return false, true, false, nil
	}
	if err != nil {
		return false, false, false, err
	}
	allowed := make(map[string]struct{}, len(evidence.Evidence))
	for _, item := range evidence.Evidence {
		allowed[item.Key] = struct{}{}
	}
	if err := validateFeatureOutput(output); err != nil {
		return false, false, false, err
	}
	if err := validateCitations(output.EvidenceKeys, allowed); err != nil {
		return false, false, false, err
	}
	if repo, ok := service.repository.(gameExecutionRepository); ok {
		published, err := repo.SaveGameFeaturesForExecution(ctx, gameID, plan, lease, hash, output)
		return published, published, published, err
	}
	if err := service.repository.SaveGameFeatures(ctx, gameID, service.model, hash, output); err != nil {
		return false, false, false, err
	}
	return true, true, true, nil
}

func (service *Service) processEmbedding(ctx context.Context, gameID uuid.UUID) error {
	_, err := service.processEmbeddingChanged(ctx, gameID)
	return err
}

func (service *Service) processEmbeddingChanged(ctx context.Context, gameID uuid.UUID) (bool, error) {
	changed, _, _, err := service.processEmbeddingForExecution(ctx, gameID, GameExecution{TextModel: service.model, EmbeddingModel: service.embeddingModel}, nil)
	return changed, err
}

func (service *Service) processEmbeddingForExecution(ctx context.Context, gameID uuid.UUID, plan GameExecution, lease *jobs.Lease) (bool, bool, bool, error) {
	stored, current, err := service.loadGameExecution(ctx, gameID, plan, lease)
	if err != nil {
		return false, false, false, fmt.Errorf("load embedding evidence: %w", err)
	}
	if !current {
		return false, false, false, nil
	}
	if plan.ContentHash != "" {
		featureHash, _, err := featureEvidenceHash(stored, service.model)
		if err != nil {
			return false, false, false, err
		}
		if stored.FeatureInputHash != featureHash {
			return false, false, false, nil
		}
	}
	canonical := canonicalEmbeddingText(stored)
	hash, err := ContentHash(EmbeddingJobKind, SchemaVersion, "canonical-game-embedding-v1", service.embeddingModel, canonical)
	if err != nil {
		return false, false, false, err
	}
	cached, err := service.repository.HasGameEmbedding(ctx, gameID, service.embeddingModel, hash)
	if err != nil {
		return false, false, false, err
	}
	if cached {
		return false, true, true, nil
	}
	vectors, err := service.client.Embed(ctx, []string{canonical})
	if errors.Is(err, ErrCapabilityDisabled) {
		return false, true, false, nil
	}
	if err != nil {
		return false, false, false, err
	}
	if len(vectors) != 1 || len(vectors[0]) != EmbeddingDimensions {
		return false, false, false, ErrInvalidEmbedding
	}
	for _, value := range vectors[0] {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false, false, false, ErrInvalidEmbedding
		}
	}
	if repo, ok := service.repository.(gameExecutionRepository); ok {
		published, err := repo.SaveGameEmbeddingForExecution(ctx, gameID, plan, lease, hash, vectors[0])
		return published, published, published, err
	}
	if err := service.repository.SaveGameEmbedding(ctx, gameID, service.embeddingModel, hash, vectors[0]); err != nil {
		return false, false, false, err
	}
	return true, true, true, nil
}

func (service *Service) processProfileSummary(ctx context.Context, lease jobs.Lease, profileID uuid.UUID) error {
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
	cached, found, err := service.repository.LoadCachedProfileSummary(ctx, profileID, service.model, hash)
	if err != nil {
		return err
	}
	if found {
		return service.repository.PublishProfileSummary(ctx, lease, profileID, service.model, hash, cached, "openai")
	}
	output, err := service.client.SummarizeProfile(ctx, evidence)
	if errors.Is(err, ErrCapabilityDisabled) {
		fallback := ProfileSummary{Summary: DeterministicActivitySummary(evidence.Facts)}
		return service.repository.PublishProfileSummary(ctx, lease, profileID, service.model, hash, fallback, "deterministic")
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
	return service.repository.PublishProfileSummary(ctx, lease, profileID, service.model, hash, output, "openai")
}

func terminalProfileSummaryAttempt(lease jobs.Lease, err error) bool {
	if lease.MaxAttempts > 0 && lease.Attempts >= lease.MaxAttempts {
		return true
	}
	type retryable interface{ Retryable() bool }
	var classified retryable
	return errors.As(err, &classified) && !classified.Retryable()
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
	if payload.Execution != nil && (!needGame || !validGameExecution(payload.GameID, *payload.Execution)) {
		return JobPayload{}, errors.New("invalid catalog enrichment execution")
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
