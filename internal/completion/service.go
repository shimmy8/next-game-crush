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
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
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

type projectionJobResult struct {
	Phase             string `json:"phase"`
	ProjectionChanged bool   `json:"projection_changed"`
	ProjectionHash    string `json:"projection_hash,omitempty"`
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
	if err := worker.Register(ClassificationJobKind, s.handleClassification); err != nil {
		return err
	}
	return worker.Register(ProjectionJobKind, s.handleProjection)
}

func (s *Service) RecordAchievementSnapshot(ctx context.Context, userID, gameID uuid.UUID, summary steam.AchievementSummary, observedAt time.Time) error {
	change, err := s.repository.SaveSteamSnapshot(ctx, userID, gameID, summary, observedAt)
	if err != nil {
		return err
	}
	if _, _, err := s.repository.RebuildProjection(ctx, userID, gameID, s.model, observedAt); err != nil {
		return err
	}
	if !change.DefinitionsChanged || change.DefinitionSetHash == "" {
		return nil
	}
	spec, err := s.PlanClassificationJob(ctx, gameID, change.DefinitionSetHash, 80)
	if err != nil {
		return err
	}
	_, err = s.queue.Enqueue(ctx, spec)
	return err
}

func (s *Service) EnqueueProjectionJobsForGame(ctx context.Context, gameID uuid.UUID) error {
	generationHash, err := s.repository.ActiveClassificationGenerationHash(ctx, gameID, s.model)
	if err != nil {
		return err
	}
	if generationHash == "" {
		return nil
	}
	users, err := s.repository.ListCompletionUsersForGame(ctx, gameID)
	if err != nil {
		return err
	}
	for _, userID := range users {
		spec, err := s.planProjectionJob(userID, gameID, generationHash, 80)
		if err != nil {
			return err
		}
		if _, err := s.queue.Enqueue(ctx, spec); err != nil {
			return fmt.Errorf("enqueue completion projection: %w", err)
		}
	}
	return nil
}

func (s *Service) planProjectionJob(userID, gameID uuid.UUID, generationHash string, priority int) (jobs.Spec, error) {
	generationHash = strings.TrimSpace(generationHash)
	if userID == uuid.Nil || gameID == uuid.Nil || generationHash == "" || priority < 1 || priority > 100 {
		return jobs.Spec{}, errors.New("projection job identity and priority are required")
	}
	payload, err := json.Marshal(ProjectionJobPayload{Version: 1, UserID: userID, GameID: gameID, GenerationHash: generationHash})
	if err != nil {
		return jobs.Spec{}, fmt.Errorf("encode completion projection job: %w", err)
	}
	keyHash := sha256.Sum256([]byte(strings.Join([]string{userID.String(), gameID.String(), generationHash, ProjectionAlgorithmVersion}, "\x00")))
	return jobs.Spec{Kind: ProjectionJobKind, Stage: "completion_projection", IdempotencyKey: ProjectionJobKind + ":v1:" + hex.EncodeToString(keyHash[:]), Payload: payload, MaxAttempts: 3, Priority: priority, UserID: userID}, nil
}

func (s *Service) handleProjection(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodeProjectionJobPayload(lease.Payload)
	if err != nil {
		return err
	}
	var result projectionJobResult
	if len(lease.Result) > 0 && string(lease.Result) != "null" {
		if err := json.Unmarshal(lease.Result, &result); err != nil {
			return errors.New("unsupported completion projection result schema")
		}
	}
	if result.Phase == "" {
		projection, changed, err := s.repository.rebuildProjection(ctx, payload.UserID, payload.GameID, s.model, time.Now().UTC(), func(ctx context.Context, tx pgx.Tx, projection Projection, changed bool) error {
			persisted := projectionJobResult{Phase: "complete", ProjectionChanged: false}
			if changed {
				persisted = projectionJobResult{Phase: "profile", ProjectionChanged: true, ProjectionHash: hashProjection(projection)}
			}
			encoded, err := json.Marshal(persisted)
			if err != nil {
				return fmt.Errorf("encode completion projection result: %w", err)
			}
			command, err := tx.Exec(ctx, `UPDATE sync_jobs SET result=$1,updated_at=now()
				WHERE id=$2 AND status='running' AND worker_id=$3 AND fencing_token=$4 AND lease_until>now()`, encoded, lease.ID, lease.WorkerID, lease.FencingToken)
			if err != nil {
				return fmt.Errorf("record completion projection result: %w", err)
			}
			if command.RowsAffected() != 1 {
				return jobs.ErrLeaseLost
			}
			result = persisted
			return nil
		})
		if err != nil {
			return err
		}
		if changed && result.ProjectionHash == "" {
			result = projectionJobResult{Phase: "profile", ProjectionChanged: true, ProjectionHash: hashProjection(projection)}
		}
	}
	profilePayload, _ := json.Marshal(map[string]any{"version": 1, "user_id": payload.UserID})
	keyPrefix := projectionRebuildKeyPrefix(payload.UserID, payload.GameID, payload.GenerationHash, lease.ID, result.ProjectionHash)
	if result.Phase == "profile" {
		ready, err := s.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Kind: "profile.rebuild", Stage: "profile", Required: true, IdempotencyKey: keyPrefix + ":profile:v1", Payload: profilePayload, MaxAttempts: 3, Priority: lease.Priority, UserID: payload.UserID}})
		if err != nil {
			return err
		}
		if !ready {
			return jobs.ErrDeferred
		}
		result.Phase = "recommendations"
		if err := s.queue.RecordResult(ctx, lease, result); err != nil {
			return err
		}
	}
	if result.Phase == "recommendations" {
		ready, err := s.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Kind: "recommendations.rebuild", Stage: "recommendations", Required: true, IdempotencyKey: keyPrefix + ":recommendations:v1", Payload: profilePayload, MaxAttempts: 3, Priority: lease.Priority, UserID: payload.UserID}})
		if err != nil {
			return err
		}
		if !ready {
			return jobs.ErrDeferred
		}
		result.Phase = "complete"
		return s.queue.RecordResult(ctx, lease, result)
	}
	if result.Phase != "complete" {
		return errors.New("unsupported completion projection result phase")
	}
	return nil
}

func decodeProjectionJobPayload(raw json.RawMessage) (ProjectionJobPayload, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var payload ProjectionJobPayload
	if err := decoder.Decode(&payload); err != nil || payload.Version != 1 || payload.UserID == uuid.Nil || payload.GameID == uuid.Nil || strings.TrimSpace(payload.GenerationHash) == "" {
		return ProjectionJobPayload{}, errors.New("unsupported completion projection payload schema")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ProjectionJobPayload{}, errors.New("completion projection payload contains trailing data")
	}
	return payload, nil
}

func hashProjection(projection Projection) string {
	encoded, _ := json.Marshal(projection)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func projectionRebuildKeyPrefix(userID, gameID uuid.UUID, generationHash string, transitionID uuid.UUID, projectionHash string) string {
	hash := sha256.Sum256([]byte(strings.Join([]string{userID.String(), gameID.String(), generationHash, transitionID.String(), ProjectionAlgorithmVersion, projectionHash}, "\x00")))
	return ProjectionJobKind + ":rebuild:v1:" + hex.EncodeToString(hash[:])
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
