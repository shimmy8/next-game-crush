package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

// GameExecution binds a catalog job to the metadata/model occurrence it plans.
// Legacy foreground payloads omit it and continue using the current model.
type GameExecution struct {
	ContentHash    string `json:"content_hash"`
	TextModel      string `json:"text_model"`
	EmbeddingModel string `json:"embedding_model"`
	Generation     int64  `json:"generation"`
	Key            string `json:"key"`
}

type gameExecutionRepository interface {
	LoadGameExecution(context.Context, uuid.UUID, GameExecution, *jobs.Lease) (StoredGameEvidence, bool, error)
	SaveGameFeaturesForExecution(context.Context, uuid.UUID, GameExecution, *jobs.Lease, string, GameFeatures) (bool, error)
	SaveGameEmbeddingForExecution(context.Context, uuid.UUID, GameExecution, *jobs.Lease, string, []float32) (bool, error)
}

func featureLogicalKey(gameID uuid.UUID, contentHash, textModel, embeddingModel string) (string, error) {
	identity, err := ContentHash(FeaturesJobKind, SchemaVersion, FeaturePromptVersion, textModel, []string{contentHash, embeddingModel})
	if err != nil {
		return "", err
	}
	return "catalog:ai.features:v1:" + gameID.String() + ":" + identity, nil
}

// BindGameFeatureExecution is called only by the catalog's transactional planner.
func (s *Service) BindGameFeatureExecution(spec jobs.Spec, generation int64, key string) (jobs.Spec, error) {
	payload, err := decodeJobPayload(spec.Payload, true, false)
	if err != nil || spec.Kind != FeaturesJobKind || payload.Execution == nil || generation < 1 {
		return jobs.Spec{}, errors.New("invalid catalog enrichment execution")
	}
	plan := *payload.Execution
	plan.Generation = generation
	plan.Key = key
	if !validGameExecution(payload.GameID, plan) {
		return jobs.Spec{}, errors.New("invalid catalog enrichment execution")
	}
	payload.Execution = &plan
	spec.Payload, err = json.Marshal(payload)
	return spec, err
}

func validGameExecution(gameID uuid.UUID, plan GameExecution) bool {
	if gameID == uuid.Nil || plan.ContentHash == "" || plan.TextModel == "" || plan.EmbeddingModel == "" || plan.Generation < 0 {
		return false
	}
	logical, err := featureLogicalKey(gameID, plan.ContentHash, plan.TextModel, plan.EmbeddingModel)
	if err != nil {
		return false
	}
	if plan.Generation == 0 {
		return plan.Key == logical
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(FeaturesJobKind+":"+logical)))
	return plan.Key == fmt.Sprintf("catalog:enrichment:v1:%s:%d:%s", gameID, plan.Generation, identity)
}

func (s *Service) executionForJob(payload JobPayload, lease jobs.Lease) (GameExecution, bool, error) {
	plan := GameExecution{TextModel: s.model, EmbeddingModel: s.embeddingModel}
	rootKey := strings.TrimSuffix(lease.IdempotencyKey, ":embedding:v1")
	if payload.Execution != nil {
		plan = *payload.Execution
		if !validGameExecution(payload.GameID, plan) || rootKey != plan.Key {
			return plan, false, errors.New("invalid catalog enrichment execution")
		}
		return plan, plan.TextModel == s.model && plan.EmbeddingModel == s.embeddingModel, nil
	}
	// Older persisted catalog payloads remain fenced even though they lack a plan.
	if strings.HasPrefix(rootKey, "catalog:enrichment:v1:"+payload.GameID.String()+":") {
		parts := strings.Split(rootKey, ":")
		if len(parts) != 6 {
			return plan, false, errors.New("invalid catalog enrichment execution")
		}
		generation, err := strconv.ParseInt(parts[4], 10, 64)
		if err != nil || generation < 1 {
			return plan, false, errors.New("invalid catalog enrichment execution")
		}
		plan.Generation, plan.Key = generation, rootKey
	} else if strings.HasPrefix(rootKey, "catalog:ai.features:v1:") {
		plan.Key = rootKey
	}
	return plan, true, nil
}

func (s *Service) loadGameExecution(ctx context.Context, id uuid.UUID, plan GameExecution, lease *jobs.Lease) (StoredGameEvidence, bool, error) {
	if repo, ok := s.repository.(gameExecutionRepository); ok {
		return repo.LoadGameExecution(ctx, id, plan, lease)
	}
	if plan.Key != "" {
		return StoredGameEvidence{}, false, errors.New("catalog enrichment requires a fenced repository")
	}
	stored, err := s.repository.LoadGameEvidence(ctx, id, s.model)
	return stored, err == nil, err
}

func featureEvidenceHash(stored StoredGameEvidence, model string) (string, GameEvidence, error) {
	evidence := gameEvidence(stored)
	_, evidence, err := normalizeGameEvidence(evidence)
	if err != nil {
		return "", evidence, err
	}
	hash, err := ContentHash(FeaturesJobKind, SchemaVersion, FeaturePromptVersion, model, evidence)
	return hash, evidence, err
}
