package recommendations

import (
	"testing"

	"github.com/google/uuid"
)

func TestScoreUsesAchievementCompletionWeights(t *testing.T) {
	weights := DefaultWeights()
	if weights != (Weights{SemanticSimilarity: 25, Wishlist: 0, TagAffinity: 15, PlaytimeAffinity: 10, AchievementAffinity: 5, RecentActivityAffinity: 5, CompletionAffinity: 5, PositiveFeedback: 20, Novelty: 5, NegativeFeedback: -30, DateUncertainty: -5, NegativeSimilarityMultiplier: .5, NegativeModePreferenceWeight: -5}) || AlgorithmVersion != "ranking-v4-achievement-completion" {
		t.Fatalf("weights=%#v", weights)
	}
	if got := Score(Factors{Wishlist: 1}, weights); got.Score != 0 {
		t.Fatalf("wishlist score = %v, want 0", got.Score)
	}
	if Score(Factors{TagAffinity: .8}, weights).Score != Score(Factors{TagAffinity: .8, Wishlist: 1}, weights).Score {
		t.Fatal("wishlist membership changes affinity score")
	}
	if Score(Factors{PositiveSimilarity: 1}, weights).Score <= Score(Factors{Wishlist: 1}, weights).Score {
		t.Fatal("affinity discovery cannot outrank membership alone")
	}
	if got := Score(Factors{PositiveSimilarity: 2}, weights); got.Score != 25 {
		t.Fatalf("semantic score = %v, want capped 25", got.Score)
	}
	if got := Score(Factors{PositiveSimilarity: 1, NegativeSimilarity: 1}, weights); got.Score >= 25 {
		t.Fatalf("negative semantic profile did not soften similarity: %v", got.Score)
	}
	if got := Score(Factors{CompletionAffinity: 1}, weights); got.Score != 5 {
		t.Fatalf("completion score = %v, want 5", got.Score)
	}
	if got := Score(Factors{NegativeFeedback: 1}, weights); got.Score != 0 {
		t.Fatalf("negative score = %v, want clamped 0", got.Score)
	}
	if got := Score(Factors{Wishlist: 1, DateUncertainty: 1}, weights); got.Score != 0 {
		t.Fatalf("uncertain wishlist score = %v, want 0", got.Score)
	}
	if got := Score(Factors{Wishlist: 1, PositiveFeedback: 1, TagAffinity: 1, PlaytimeAffinity: 1, AchievementAffinity: 1, RecentActivityAffinity: 1, CompletionAffinity: 1, Novelty: 1, PositiveSimilarity: 1}, weights); got.Score != 90 {
		t.Fatalf("high score = %v, want 90", got.Score)
	}
	if got := Score(Factors{PositiveSimilarity: 1}, Weights{SemanticSimilarity: 200}); got.Score != 100 {
		t.Fatalf("oversized weights score = %v, want clamp 100", got.Score)
	}
}

func TestScorePreservesExplicitVersionOneWeights(t *testing.T) {
	weights := Weights{SemanticSimilarity: 25, Wishlist: 30, TagAffinity: 15, PlaytimeAffinity: 10, AchievementAffinity: 5, RecentActivityAffinity: 5, PositiveFeedback: 20, Novelty: 5, NegativeFeedback: -30, DateUncertainty: -5, NegativeSimilarityMultiplier: .5}
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
