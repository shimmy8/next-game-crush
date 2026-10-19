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

func TestCatalogFeaturePlanningPreservesLegacySemanticIdentity(t *testing.T) {
	gameID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	service := &Service{model: "text-v1", embeddingModel: "embedding-v1"}

	legacy, err := service.PlanGameFeatureJob(context.Background(), gameID, "content", 42)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := service.PlanCatalogGameFeatureJob(context.Background(), gameID, "content", 42)
	if err != nil {
		t.Fatal(err)
	}

	wantKey := "catalog:ai.features:v1:11111111-1111-4111-8111-111111111111:ec7907e0740f96381e6fd1b046f7c3a9e9f3a77427fe48b5ba138d4ec1c99fa6"
	wantPayload := `{"version":1,"game_id":"11111111-1111-4111-8111-111111111111","profile_id":"00000000-0000-0000-0000-000000000000","execution":{"content_hash":"content","text_model":"text-v1","embedding_model":"embedding-v1","generation":0,"key":"` + wantKey + `"}}`
	if legacy.Lane != jobs.LaneInteractive || legacy.MaxAttempts != 3 || legacy.Kind != FeaturesJobKind || legacy.Stage != "ai_features" || legacy.Priority != 42 {
		t.Fatalf("legacy spec changed: %+v", legacy)
	}
	if legacy.IdempotencyKey != wantKey || string(legacy.Payload) != wantPayload {
		t.Fatalf("legacy identity changed: key=%q payload=%s", legacy.IdempotencyKey, legacy.Payload)
	}
	if catalog.Lane != jobs.LaneCatalog || catalog.MaxAttempts != 4 {
		t.Fatalf("catalog retry contract = lane %q attempts %d", catalog.Lane, catalog.MaxAttempts)
	}
	if catalog.Kind != legacy.Kind || catalog.Stage != legacy.Stage || catalog.Priority != legacy.Priority || catalog.IdempotencyKey != legacy.IdempotencyKey || string(catalog.Payload) != string(legacy.Payload) {
		t.Fatalf("catalog planning changed semantic identity:\nlegacy=%+v\ncatalog=%+v", legacy, catalog)
	}
}

func TestFeaturePlanningRejectsInvalidIdentityOnBothLanes(t *testing.T) {
	service := &Service{model: "text-v1", embeddingModel: "embedding-v1"}
	tests := []struct {
		name     string
		gameID   uuid.UUID
		hash     string
		priority int
	}{
		{name: "nil game", hash: "content", priority: 50},
		{name: "blank hash", gameID: uuid.New(), hash: " \t", priority: 50},
		{name: "zero priority", gameID: uuid.New(), hash: "content"},
		{name: "priority above maximum", gameID: uuid.New(), hash: "content", priority: 101},
	}
	planners := []struct {
		name string
		plan func(context.Context, uuid.UUID, string, int) (jobs.Spec, error)
	}{
		{name: "legacy", plan: service.PlanGameFeatureJob},
		{name: "catalog", plan: service.PlanCatalogGameFeatureJob},
	}
	for _, planner := range planners {
		for _, test := range tests {
			t.Run(planner.name+"/"+test.name, func(t *testing.T) {
				if _, err := planner.plan(context.Background(), test.gameID, test.hash, test.priority); err == nil {
					t.Fatal("invalid enrichment identity was accepted")
				}
			})
		}
	}
}

func TestRegisterCatalogHandlersExcludesProfileSummary(t *testing.T) {
	service, _ := NewService(&fakeRepository{}, &fakeClient{}, "gpt-6-luna", "text-embedding-3-small")
	if err := service.RegisterCatalogHandlers(nil); err == nil {
		t.Fatal("nil catalog worker was accepted")
	}
	worker := jobs.NewWorker(nil, jobs.LaneCatalog, "catalog-registration-only")
	if err := service.RegisterCatalogHandlers(worker); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{FeaturesJobKind, EmbeddingJobKind} {
		if err := worker.Register(kind, func(context.Context, jobs.Lease) error { return nil }); err == nil {
			t.Fatalf("handler %q was not registered", kind)
		}
	}
	if err := worker.Register(ProfileSummaryJobKind, func(context.Context, jobs.Lease) error { return nil }); err != nil {
		t.Fatalf("profile summary leaked into catalog registration: %v", err)
	}
}

