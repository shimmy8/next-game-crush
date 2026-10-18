package recommendations

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/profile"
)

func TestBuildGroundingEvidenceUsesConcreteCandidateContext(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	candidateID := uuid.MustParse("00000000-0000-0000-0000-000000000099")
	anchors := []EvidenceAnchor{
		{Name: "SOMA", Signal: profile.Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: evidenceVector(0), PlaytimeMinutes: 900, RecentMinutes: 30, LastPlayedAt: asOf, AchievementRatio: .8}, Features: FeatureValues{Themes: []string{"Psychological Horror"}, Mechanics: []string{"Exploration"}}},
		{Name: "Short Story", Signal: profile.Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), Vector: evidenceVector(0), CompletionWeight: 1.5, LastPlayedAt: asOf.Add(-10 * 365 * 24 * time.Hour)}, Features: FeatureValues{Themes: []string{"Psychological Horror"}}},
		{Name: "Overridden", Signal: profile.Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000004"), Vector: evidenceVector(0), CompletionWeight: 2.25, CompletionOverridden: true}, Features: FeatureValues{Themes: []string{"Psychological Horror"}}},
		{Name: "Signalis", Signal: profile.Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: evidenceVector(0), PlaytimeMinutes: 300, Likes: 1}, Features: FeatureValues{Themes: []string{"psychological horror"}, Modes: []string{"Single Player"}}},
		{Name: "Disliked", Signal: profile.Signal{GameID: uuid.New(), Vector: evidenceVector(0), PlaytimeMinutes: 1000, Dislikes: 1}, Features: FeatureValues{Themes: []string{"Psychological Horror"}}},
		{Name: "Candidate", Signal: profile.Signal{GameID: candidateID, Vector: evidenceVector(0), Likes: 1}},
	}
	r := Recommendation{GameID: candidateID, Factors: Factors{PositiveSimilarity: .8, Wishlist: 1, TagAffinity: .9, PlaytimeAffinity: .7, RecentActivityAffinity: .6, AchievementAffinity: .5, CompletionAffinity: .5, PositiveFeedback: 1, Novelty: 1}, Weights: DefaultWeights()}
	got := BuildGroundingEvidence(r, CandidateEvidence{GameID: candidateID, Vector: evidenceVector(0), Features: FeatureValues{Themes: []string{" psychological horror "}, Mechanics: []string{"Exploration"}, Modes: []string{"single player"}}}, anchors, asOf)

	wantKinds := []EvidenceKind{EvidenceFeatureOverlap, EvidenceSimilarGame, EvidencePlaytimeAnchor, EvidenceRecentActivityAnchor, EvidenceCompletionAnchor, EvidenceAchievementAnchor, EvidencePositiveFeedback, EvidenceWishlistContext}
	var gotKinds []EvidenceKind
	for _, item := range got {
		gotKinds = append(gotKinds, item.Kind)
		if strings.Contains(item.PublicText, "Disliked") {
			t.Fatalf("negative anchor leaked into evidence: %#v", item)
		}
		if strings.Contains(item.PublicText, "Overridden") {
			t.Fatalf("overridden completion leaked into evidence: %#v", item)
		}
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("kinds = %#v, want %#v; evidence=%#v", gotKinds, wantKinds, got)
	}
	if got[0].PublicText != "Совпадают темы и механики: psychological horror, Exploration, single player." {
		t.Fatalf("feature copy = %q", got[0].PublicText)
	}
	if got[1].PublicText != "Игры с похожим профилем: SOMA, Signalis." {
		t.Fatalf("similar-game copy = %q", got[1].PublicText)
	}
	if got[2].PublicText != "Есть сходство с игрой SOMA, которой вы уделили много времени." || got[3].PublicText != "Есть сходство с игрой SOMA, которую вы запускали недавно." {
		t.Fatalf("activity anchors = %#v", got[2:4])
	}
	if got[4].PublicText != "Есть сходство с пройденной вами игрой Short Story." {
		t.Fatalf("completion copy = %q", got[4].PublicText)
	}
	if got[6].PublicText != "Вы уже отмечали эту игру как понравившуюся." {
		t.Fatalf("feedback copy = %q", got[6].PublicText)
	}
}

