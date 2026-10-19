package recommendations

import (
	"testing"

	"github.com/google/uuid"
)

func TestScoreUsesAchievementCompletionWeights(t *testing.T) {
	weights := DefaultWeights()
	if weights != (Weights{ActivitySimilarity: 25, PositivePreferenceSimilarity: 10, NegativePreferenceSimilarity: -10, PositiveDirectFeedback: 20, NegativeDirectFeedback: -30, NegativeModePreference: -5, Wishlist: 0, TagAffinity: 15, PlaytimeAffinity: 10, AchievementAffinity: 5, RecentActivityAffinity: 5, CompletionAffinity: 5, Novelty: 5, DateUncertainty: -5}) || AlgorithmVersion != "ranking-v5-activity-overlay" {
		t.Fatalf("weights=%#v", weights)
	}
	if got := Score(Factors{Wishlist: 1}, weights); got.Score != 0 {
		t.Fatalf("wishlist score = %v, want 0", got.Score)
	}
	if Score(Factors{TagAffinity: .8}, weights).Score != Score(Factors{TagAffinity: .8, Wishlist: 1}, weights).Score {
		t.Fatal("wishlist membership changes affinity score")
	}
	if Score(Factors{ActivitySimilarity: 1}, weights).Score <= Score(Factors{Wishlist: 1}, weights).Score {
		t.Fatal("affinity discovery cannot outrank membership alone")
	}
	if got := Score(Factors{ActivitySimilarity: 2}, weights); got.Score != 25 {
		t.Fatalf("semantic score = %v, want capped 25", got.Score)
	}
	if got := Score(Factors{ActivitySimilarity: 1, NegativePreferenceSimilarity: 1}, weights); got.Score >= 25 {
		t.Fatalf("negative semantic profile did not soften similarity: %v", got.Score)
	}
	if got := Score(Factors{CompletionAffinity: 1}, weights); got.Score != 5 {
		t.Fatalf("completion score = %v, want 5", got.Score)
	}
	if got := Score(Factors{NegativeDirectFeedback: 1}, weights); got.Score != 0 {
		t.Fatalf("negative score = %v, want clamped 0", got.Score)
	}
	if got := Score(Factors{Wishlist: 1, DateUncertainty: 1}, weights); got.Score != 0 {
		t.Fatalf("uncertain wishlist score = %v, want 0", got.Score)
	}
	if got := Score(Factors{Wishlist: 1, PositiveDirectFeedback: 1, TagAffinity: 1, PlaytimeAffinity: 1, AchievementAffinity: 1, RecentActivityAffinity: 1, CompletionAffinity: 1, Novelty: 1, ActivitySimilarity: 1}, weights); got.Score != 90 {
		t.Fatalf("high score = %v, want 90", got.Score)
	}
	if got := Score(Factors{ActivitySimilarity: 1}, Weights{ActivitySimilarity: 200}); got.Score != 100 {
		t.Fatalf("oversized weights score = %v, want clamp 100", got.Score)
	}
}

func TestOverlayWeightsAreBoundedAndWishlistIsZero(t *testing.T) {
	weights := DefaultWeights()
	want := Weights{
		ActivitySimilarity: 25, PositivePreferenceSimilarity: 10, NegativePreferenceSimilarity: -10,
		PositiveDirectFeedback: 20, NegativeDirectFeedback: -30, NegativeModePreference: -5,
		Wishlist: 0, TagAffinity: 15, PlaytimeAffinity: 10, AchievementAffinity: 5,
		RecentActivityAffinity: 5, CompletionAffinity: 5, Novelty: 5, DateUncertainty: -5,
	}
	if weights != want || AlgorithmVersion != "ranking-v5-activity-overlay" {
		t.Fatalf("weights/version=%+v/%q want %+v", weights, AlgorithmVersion, want)
	}
	if activity, preference := Score(Factors{ActivitySimilarity: 1}, weights).Score, Score(Factors{PositivePreferenceSimilarity: 1}, weights).Score; activity != 25 || preference != 10 || preference >= activity {
		t.Fatalf("activity/preference contributions=%v/%v", activity, preference)
	}
	if got := Score(Factors{ActivitySimilarity: 1, NegativePreferenceSimilarity: 1}, weights).Score; got != 15 {
		t.Fatalf("negative preference score=%v want 15", got)
	}
	if got := Score(Factors{Wishlist: 1}, weights).Score; got != 0 {
		t.Fatalf("wishlist score=%v want 0", got)
	}
	if got := Score(Factors{ActivitySimilarity: 1, PositivePreferenceSimilarity: 1, PositiveDirectFeedback: 1, TagAffinity: 1, PlaytimeAffinity: 1, AchievementAffinity: 1, RecentActivityAffinity: 1, CompletionAffinity: 1, Novelty: 1}, weights).Score; got != 100 {
		t.Fatalf("clamped score=%v want 100", got)
	}
	if got := Score(Factors{NegativeDirectFeedback: 1}, weights).Score; got != 0 {
		t.Fatalf("negative direct score=%v want 0", got)
	}
}

func TestScorePreservesDecodedVersionOneWeights(t *testing.T) {
	weights := decodeWeights([]byte(`{"semanticSimilarity":25,"wishlist":30,"tagAffinity":15,"playtimeAffinity":10,"achievementAffinity":5,"recentActivityAffinity":5,"positiveFeedback":20,"novelty":5,"negativeFeedback":-30,"dateUncertainty":-5,"negativeSimilarityMultiplier":0.5}`))
	if got := Score(Factors{Wishlist: 1, DateUncertainty: 1}, weights).Score; got != 25 {
		t.Fatalf("historical score = %v, want 25", got)
	}
}

func TestEligibilityAndDeterministicTieBreak(t *testing.T) {
	if Eligible(Candidate{Owned: true}) || Eligible(Candidate{ExplicitlyHidden: true}) {
		t.Fatal("owned and explicitly hidden candidates must be excluded")
	}
	if !Eligible(Candidate{GameID: uuid.New(), Released: true}) {
		t.Fatal("released candidates approved by current eligibility must remain rankable")
	}
	one := Recommendation{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Name: "Zulu", Score: 50}
	two := Recommendation{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Name: "Alpha", Score: 50}
	got := Sort([]Recommendation{one, two})
	if got[0].Name != "Alpha" {
		t.Fatalf("tie order = %q, want Alpha", got[0].Name)
	}
}

func TestLatestDirectActionControlsExactCandidateEligibility(t *testing.T) {
	if !Eligible(Candidate{GameID: uuid.New(), Dislikes: 2}) {
		t.Fatal("historical dislikes must remain soft evidence")
	}
	if Eligible(Candidate{GameID: uuid.New(), ExcludedByLatestDirectAction: true}) {
		t.Fatal("current dislike or legacy exclude must hard-exclude the exact game")
	}
}

func TestTieBreakOrdersCaseVariantsByRawNameThenID(t *testing.T) {
	upper := Recommendation{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Name: "Alpha", Score: 10}
	lower := Recommendation{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Name: "alpha", Score: 10}
	for i := 0; i < 20; i++ {
		got := Sort([]Recommendation{lower, upper})
		if got[0].Name != "Alpha" {
			t.Fatalf("case tie order=%q", got[0].Name)
		}
	}
}