func TestEmbeddingChildInheritsFeatureLaneAndAttempts(t *testing.T) {
	for _, test := range []struct {
		name        string
		lane        jobs.Lane
		maxAttempts int
	}{
		{name: "legacy", lane: jobs.LaneInteractive, maxAttempts: 3},
		{name: "catalog", lane: jobs.LaneCatalog, maxAttempts: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openAIIntegrationStore(t)
			queue := jobs.NewQueue(db)
			gameID := uuid.New()
			repository := &fakeRepository{game: StoredGameEvidence{Name: "Portal"}}
			client := &fakeClient{features: GameFeatures{Themes: []string{"puzzle"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"title"}}}
			service, err := NewService(repository, client, "text-v1", "embedding-v1")
			if err != nil {
				t.Fatal(err)
			}
			service.SetQueue(queue)
			payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
			parentID, err := queue.Enqueue(context.Background(), jobs.Spec{Lane: test.lane, Kind: FeaturesJobKind, IdempotencyKey: "feature-lane:" + test.name, Payload: payload, MaxAttempts: test.maxAttempts})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := queue.Claim(context.Background(), test.lane, "feature-lane", time.Minute)
			if err != nil || parent == nil || parent.ID != parentID {
				t.Fatalf("parent=%+v err=%v", parent, err)
			}
			if err := service.handleFeatures(context.Background(), *parent); !errors.Is(err, jobs.ErrDeferred) {
				t.Fatalf("feature handler = %v, want deferred", err)
			}
			child, err := queue.Claim(context.Background(), test.lane, "embedding-lane", time.Minute)
			if err != nil || child == nil {
				t.Fatalf("child=%+v err=%v", child, err)
			}
			if child.Kind != EmbeddingJobKind || child.Lane != test.lane || child.MaxAttempts != test.maxAttempts {
				t.Fatalf("embedding child = kind %q lane %q attempts %d", child.Kind, child.Lane, child.MaxAttempts)
			}
		})
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
	if err := service.processProfileSummary(context.Background(), jobs.Lease{}, uuid.New()); err != nil {
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

func TestDisabledOpenAIPublishesDeterministicActivityFallback(t *testing.T) {
	repository := &fakeRepository{profileEvidence: ProfileEvidence{
		EvidenceKey: "profile", Evidence: []EvidenceItem{{Key: "signal-1", Value: "playtime_minutes=12"}},
		Facts: ActivityFacts{PlayedGames: 1, RecentlyPlayedGames: 1},
	}}
	client := &fakeClient{profileErr: ErrCapabilityDisabled}
	service, _ := NewService(repository, client, "gpt-6-luna", "text-embedding-3-small")
	if err := service.processProfileSummary(context.Background(), jobs.Lease{}, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if !repository.savedProfileSummary || repository.summarySource != "deterministic" || !strings.Contains(repository.profileSummary.Summary, "игровым временем — 1") {
		t.Fatalf("fallback publication = saved:%v source:%q summary:%q", repository.savedProfileSummary, repository.summarySource, repository.profileSummary.Summary)
	}
}

func TestCachedProfileSummaryStillPublishesDraft(t *testing.T) {
	repository := &fakeRepository{
		profileEvidence: ProfileEvidence{EvidenceKey: "profile", Evidence: []EvidenceItem{{Key: "signal-1", Value: "playtime_minutes=12"}}},
		cachedProfile:   ProfileSummary{Summary: strings.Repeat("я", 650)},
		cachedSummary:   true,
	}
	service, _ := NewService(repository, &fakeClient{}, "gpt-6-luna", "text-embedding-3-small")
	if err := service.processProfileSummary(context.Background(), jobs.Lease{}, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if !repository.savedProfileSummary || repository.summarySource != "openai" {
		t.Fatalf("cached summary was not published: %#v", repository)
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
	cachedProfile       ProfileSummary
	cachedSummary       bool
	summarySource       string
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
func (f *fakeRepository) LoadCachedProfileSummary(context.Context, uuid.UUID, string, string) (ProfileSummary, bool, error) {
	return f.cachedProfile, f.cachedSummary, nil
}

func (f *fakeRepository) PublishProfileSummary(_ context.Context, _ jobs.Lease, _ uuid.UUID, _, _ string, value ProfileSummary, source string) error {
	f.savedProfileSummary = true
	f.profileSummary = value
	f.summarySource = source
	return nil
}

func (f *fakeRepository) HandleTerminalProfileSummaryFailure(context.Context, jobs.Lease, uuid.UUID, string) (bool, error) {
	return false, nil
}

type fakeClient struct {
	featureErr     error
	featureCalls   int
	features       GameFeatures
	vectors        [][]float32
	profileSummary ProfileSummary
	profileErr     error
}

func (f *fakeClient) ExtractGameFeatures(context.Context, GameEvidence) (GameFeatures, error) {
	f.featureCalls++
	return f.features, f.featureErr
}
func (f *fakeClient) SummarizeProfile(context.Context, ProfileEvidence) (ProfileSummary, error) {
	return f.profileSummary, f.profileErr
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