func TestBuildGroundingEvidenceDiffersWithFeatureOverlap(t *testing.T) {
	asOf := time.Now().UTC()
	anchor := EvidenceAnchor{Name: "Anchor", Signal: profile.Signal{GameID: uuid.New(), Vector: evidenceVector(0), Likes: 1}, Features: FeatureValues{Themes: []string{"horror", "science fiction"}}}
	r := Recommendation{Factors: Factors{TagAffinity: 1}, Weights: DefaultWeights()}
	first := BuildGroundingEvidence(r, CandidateEvidence{Vector: evidenceVector(1), Features: FeatureValues{Themes: []string{"horror"}}}, []EvidenceAnchor{anchor}, asOf)
	second := BuildGroundingEvidence(r, CandidateEvidence{Vector: evidenceVector(1), Features: FeatureValues{Themes: []string{"science fiction"}}}, []EvidenceAnchor{anchor}, asOf)
	if len(first) != 1 || len(second) != 1 || first[0].PublicText == second[0].PublicText {
		t.Fatalf("candidate-specific evidence not produced: %#v / %#v", first, second)
	}
}

func TestBuildGroundingEvidenceTieBreaksNamesDeterministically(t *testing.T) {
	asOf := time.Now().UTC()
	anchors := []EvidenceAnchor{
		{Name: "alpha", Signal: profile.Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), Vector: evidenceVector(0), Likes: 1}},
		{Name: "Alpha", Signal: profile.Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: evidenceVector(0), Likes: 1}},
		{Name: "Alpha", Signal: profile.Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: evidenceVector(0), Likes: 1}},
	}
	r := Recommendation{Factors: Factors{PositiveSimilarity: 1}, Weights: DefaultWeights()}
	got := BuildGroundingEvidence(r, CandidateEvidence{Vector: evidenceVector(0)}, anchors, asOf)
	if len(got) != 1 || !reflect.DeepEqual(got[0].SourceGameIDs, []uuid.UUID{anchors[2].Signal.GameID}) {
		t.Fatalf("tie order = %#v", got)
	}
}

func TestBuildGroundingEvidenceUsesEmbeddingCompletionAndRestoredLike(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		signal profile.Signal
	}{
		{name: "embedding only", signal: profile.Signal{GameID: uuid.New(), Vector: evidenceVector(0), CompletionWeight: 1.5}},
		{name: "later like restores completion evidence", signal: profile.Signal{GameID: uuid.New(), Vector: evidenceVector(0), CompletionWeight: 1.5, Dislikes: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			anchor := EvidenceAnchor{Name: "Completed Source", Signal: test.signal, Features: FeatureValues{Themes: []string{"different"}}}
			r := Recommendation{Factors: Factors{PositiveSimilarity: 1}, Weights: DefaultWeights()}
			got := SelectGroundingEvidence(BuildGroundingEvidence(r, CandidateEvidence{Vector: evidenceVector(0), Features: FeatureValues{Themes: []string{"candidate"}}}, []EvidenceAnchor{anchor}, asOf))
			var completion *GroundingEvidence
			for i := range got {
				if got[i].Kind == EvidenceCompletionAnchor {
					completion = &got[i]
				}
			}
			if completion == nil || completion.PublicText != "Есть сходство с пройденной вами игрой Completed Source." {
				t.Fatalf("completion evidence=%#v", got)
			}
		})
	}
}

