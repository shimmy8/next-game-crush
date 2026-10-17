package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/profile"
)

const (
	JobKind           = "recommendations.rebuild"
	JobPayloadVersion = 1
)

type ExplanationClient interface {
	ExplainRecommendation(context.Context, ai.RecommendationEvidence) (ai.Explanation, error)
}
type Repository interface {
	Candidates(context.Context, uuid.UUID, profile.Profile, int) ([]Candidate, error)
	SaveRanking(context.Context, uuid.UUID, profile.Profile, []Recommendation, string) ([]Recommendation, int64, error)
	SaveExplanation(context.Context, uuid.UUID, string, string, bool, []Evidence, []Evidence) error
	PublishRanking(context.Context, uuid.UUID, int64) error
	Latest(context.Context, uuid.UUID) ([]Recommendation, error)
	Get(context.Context, uuid.UUID, uuid.UUID) (Recommendation, error)
}
type Service struct {
	repository Repository
	profiles   *profile.Service
	explainer  ExplanationClient
	limit      int
	queue      *jobs.Queue
}

func (s *Service) SetQueue(queue *jobs.Queue) { s.queue = queue }

func NewService(repository Repository, profiles *profile.Service, explainer ExplanationClient) (*Service, error) {
	if repository == nil || profiles == nil {
		return nil, errors.New("recommendation repository and profile service are required")
	}
	return &Service{repository: repository, profiles: profiles, explainer: explainer, limit: 100}, nil
}
func (s *Service) Rebuild(ctx context.Context, userID uuid.UUID) ([]Recommendation, error) {
	items, _, err := s.rebuild(ctx, userID, uuid.NewString())
	return items, err
}
func (s *Service) rebuild(ctx context.Context, userID uuid.UUID, buildKey string) ([]Recommendation, int64, error) {
	p, err := s.profiles.Latest(ctx, userID)
	if err != nil {
		return nil, 0, fmt.Errorf("load current profile: %w", err)
	}
	candidates, err := s.repository.Candidates(ctx, userID, p, s.limit)
	if err != nil {
		return nil, 0, fmt.Errorf("retrieve candidates: %w", err)
	}
	weights := DefaultWeights()
	output := make([]Recommendation, 0, len(candidates))
	for _, candidate := range candidates {
		if !Eligible(candidate) {
			continue
		}
		result := Score(candidate.Factors, weights)
		output = append(output, Recommendation{GameID: candidate.GameID, Name: candidate.Name, ProfileVersion: p.Version, AlgorithmVersion: AlgorithmVersion, Score: result.Score, Confidence: clamp(candidate.Factors.Confidence * p.Confidence), Factors: candidate.Factors, Weights: weights, Release: candidate.Release})
	}
	output = Sort(output)
	saved, rankingVersion, err := s.repository.SaveRanking(ctx, userID, p, output, buildKey)
	if err != nil {
		return nil, 0, fmt.Errorf("persist ranking: %w", err)
	}
	for i := range saved {
		evidence := factorEvidence(saved[i])
		explanation := Explain(ctx, s.explainer, saved[i].Name, evidence)
		cited := citedEvidence(evidence, explanation.EvidenceKeys)
		if err := s.repository.SaveExplanation(ctx, saved[i].ID, saved[i].Name, explanation.Text, explanation.Generated, evidence, cited); err != nil {
			return nil, 0, err
		}
		saved[i].Explanation, saved[i].GeneratedExplanation, saved[i].Evidence = explanation.Text, explanation.Generated, evidence
	}
	if err := s.repository.PublishRanking(ctx, userID, rankingVersion); err != nil {
		return nil, 0, err
	}
	return saved, rankingVersion, nil
}
func citedEvidence(evidence []Evidence, ids []string) []Evidence {
	allowed := map[string]Evidence{}
	for _, item := range evidence {
		allowed[item.ID] = item
	}
	result := make([]Evidence, 0, len(ids))
	for _, id := range ids {
		if item, ok := allowed[id]; ok {
			result = append(result, item)
		}
	}
	return result
}
func (s *Service) Latest(ctx context.Context, userID uuid.UUID) ([]Recommendation, error) {
	return s.repository.Latest(ctx, userID)
}
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Recommendation, error) {
	return s.repository.Get(ctx, userID, id)
}
func Explain(ctx context.Context, client ExplanationClient, name string, evidence []Evidence) ai.Explanation {
	fallback := func() ai.Explanation {
		facts := make([]string, 0, len(evidence))
		keys := make([]string, 0, len(evidence))
		for _, item := range evidence {
			facts = append(facts, item.Text)
			keys = append(keys, item.ID)
		}
		out := ai.DeterministicExplanation(facts)
		out.EvidenceKeys = keys
		return out
	}
	if client == nil {
		return fallback()
	}
	items := make([]ai.EvidenceItem, 0, len(evidence))
	allowed := map[string]bool{}
	for _, item := range evidence {
		items = append(items, ai.EvidenceItem{Key: item.ID, Value: item.Text})
		allowed[item.ID] = true
	}
	out, err := client.ExplainRecommendation(ctx, ai.RecommendationEvidence{EvidenceKey: "recommendation-factors", GameName: name, Evidence: items})
	if err != nil || out.Text == "" || len(out.EvidenceKeys) == 0 {
		return fallback()
	}
	seen := map[string]bool{}
	for _, key := range out.EvidenceKeys {
		if !allowed[key] || seen[key] {
			return fallback()
		}
		seen[key] = true
	}
	out.Generated = true
	return out
}
func factorEvidence(r Recommendation) []Evidence {
	encoded, _ := json.Marshal(r.Factors)
	return []Evidence{{ID: "score", Text: fmt.Sprintf("deterministic score %.3f", r.Score)}, {ID: "factors", Text: string(encoded)}, {ID: "release-window", Text: r.Release.Precision + ":" + r.Release.Raw}}
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
		var p JobPayload
		if json.Unmarshal(lease.Payload, &p) != nil || p.Version != JobPayloadVersion || p.UserID == uuid.Nil {
			return errors.New("unsupported recommendations job payload schema")
		}
		result, rankingVersion, err := s.rebuild(ctx, p.UserID, lease.IdempotencyKey)
		if err != nil {
			return err
		}
		if s.queue != nil {
			profileValue, err := s.profiles.Latest(ctx, p.UserID)
			if err != nil {
				return err
			}
			if err := s.queue.RecordResult(ctx, lease, map[string]any{"profileVersion": profileValue.Version, "rankingVersion": rankingVersion, "algorithmVersion": AlgorithmVersion, "recommendationCount": len(result)}); err != nil {
				return err
			}
		}
		return nil
	})
}
