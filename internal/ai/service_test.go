package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

func TestInvalidOutputIsNeverPersisted(t *testing.T) {
	repository := &fakeRepository{game: StoredGameEvidence{Name: "Portal", Description: "Puzzle game"}}
	client := &fakeClient{}
	service, err := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.processFeatures(context.Background(), uuid.New()); !errors.Is(err, ErrInvalidOutput) && !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("error = %v", err)
	}
	if repository.savedFeatures {
		t.Fatal("invalid output was persisted")
	}
}

func TestContentHashCacheSkipsRemoteCall(t *testing.T) {
	repository := &fakeRepository{game: StoredGameEvidence{Name: "Portal", Description: "Puzzle game"}, cachedFeatures: true}
	client := &fakeClient{}
	service, _ := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
	if err := service.processFeatures(context.Background(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if client.featureCalls != 0 {
		t.Fatalf("remote calls = %d", client.featureCalls)
	}
}

func TestCatalogChangedHookReplaysAfterPostCommitFailure(t *testing.T) {
	for _, kind := range []string{FeaturesJobKind, EmbeddingJobKind} {
		t.Run(kind, func(t *testing.T) {
			gameID := uuid.New()
			repository := &fakeRepository{game: StoredGameEvidence{Name: "Portal"}}
			client := &fakeClient{
				features: GameFeatures{Themes: []string{"puzzle"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"title"}},
				vectors:  [][]float32{make([]float32, EmbeddingDimensions)},
			}
			service, err := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
			if err != nil {
				t.Fatal(err)
			}
			hookCalls := 0
			service.SetCatalogChangedHook(func(context.Context, uuid.UUID) error {
				hookCalls++
				if hookCalls == 1 {
					return errors.New("transient rebuild scheduling failure")
				}
				return nil
			})
			payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
			lease := jobs.Lease{Lane: jobs.LaneInteractive, Kind: kind, IdempotencyKey: "catalog:hook-replay:" + kind, Payload: payload, Attempts: 1}
			run := service.handleFeatures
			if kind == EmbeddingJobKind {
				run = service.handleEmbedding
			}
			if err := run(context.Background(), lease); err == nil {
				t.Fatal("first hook failure was not returned")
			}
			lease.Attempts = 2
			lease.Result, _ = json.Marshal(catalogChangeResult{Pending: true})
			if err := run(context.Background(), lease); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			if hookCalls != 2 {
				t.Fatalf("hook calls=%d, want replay after committed cache write", hookCalls)
			}
		})
	}
}

func TestCatalogChangedHookFailureDefersOwnedLease(t *testing.T) {
	db := openAIIntegrationStore(t)
	queue := jobs.NewQueue(db)
	gameID := uuid.New()
	repository := &fakeRepository{game: StoredGameEvidence{Name: "Portal"}}
	client := &fakeClient{
		features: GameFeatures{Themes: []string{"puzzle"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"title"}},
		vectors:  [][]float32{make([]float32, EmbeddingDimensions)},
	}
	service, err := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
	if err != nil {
		t.Fatal(err)
	}
	service.SetQueue(queue)
	hookCalls := 0
	service.SetCatalogChangedHook(func(context.Context, uuid.UUID) error {
		hookCalls++
		if hookCalls == 1 {
			return errors.New("temporary scheduler failure")
		}
		return nil
	})
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
	jobID, err := queue.Enqueue(context.Background(), jobs.Spec{Lane: jobs.LaneInteractive, Kind: EmbeddingJobKind, IdempotencyKey: "catalog:hook-replay:" + EmbeddingJobKind, Payload: payload, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(context.Background(), jobs.LaneInteractive, "hook-replay", time.Minute)
	if err != nil || lease == nil || lease.ID != jobID {
		t.Fatalf("claim=%+v err=%v", lease, err)
	}
	if err := service.handleEmbedding(context.Background(), *lease); !errors.Is(err, jobs.ErrDeferred) {
		t.Fatalf("first attempt=%v, want deferred", err)
	}
	if _, err := db.Pool.Exec(context.Background(), `UPDATE sync_jobs SET available_at=now() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	retry, err := queue.Claim(context.Background(), jobs.LaneInteractive, "hook-replay", time.Minute)
	if err != nil || retry == nil || retry.ID != jobID || retry.Attempts != 1 {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	err = service.handleEmbedding(context.Background(), *retry)
	if err != nil {
		t.Fatal(err)
	}
	if hookCalls != 2 {
		t.Fatalf("hook calls=%d, want 2", hookCalls)
	}
}

func TestFeatureParentNotifiesCatalogOnceAfterEmbeddingChild(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail_first_%t", failFirst), func(t *testing.T) {
			db := openAIIntegrationStore(t)
			queue := jobs.NewQueue(db)
			gameID := uuid.New()
			repository := &fakeRepository{game: StoredGameEvidence{Name: "Portal"}}
			client := &fakeClient{features: GameFeatures{Themes: []string{"puzzle"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"title"}}}
			service, _ := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
			service.SetQueue(queue)
			hookCalls := 0
			service.SetCatalogChangedHook(func(context.Context, uuid.UUID) error {
				hookCalls++
				if failFirst && hookCalls == 1 {
					return errors.New("temporary scheduler failure")
				}
				return nil
			})
			payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
			parentID, err := queue.Enqueue(context.Background(), jobs.Spec{Lane: jobs.LaneInteractive, Kind: FeaturesJobKind, IdempotencyKey: "catalog:feature-lifecycle", Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := queue.Claim(context.Background(), jobs.LaneInteractive, "feature-lifecycle", time.Minute)
			if err != nil || parent == nil || parent.ID != parentID {
				t.Fatalf("parent=%+v err=%v", parent, err)
			}
			if err := service.handleFeatures(context.Background(), *parent); !errors.Is(err, jobs.ErrDeferred) || hookCalls != 0 {
				t.Fatalf("first parent=%v hooks=%d", err, hookCalls)
			}
			repository.cachedEmbedding = true
			child, err := queue.Claim(context.Background(), jobs.LaneInteractive, "feature-lifecycle", time.Minute)
			if err != nil || child == nil || child.Kind != EmbeddingJobKind {
				t.Fatalf("child=%+v err=%v", child, err)
			}
			if err := service.handleEmbedding(context.Background(), *child); err != nil {
				t.Fatal(err)
			}
			if err := queue.Complete(context.Background(), *child); err != nil {
				t.Fatal(err)
			}
			parent, err = queue.Claim(context.Background(), jobs.LaneInteractive, "feature-lifecycle", time.Minute)
			if err != nil || parent == nil || parent.ID != parentID {
				t.Fatalf("resumed parent=%+v err=%v", parent, err)
			}
			err = service.handleFeatures(context.Background(), *parent)
			if failFirst {
				if !errors.Is(err, jobs.ErrDeferred) {
					t.Fatalf("feature hook failure=%v, want deferred", err)
				}
				if _, err := db.Pool.Exec(context.Background(), `UPDATE sync_jobs SET available_at=now() WHERE id=$1`, parentID); err != nil {
					t.Fatal(err)
				}
				parent, err = queue.Claim(context.Background(), jobs.LaneInteractive, "feature-lifecycle", time.Minute)
				if err != nil || parent == nil || parent.ID != parentID {
					t.Fatalf("hook retry parent=%+v err=%v", parent, err)
				}
				err = service.handleFeatures(context.Background(), *parent)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if failFirst {
				wantCalls = 2
			}
			if hookCalls != wantCalls {
				t.Fatalf("hook calls=%d, want %d", hookCalls, wantCalls)
			}
		})
	}
}

func TestRegisterHandlersRegistersAllVersionedKinds(t *testing.T) {
	service, _ := NewService(&fakeRepository{}, &fakeClient{}, "gpt-6-luna", "text-embedding-3-small")
	worker := jobs.NewWorker(nil, jobs.LaneInteractive, "registration-only")
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{FeaturesJobKind, EmbeddingJobKind, ProfileSummaryJobKind} {
		if err := worker.Register(kind, func(context.Context, jobs.Lease) error { return nil }); err == nil {
			t.Fatalf("handler %q was not registered", kind)
		}
	}
}

func TestPlanFeatureJobsUsesVersionedIdempotencyKeys(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	repository := &fakeRepository{gameIDs: []uuid.UUID{first, second}}
	service, _ := NewService(repository, &fakeClient{}, "gpt-6-luna", "text-embedding-3-small")
	specs, err := service.PlanFeatureJobs(context.Background(), uuid.New(), "steam.sync:root")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Kind != FeaturesJobKind || specs[0].IdempotencyKey != "steam.sync:root:ai.features:v1:"+first.String() {
		t.Fatalf("specs = %#v", specs)
	}
}

func TestNonFiniteEmbeddingIsNeverPersisted(t *testing.T) {
	vector := make([]float32, EmbeddingDimensions)
	vector[10] = float32(math.NaN())
	repository := &fakeRepository{game: StoredGameEvidence{Name: "Portal"}}
	client := &fakeClient{vectors: [][]float32{vector}}
	service, _ := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
	if err := service.processEmbedding(context.Background(), uuid.New()); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("error = %v", err)
	}
	if repository.savedEmbedding {
		t.Fatal("invalid embedding was persisted")
	}
}

func TestProfileSummaryIsNormalizedBeforePersistence(t *testing.T) {
	repository := &fakeRepository{profileEvidence: ProfileEvidence{EvidenceKey: "profile", Evidence: []EvidenceItem{{Key: "signal-1", Value: "puzzles"}}}}
	client := &fakeClient{profileSummary: ProfileSummary{
		Summary:      "  " + strings.Repeat("я", 650) + "  ",
		Preferences:  []string{" puzzles ", "", "puzzles", strings.Repeat("я", 145)},
		Avoidances:   nil,
		Uncertainty:  "  мало данных  ",
		EvidenceKeys: []string{"signal-1"},
	}}
	service, _ := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
	if err := service.processProfileSummary(context.Background(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if !repository.savedProfileSummary {
		t.Fatal("profile summary was not persisted")
	}
	if repository.profileSummary.Summary != strings.Repeat("я", 650) {
		t.Fatalf("summary was not trimmed before persistence")
	}
	if !reflect.DeepEqual(repository.profileSummary.Preferences, []string{"puzzles"}) {
		t.Fatalf("preferences = %#v", repository.profileSummary.Preferences)
	}
	if repository.profileSummary.Avoidances == nil || repository.profileSummary.Uncertainty != "мало данных" {
		t.Fatalf("profile summary = %#v", repository.profileSummary)
	}
}

type fakeRepository struct {
	game                StoredGameEvidence
	gameIDs             []uuid.UUID
	profileEvidence     ProfileEvidence
	cachedFeatures      bool
	cachedEmbedding     bool
	savedFeatures       bool
	savedEmbedding      bool
	savedProfileSummary bool
	profileSummary      ProfileSummary
}

func (f *fakeRepository) ListGameIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return f.gameIDs, nil
}

func (f *fakeRepository) LoadGameEvidence(context.Context, uuid.UUID, string) (StoredGameEvidence, error) {
	return f.game, nil
}
func (f *fakeRepository) HasGameFeatures(context.Context, uuid.UUID, string, string) (bool, error) {
	return f.cachedFeatures, nil
}
func (f *fakeRepository) SaveGameFeatures(context.Context, uuid.UUID, string, string, GameFeatures) error {
	f.savedFeatures = true
	f.cachedFeatures = true
	return nil
}
func (f *fakeRepository) HasGameEmbedding(context.Context, uuid.UUID, string, string) (bool, error) {
	return f.cachedEmbedding, nil
}
func (f *fakeRepository) SaveGameEmbedding(context.Context, uuid.UUID, string, string, []float32) error {
	f.savedEmbedding = true
	f.cachedEmbedding = true
	return nil
}
func (f *fakeRepository) LoadProfileEvidence(context.Context, uuid.UUID) (ProfileEvidence, error) {
	return f.profileEvidence, nil
}
func (f *fakeRepository) HasProfileSummary(context.Context, uuid.UUID, string, string) (bool, error) {
	return false, nil
}

func (f *fakeRepository) SaveProfileSummary(_ context.Context, _ uuid.UUID, _, _ string, value ProfileSummary) error {
	f.savedProfileSummary = true
	f.profileSummary = value
	return nil
}

type fakeClient struct {
	featureErr     error
	featureCalls   int
	features       GameFeatures
	vectors        [][]float32
	profileSummary ProfileSummary
}

func (f *fakeClient) ExtractGameFeatures(context.Context, GameEvidence) (GameFeatures, error) {
	f.featureCalls++
	return f.features, f.featureErr
}
func (f *fakeClient) SummarizeProfile(context.Context, ProfileEvidence) (ProfileSummary, error) {
	return f.profileSummary, nil
}
func (f *fakeClient) ExplainRecommendation(context.Context, RecommendationEvidence) (Explanation, error) {
	return Explanation{}, nil
}
func (f *fakeClient) ClassifyFeedback(context.Context, FeedbackText) (FeedbackClassification, error) {
	return FeedbackClassification{}, nil
}

func (f *fakeClient) ClassifyAchievementSemantics(context.Context, AchievementSemanticEvidence) (AchievementSemanticClassification, error) {
	return AchievementSemanticClassification{}, nil
}
func (f *fakeClient) Embed(context.Context, []string) ([][]float32, error) { return f.vectors, nil }