func TestSelectGroundingEvidencePolicy(t *testing.T) {
	input := []GroundingEvidence{
		{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы и механики: horror.", Strength: 20},
		{ID: "feature-overlap-2", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы и механики: exploration.", Strength: 9},
		{ID: "similar-game-1", Kind: EvidenceSimilarGame, PublicText: "Игры с похожим профилем: SOMA.", Strength: 8},
		{ID: "too-small-absolute", Kind: EvidencePlaytimeAnchor, PublicText: "Есть сходство с игрой A, которой вы уделили много времени.", Strength: .49},
		{ID: "too-small-relative", Kind: EvidenceAchievementAnchor, PublicText: "Есть сходство с игрой B, где у вас заметный прогресс достижений.", Strength: .75},
		{ID: "wishlist-context-1", Kind: EvidenceWishlistContext, PublicText: "Вы уже добавили эту игру в желаемое — следим за датой выхода.", Strength: 0},
	}
	got := SelectGroundingEvidence(input)
	wantIDs := []string{"feature-overlap-1", "similar-game-1", "wishlist-context-1"}
	var gotIDs []string
	for _, item := range got {
		gotIDs = append(gotIDs, item.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("selected = %#v, want %v", got, wantIDs)
	}

	membership := SelectGroundingEvidence(input[len(input)-1:])
	if len(membership) != 1 || membership[0].Kind != EvidenceWishlistContext {
		t.Fatalf("membership-only selection = %#v", membership)
	}
}

func TestPublicAndDeterministicEvidenceSafety(t *testing.T) {
	if StoredEvidenceVersion != 3 {
		t.Fatalf("stored evidence version = %d, want 3", StoredEvidenceVersion)
	}
	valid := GroundingEvidence{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы и механики: horror.", Strength: 5}
	unsafe := []GroundingEvidence{
		{ID: "", Kind: EvidenceFeatureOverlap, PublicText: valid.PublicText, Strength: 5},
		{ID: "bad", Kind: "novelty", PublicText: "Добавляет новое.", Strength: 5},
		{ID: "bad", Kind: EvidenceFeatureOverlap, PublicText: `{"score": 1}`, Strength: 5},
		{ID: "bad-camel", Kind: EvidenceFeatureOverlap, PublicText: "Поле recentActivityAffinity совпало.", Strength: 5},
		{ID: "bad-completion", Kind: EvidenceCompletionAnchor, PublicText: "Поле completionAffinityWeight совпало.", Strength: 5},
		{ID: "bad-private", Kind: EvidenceFeatureOverlap, PublicText: "Внутренний sourceGameIds найден.", Strength: 5},
		{ID: "bad-score", Kind: EvidenceFeatureOverlap, PublicText: "score", Strength: 5},
		{ID: "bad-json-fragment", Kind: EvidenceFeatureOverlap, PublicText: `Поле "score": 1 совпало.`, Strength: 5},
		{ID: "bad-uuid", Kind: EvidenceFeatureOverlap, PublicText: "Источник 550e8400-e29b-41d4-a716-446655440000.", Strength: 5},
		{ID: "bad-steamid", Kind: EvidenceFeatureOverlap, PublicText: "Источник 76561198000000000.", Strength: 5},
		{ID: "bad-secret", Kind: EvidenceFeatureOverlap, PublicText: "Ключ sk-example_secret_123.", Strength: 5},
		{ID: "bad", Kind: EvidenceFeatureOverlap, PublicText: strings.Repeat("я", 241), Strength: 5},
		valid,
		valid,
	}
	got := PublicEvidence(unsafe)
	if !reflect.DeepEqual(got, []Evidence{{ID: valid.ID, Kind: valid.Kind, Text: valid.PublicText}}) {
		t.Fatalf("public evidence = %#v", got)
	}
	if got := DeterministicExplanation(nil); got != "Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно." {
		t.Fatalf("neutral copy = %q", got)
	}
	if got := DeterministicExplanation([]GroundingEvidence{{ID: "wishlist-context-1", Kind: EvidenceWishlistContext, PublicText: "Вы уже добавили эту игру в желаемое — следим за датой выхода."}}); got != "Вы уже добавили эту игру в желаемое — следим за датой выхода." {
		t.Fatalf("wishlist copy = %q", got)
	}
}

func TestMapStoredExplanationReadsSafeVersionTwoEvidence(t *testing.T) {
	item := GroundingEvidence{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы и механики: horror.", Strength: 5}
	raw, err := json.Marshal(StoredEvidence{Version: 2, Items: []GroundingEvidence{item}})
	if err != nil {
		t.Fatal(err)
	}
	recommendation := Recommendation{}
	mapStoredExplanation(&recommendation, pgtype.Text{String: item.PublicText, Valid: true}, raw, false, ExplanationSourceDeterministic, pgtype.Text{})
	if recommendation.ExplanationSource != ExplanationSourceDeterministic || !reflect.DeepEqual(recommendation.Evidence, []Evidence{{ID: item.ID, Kind: item.Kind, Text: item.PublicText}}) {
		t.Fatalf("legacy v2 evidence=%#v", recommendation)
	}
}

func TestMapStoredExplanationRejectsVersionTwoCompletionKind(t *testing.T) {
	item := GroundingEvidence{ID: "completion-anchor-1", Kind: EvidenceCompletionAnchor, PublicText: "Есть сходство с пройденной вами игрой SOMA.", Strength: 5}
	raw, err := json.Marshal(StoredEvidence{Version: 2, Items: []GroundingEvidence{item}})
	if err != nil {
		t.Fatal(err)
	}
	recommendation := Recommendation{}
	mapStoredExplanation(&recommendation, pgtype.Text{String: item.PublicText, Valid: true}, raw, false, ExplanationSourceDeterministic, pgtype.Text{})
	if recommendation.ExplanationSource != ExplanationSourceNone || len(recommendation.Evidence) != 0 {
		t.Fatalf("v2 accepted v3-only completion evidence: %#v", recommendation)
	}
}

func TestUnsupportedNovelty(t *testing.T) {
	r := Recommendation{Factors: Factors{Novelty: 1}, Weights: DefaultWeights()}
	built := BuildGroundingEvidence(r, CandidateEvidence{}, nil, time.Now())
	if len(built) != 0 || len(PublicEvidence(built)) != 0 || strings.Contains(DeterministicExplanation(built), "Добавляет новое") {
		t.Fatalf("novelty leaked: %#v / %q", built, DeterministicExplanation(built))
	}
}

func TestBuildGroundingEvidenceRejectsMalformedInputs(t *testing.T) {
	r := Recommendation{Factors: Factors{PositiveSimilarity: 1, TagAffinity: 1}, Weights: DefaultWeights()}
	anchors := []EvidenceAnchor{
		{Name: `{"game":"bad"}`, Signal: profile.Signal{GameID: uuid.New(), Vector: evidenceVector(0), Likes: 1}, Features: FeatureValues{Themes: []string{"novelty", " ", strings.Repeat("x", 121)}}},
		{Name: "Technical", Signal: profile.Signal{GameID: uuid.New(), Vector: evidenceVector(1), Likes: 1}, Features: FeatureValues{Themes: []string{"negativeSimilarity", "sourceGameIds", "score", `"score": 1`, "550e8400-e29b-41d4-a716-446655440000", "76561198000000000", "sk-example_secret_123"}}},
		{Name: "Valid", Signal: profile.Signal{GameID: uuid.New(), Vector: []float32{float32(math.NaN())}, Likes: 1}},
	}
	got := BuildGroundingEvidence(r, CandidateEvidence{Vector: evidenceVector(0), Features: FeatureValues{Themes: []string{"novelty", strings.Repeat("x", 121), "negativeSimilarity", "sourceGameIds", "score", `"score": 1`, "550e8400-e29b-41d4-a716-446655440000", "76561198000000000", "sk-example_secret_123"}}}, anchors, time.Now())
	if len(got) != 0 {
		t.Fatalf("unsafe evidence = %#v", got)
	}
}

func TestBuildGroundingEvidenceAllowsOrdinaryNamesWithTechnicalSyllables(t *testing.T) {
	r := Recommendation{Factors: Factors{PositiveSimilarity: 1, TagAffinity: 1}, Weights: DefaultWeights()}
	anchor := EvidenceAnchor{
		Name:     "Factorio",
		Signal:   profile.Signal{GameID: uuid.New(), Vector: evidenceVector(0), Likes: 1},
		Features: FeatureValues{Mechanics: []string{"vector graphics"}},
	}
	got := BuildGroundingEvidence(r, CandidateEvidence{Vector: evidenceVector(0), Features: FeatureValues{Mechanics: []string{"vector graphics"}}}, []EvidenceAnchor{anchor}, time.Now())
	if len(got) != 2 || !strings.Contains(got[0].PublicText, "vector graphics") || !strings.Contains(got[1].PublicText, "Factorio") {
		t.Fatalf("ordinary evidence was over-filtered: %#v", got)
	}
}

func evidenceVector(index int) []float32 {
	vector := make([]float32, profile.Dimensions)
	vector[index] = 1
	return vector
}
