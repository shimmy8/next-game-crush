package completion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
)

type Service struct {
	repository *SQLRepository
	classifier SemanticClassifier
	model      string
	queue      *jobs.Queue
	published  func(context.Context, uuid.UUID) error
}

type classificationJobResult struct {
	PublishedHookPending bool `json:"published_hook_pending"`
}

func NewService(repository *SQLRepository, classifier SemanticClassifier, model string, queue *jobs.Queue) (*Service, error) {
	if repository == nil || classifier == nil || queue == nil || strings.TrimSpace(model) == "" {
		return nil, errors.New("completion repository, classifier, model, and queue are required")
	}
	return &Service{repository: repository, classifier: classifier, model: strings.TrimSpace(model), queue: queue}, nil
}

func (s *Service) SetClassificationPublishedHook(hook func(context.Context, uuid.UUID) error) {
	s.published = hook
}

func (s *Service) PlanClassificationJob(_ context.Context, gameID uuid.UUID, definitionSetHash string, priority int) (jobs.Spec, error) {
	definitionSetHash = strings.TrimSpace(definitionSetHash)
	if gameID == uuid.Nil || definitionSetHash == "" || priority < 1 || priority > 100 {
		return jobs.Spec{}, errors.New("classification job identity and priority are required")
	}
	keyHash := sha256.Sum256([]byte(strings.Join([]string{gameID.String(), definitionSetHash, s.model, ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion}, "\x00")))
	key := ClassificationJobKind + ":v1:" + hex.EncodeToString(keyHash[:])
	payload, err := json.Marshal(JobPayload{Version: ClassificationJobPayloadVersion, GameID: gameID, DefinitionSetHash: definitionSetHash})
	if err != nil {
		return jobs.Spec{}, fmt.Errorf("encode classification job: %w", err)
	}
	return jobs.Spec{Kind: ClassificationJobKind, Stage: "achievement_classification", IdempotencyKey: key, Payload: payload, MaxAttempts: 3, Priority: priority, UserID: uuid.Nil}, nil
}

func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	return worker.Register(ClassificationJobKind, s.handleClassification)
}

func (s *Service) runClassification(ctx context.Context, payload JobPayload) error {
	_, published, err := s.classifyAndPublish(ctx, payload)
	if err != nil || !published || s.published == nil {
		return err
	}
	return s.published(ctx, payload.GameID)
}

func (s *Service) handleClassification(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodeClassificationJobPayload(lease.Payload)
	if err != nil {
		return err
	}
	// Persist intent before publication. If the process stops after the database
	// commit, the replayed lease still knows that game-scoped fanout is owed.
	if err := s.queue.RecordResult(ctx, lease, classificationJobResult{PublishedHookPending: true}); err != nil {
		return err
	}
	active, _, err := s.classifyAndPublish(ctx, payload)
	if err != nil {
		return err
	}
	if active && s.published != nil {
		if err := s.published(ctx, payload.GameID); err != nil {
			return err
		}
	}
	return s.queue.RecordResult(ctx, lease, classificationJobResult{PublishedHookPending: false})
}

func (s *Service) classifyAndPublish(ctx context.Context, payload JobPayload) (bool, bool, error) {
	evidence, reusable, currentHash, err := s.repository.LoadDefinitionGenerationInput(ctx, payload.GameID, s.model, ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion)
	if err != nil {
		return false, false, err
	}
	if currentHash != payload.DefinitionSetHash || len(evidence.Achievements) == 0 {
		return false, false, nil
	}
	definitions := make(map[string]ai.AchievementSemanticItem, len(evidence.Achievements))
	results := make(map[string]StagedClassification, len(evidence.Achievements))
	unresolved := make([]ai.AchievementSemanticItem, 0, len(evidence.Achievements))
	for _, item := range evidence.Achievements {
		definitions[item.EvidenceKey] = item
		if previous, ok := reusable[item.APIName]; ok {
			results[item.APIName] = StagedClassification{APIName: item.APIName, DefinitionContentHash: previous.DefinitionContentHash, Class: previous.Class, InputContentHash: previous.InputContentHash, Source: ClassificationSourceReused}
			continue
		}
		if class, ok := ClassifyLexically(item.Name, item.Description); ok {
			results[item.APIName] = StagedClassification{APIName: item.APIName, DefinitionContentHash: definitionContentHashForEvidence(item, evidence), Class: class, Source: ClassificationSourceLexical}
			continue
		}
		unresolved = append(unresolved, item)
	}
	if len(unresolved) > 0 {
		for start := 0; start < len(unresolved); start += 200 {
			end := min(start+200, len(unresolved))
			chunk := unresolved[start:end]
			request := evidence
			request.Achievements = chunk
			output, classifyErr := s.classifier.ClassifyAchievementSemantics(ctx, request)
			if classifyErr != nil {
				if providerUnavailable(classifyErr) {
					active, activeErr := s.repository.HasActiveClassificationGeneration(ctx, payload.GameID, s.model, ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion)
					if activeErr != nil {
						return false, false, activeErr
					}
					if active {
						if errors.Is(classifyErr, ai.ErrCapabilityDisabled) {
							return false, false, nil
						}
						return false, false, classifyErr
					}
					for _, item := range unresolved[start:] {
						results[item.APIName] = StagedClassification{APIName: item.APIName, DefinitionContentHash: definitionContentHashForEvidence(item, evidence), Class: ai.AchievementSemanticUnknown, Source: ClassificationSourceUnknown}
					}
					break
				}
				return false, false, classifyErr
			}
			expectedKeys := make([]string, len(chunk))
			for index, item := range chunk {
				expectedKeys[index] = item.EvidenceKey
			}
			if err := ai.ValidateAchievementSemanticClassification(output, expectedKeys); err != nil {
				return false, false, err
			}
			for _, result := range output.Results {
				item := definitions[result.EvidenceKey]
				results[item.APIName] = StagedClassification{APIName: item.APIName, DefinitionContentHash: definitionContentHashForEvidence(item, evidence), Class: result.Class, Source: ClassificationSourceOpenAI}
			}
		}
	}
	staged := make([]StagedClassification, 0, len(results))
	for _, result := range results {
		staged = append(staged, result)
	}
	sort.Slice(staged, func(i, j int) bool { return staged[i].APIName < staged[j].APIName })
	published, err := s.repository.PublishClassificationGeneration(ctx, payload.GameID, s.model, ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, payload.DefinitionSetHash, staged, time.Now().UTC())
	if err != nil {
		return false, false, err
	}
	active, err := s.repository.IsClassificationGenerationActive(ctx, payload.GameID, s.model, ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, payload.DefinitionSetHash)
	return active, published, err
}

func providerUnavailable(err error) bool {
	if errors.Is(err, ai.ErrCapabilityDisabled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var requestError *ai.RequestError
	if errors.As(err, &requestError) {
		return requestError.Retryable()
	}
	return errors.Is(err, ai.ErrHTTP)
}

func definitionContentHashForEvidence(item ai.AchievementSemanticItem, evidence ai.AchievementSemanticEvidence) string {
	return hashDefinition(evidence.GameName, item.APIName, item.Name, item.Description)
}

func decodeClassificationJobPayload(raw json.RawMessage) (JobPayload, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var payload JobPayload
	if err := decoder.Decode(&payload); err != nil || payload.Version != ClassificationJobPayloadVersion || payload.GameID == uuid.Nil || strings.TrimSpace(payload.DefinitionSetHash) == "" {
		return JobPayload{}, errors.New("unsupported achievement classification payload schema")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return JobPayload{}, errors.New("achievement classification payload contains trailing data")
	}
	return payload, nil
}
