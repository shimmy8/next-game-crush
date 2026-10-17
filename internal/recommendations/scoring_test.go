package recommendations

import (
	"testing"

	"github.com/google/uuid"
)

func TestScoreUsesVersionOneWeights(t *testing.T) {
	weights := DefaultWeights()
	if weights != (Weights{SemanticSimilarity: 25, Wishlist: 30, TagAffinity: 15, PlaytimeAffinity: 10, AchievementAffinity: 5, RecentActivityAffinity: 5, PositiveFeedback: 20, Novelty: 5, NegativeFeedback: -30, DateUncertainty: -5, NegativeSimilarityMultiplier: .5}) {
		t.Fatalf("weights=%#v", weights)
	}
	if got := Score(Factors{Wishlist: 1}, weights); got.Score != 30 {
		t.Fatalf("wishlist score = %v, want 30", got.Score)
	}
	if got := Score(Factors{PositiveSimilarity: 2}, weights); got.Score != 25 {
		t.Fatalf("semantic score = %v, want capped 25", got.Score)
	}
	if got := Score(Factors{PositiveSimilarity: 1, NegativeSimilarity: 1}, weights); got.Score >= 25 {
		t.Fatalf("negative semantic profile did not soften similarity: %v", got.Score)
	}
	if got := Score(Factors{NegativeFeedback: 1}, weights); got.Score != 0 {
		t.Fatalf("negative score = %v, want clamped 0", got.Score)
	}
	if got := Score(Factors{Wishlist: 1, DateUncertainty: 1}, weights); got.Score != 25 {
		t.Fatalf("uncertain wishlist score = %v, want 25", got.Score)
	}
	if got := Score(Factors{Wishlist: 1, PositiveFeedback: 1, TagAffinity: 1, PlaytimeAffinity: 1, AchievementAffinity: 1, RecentActivityAffinity: 1, Novelty: 1, PositiveSimilarity: 1}, weights); got.Score != 100 {
		t.Fatalf("high score = %v, want clamp 100", got.Score)
	}
}

func TestEligibilityAndDeterministicTieBreak(t *testing.T) {
	if Eligible(Candidate{Owned: true}) || Eligible(Candidate{Released: true}) || Eligible(Candidate{ExplicitlyHidden: true}) {
		t.Fatal("owned, released, and explicitly hidden candidates must be excluded")
	}
	one := Recommendation{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Name: "Zulu", Score: 50}
	two := Recommendation{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Name: "Alpha", Score: 50}
	got := Sort([]Recommendation{one, two})
	if got[0].Name != "Alpha" {
		t.Fatalf("tie order = %q, want Alpha", got[0].Name)
	}
}

func TestRepeatedFeedbackCanExcludeButOneDislikeCannot(t *testing.T) {
	if ShouldExcludeForFeedback(1) {
		t.Fatal("one dislike must be a soft penalty")
	}
	if !ShouldExcludeForFeedback(2) {
		t.Fatal("repeated explicit dislikes must permit exclusion")
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
