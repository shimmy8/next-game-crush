package recommendations

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"math"
	"next-game-crush/internal/preferences"
	"testing"
)

func TestModePreferenceLegacyJSONDefaultsToZero(t *testing.T) {
	var factors Factors
	var weights Weights
	if err := json.Unmarshal([]byte(`{"positiveSimilarity":0.4,"novelty":1}`), &factors); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"semanticSimilarity":25,"novelty":5}`), &weights); err != nil {
		t.Fatal(err)
	}
	if factors.NegativeModePreference != 0 || weights.NegativeModePreferenceWeight != 0 || Score(factors, weights).Score != 15 {
		t.Fatalf("legacy factors=%#v weights=%#v", factors, weights)
	}
}

func TestModePreferenceFactorUsesMaxAfterSourceExclusion(t *testing.T) {
	source, other := uuid.New(), uuid.New()
	a := preferences.Adjustment{ID: uuid.New(), SourceGameID: source, Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid", Confidence: .8, Strength: .5, Active: true}
	b := a
	b.ID = uuid.New()
	b.SourceGameID = other
	b.Confidence = .9
	b.Strength = 1
	if got := ModePreferenceFactor(uuid.New(), []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{a, b}); got != .9 {
		t.Fatalf("factor=%v", got)
	}
	if got := ModePreferenceFactor(source, []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{a}); got != 0 {
		t.Fatalf("own factor=%v", got)
	}
	if got := ModePreferenceFactor(source, []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{a, b}); got != .9 {
		t.Fatalf("other factor=%v", got)
	}
	b.Confidence = math.NaN()
	if got := ModePreferenceFactor(source, []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{b}); got != 0 {
		t.Fatalf("NaN=%v", got)
	}
	b.Confidence = 1
	b.Target = preferences.ModeMultiplayer
	if got := ModePreferenceFactor(source, []preferences.Mode{preferences.ModeSinglePlayer}, []preferences.Adjustment{b}); got != 0 {
		t.Fatalf("single=%v", got)
	}
	if got := ModePreferenceFactor(source, []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{b}); got != 1 {
		t.Fatalf("broad=%v", got)
	}
}
func TestModePenaltyIsSoftAndCalibrated(t *testing.T) {
	f := Factors{PositiveSimilarity: .4, Novelty: 1}
	w := DefaultWeights()
	if got := Score(f, w).Score; got != 15 {
		t.Fatalf("baseline=%v", got)
	}
	f.NegativeModePreference = 1
	if got := Score(f, w).Score; got != 10 {
		t.Fatalf("penalty=%v", got)
	}
	if !Eligible(Candidate{GameID: uuid.New(), Factors: f}) {
		t.Fatal("mode penalty excluded candidate")
	}
}

func TestModePreferenceFiniteClampingAndEvidenceTie(t *testing.T) {
	candidate := uuid.New()
	a := preferences.Adjustment{ID: uuid.MustParse("00000000-0000-4000-8000-000000000001"), SourceGameID: uuid.New(), Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid", Active: true, Confidence: 1, Strength: 1}
	for _, test := range []struct {
		name                       string
		confidence, strength, want float64
	}{{"above one", 2, .2, .4}, {"product clamp", 1, 5, 1}, {"negative", -1, 1, 0}, {"negative both", -1, -1, 0}, {"nan", 1, math.NaN(), 0}, {"infinite", math.Inf(1), 1, 0}} {
		t.Run(test.name, func(t *testing.T) {
			a.Confidence = test.confidence
			a.Strength = test.strength
			if got := ModePreferenceFactor(candidate, []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{a}); got != test.want {
				t.Fatalf("factor=%v want=%v", got, test.want)
			}
		})
	}
	a.Confidence = 1
	a.Strength = 1
	b := a
	b.Target = preferences.ModeMultiplayer
	got := ModePreferenceEvidence(candidate, []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{b, a})
	if got == nil || got.ID != "negative-mode-preference:co-op" || got.Text != preferences.ModeEvidence(preferences.ModeCoop) {
		t.Fatalf("evidence=%#v", got)
	}
	a.Active = false
	if got := ModePreferenceFactor(candidate, []preferences.Mode{preferences.ModeCoop}, []preferences.Adjustment{a}); got != 0 {
		t.Fatalf("inactive=%v", got)
	}
	a.Active = true
	if got := ModePreferenceFactor(candidate, nil, []preferences.Adjustment{a}); got != 0 {
		t.Fatalf("unknown=%v", got)
	}
	if got := ModePreferenceFactor(candidate, []preferences.Mode{preferences.ModeMultiplayer}, []preferences.Adjustment{a}); got != 0 {
		t.Fatalf("generic multiplayer inferred co-op=%v", got)
	}
}

func TestModeCorrectionGroundingReservesHumanEvidenceSlot(t *testing.T) {
	correction := GroundingEvidence{ID: "negative-mode-preference:co-op", Kind: EvidenceNegativeModePreference, PublicText: preferences.ModeEvidence(preferences.ModeCoop), Strength: .9}
	selected := SelectGroundingEvidence([]GroundingEvidence{
		{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы и механики.", Strength: 15},
		{ID: "similar-game-1", Kind: EvidenceSimilarGame, PublicText: "Похожа на любимые игры.", Strength: 12},
		{ID: "wishlist-context-1", Kind: EvidenceWishlistContext, PublicText: "Игра есть в желаемом.", Strength: 0}, correction,
	})
	if len(selected) != 3 || !hasEvidenceID(PublicEvidence(selected), correction.ID) || !hasEvidenceID(PublicEvidence(selected), "wishlist-context-1") {
		t.Fatalf("selected=%#v", selected)
	}
	correction.PublicText = "Учитываем private-feedback-canary"
	if validGroundingEvidence(correction) {
		t.Fatal("forged correction copy accepted")
	}
	legacy := publicEvidence(Recommendation{Factors: Factors{NegativeModePreference: 1}})
	if len(legacy) != 1 || legacy[0].ID != "negative-mode-preference" {
		t.Fatalf("generic legacy=%#v", legacy)
	}
}

func TestModeCorrectionPresentationRequiresPositivePersistedFactor(t *testing.T) {
	text := preferences.ModeEvidence(preferences.ModeCoop)
	raw, err := json.Marshal(StoredEvidence{Version: StoredEvidenceVersion, Items: []GroundingEvidence{{ID: "negative-mode-preference:co-op", Kind: EvidenceNegativeModePreference, PublicText: text, Strength: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	item := Recommendation{}
	mapStoredExplanation(&item, pgtype.Text{String: text, Valid: true}, raw, false, ExplanationSourceDeterministic, pgtype.Text{})
	if len(item.Evidence) != 0 {
		t.Fatalf("correction shown without positive persisted factor: %#v", item.Evidence)
	}
	direct := safePresentation(Recommendation{ExplanationSource: ExplanationSourceDeterministic, Explanation: text, Evidence: []Evidence{{ID: "negative-mode-preference:co-op", Kind: EvidenceNegativeModePreference, Text: text}}})
	if len(direct.Evidence) != 0 {
		t.Fatalf("direct correction without factor=%#v", direct.Evidence)
	}
	legacy := safePresentation(Recommendation{ExplanationSource: ExplanationSourceNone, Factors: Factors{NegativeModePreference: .9}})
	if len(legacy.Evidence) != 1 || legacy.Evidence[0].ID != "negative-mode-preference" || legacy.ExplanationSource != ExplanationSourceDeterministic {
		t.Fatalf("legacy correction omitted=%#v", legacy)
	}
}
func TestActiveAdjustmentsDigestAndRankingBuildIdentity(t *testing.T) {
	a := preferences.Adjustment{ID: uuid.New(), SourceGameID: uuid.New(), Active: true, Confidence: 1, Strength: 1}
	b := a
	b.ID = uuid.New()
	if ActiveAdjustmentsDigest([]preferences.Adjustment{a, b}) != ActiveAdjustmentsDigest([]preferences.Adjustment{b, a}) {
		t.Fatal("order changed digest")
	}
	before := ActiveAdjustmentsDigest([]preferences.Adjustment{a})
	a.SourceGameID = uuid.New()
	if before == ActiveAdjustmentsDigest([]preferences.Adjustment{a}) {
		t.Fatal("source omitted")
	}
	a.Active = false
	if before == ActiveAdjustmentsDigest([]preferences.Adjustment{a}) {
		t.Fatal("disable omitted")
	}
	if RankingBuildKey("request", AlgorithmVersion, before) == RankingBuildKey("request2", AlgorithmVersion, before) {
		t.Fatal("request omitted")
	}
}
