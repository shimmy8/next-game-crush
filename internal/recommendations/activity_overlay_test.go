package recommendations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
)

func TestNewRankingFactorNamesAndLegacyDecoder(t *testing.T) {
	factors, err := json.Marshal(Factors{
		ActivitySimilarity: .8, PositivePreferenceSimilarity: .4, NegativePreferenceSimilarity: .2,
		PositiveDirectFeedback: 1, NegativeDirectFeedback: 0, NegativeModePreference: .5,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(factors)
	for _, name := range []string{"activitySimilarity", "positivePreferenceSimilarity", "negativePreferenceSimilarity", "positiveDirectFeedback", "negativeDirectFeedback", "negativeModePreference"} {
		if !strings.Contains(encoded, `"`+name+`"`) {
			t.Fatalf("new factor %q missing from %s", name, encoded)
		}
	}
	for _, legacy := range []string{"positiveSimilarity", "negativeSimilarity", "positiveFeedback", "negativeFeedback"} {
		if strings.Contains(encoded, `"`+legacy+`"`) {
			t.Fatalf("legacy factor %q written in %s", legacy, encoded)
		}
	}
	decoded := decodeFactors([]byte(`{"positiveSimilarity":0.7,"negativeSimilarity":0.3,"positiveFeedback":1,"negativeFeedback":1}`))
	if decoded.ActivitySimilarity != .7 || decoded.NegativePreferenceSimilarity != .3 || decoded.PositiveDirectFeedback != 1 || decoded.NegativeDirectFeedback != 1 {
		t.Fatalf("legacy factors were not mapped: %+v", decoded)
	}
	weights := decodeWeights([]byte(`{"semanticSimilarity":25,"negativeSimilarityMultiplier":0.5,"positiveFeedback":20,"negativeFeedback":-30,"negativeModePreferenceWeight":-5}`))
	if weights.ActivitySimilarity != 25 || weights.NegativePreferenceSimilarity != -12.5 || weights.PositiveDirectFeedback != 20 || weights.NegativeDirectFeedback != -30 || weights.NegativeModePreference != -5 {
		t.Fatalf("legacy weights were not mapped: %+v", weights)
	}
}

func TestRankingBuildKeyUsesEveryPersistedSourceIdentityField(t *testing.T) {
	base := RankingSource{Activity: profile.Profile{ID: uuid.New(), Version: 2}, ProfileAlgorithmVersion: "activity-a", ProfileBuildKey: "profile-build", ProfileInputContentHash: "profile-input", Overlay: preferences.Overlay{ID: uuid.New(), Version: 3, AlgorithmVersion: "overlay-a", BuildKey: "overlay-build", InputContentHash: "overlay-input"}}
	want := RankingBuildKey("request", AlgorithmVersion, base)
	mutations := []func(*RankingSource){
		func(s *RankingSource) { s.Activity.ID = uuid.New() }, func(s *RankingSource) { s.Activity.Version++ },
		func(s *RankingSource) { s.ProfileAlgorithmVersion += "x" }, func(s *RankingSource) { s.ProfileBuildKey += "x" }, func(s *RankingSource) { s.ProfileInputContentHash += "x" },
		func(s *RankingSource) { s.Overlay.ID = uuid.New() }, func(s *RankingSource) { s.Overlay.Version++ },
		func(s *RankingSource) { s.Overlay.AlgorithmVersion += "x" }, func(s *RankingSource) { s.Overlay.BuildKey += "x" }, func(s *RankingSource) { s.Overlay.InputContentHash += "x" },
	}
	for index, mutate := range mutations {
		changed := base
		mutate(&changed)
		if got := RankingBuildKey("request", AlgorithmVersion, changed); got == want {
			t.Fatalf("provenance mutation %d omitted", index)
		}
	}
}

type scriptedRankingRepository struct {
	Repository
	inputs []RankingInputs
	calls  int
}

func (r *scriptedRankingRepository) ResolveRankingInputs(context.Context, RankingRequest, int) (RankingInputs, error) {
	result := r.inputs[r.calls]
	r.calls++
	return result, nil
}

type scriptedOverlayEnsurer struct {
	overlays []preferences.Overlay
	calls    int
}

func (e *scriptedOverlayEnsurer) EnsureCurrent(context.Context, uuid.UUID) (preferences.Overlay, error) {
	result := e.overlays[e.calls]
	e.calls++
	return result, nil
}

func TestFeedbackCommitInEnsureResolveGapRetriesExactOverlayPair(t *testing.T) {
	a := preferences.Overlay{ID: uuid.New(), InputContentHash: "a"}
	b := preferences.Overlay{ID: uuid.New(), InputContentHash: "b"}
	repository := &scriptedRankingRepository{inputs: []RankingInputs{{Source: RankingSource{Overlay: a}}, {Source: RankingSource{Overlay: b}}}}
	ensurer := &scriptedOverlayEnsurer{overlays: []preferences.Overlay{a, b, b, b}}
	service := &Service{repository: repository, overlays: ensurer}
	input, err := service.resolveCoherentInputs(context.Background(), uuid.New(), "gap")
	if err != nil || input.Source.Overlay.ID != b.ID || repository.calls != 2 || ensurer.calls != 4 {
		t.Fatalf("coherence retry input=%#v resolve=%d ensure=%d err=%v", input.Source.Overlay, repository.calls, ensurer.calls, err)
	}
}

func TestNonWishlistCapRunsAfterDeterministicSortAndPreservesWishlist(t *testing.T) {
	wishlist := Recommendation{GameID: uuid.New(), Name: "Wishlist", Score: 1, Wishlisted: true}
	low := Recommendation{GameID: uuid.New(), Name: "Low", Score: 2}
	high := Recommendation{GameID: uuid.New(), Name: "High", Score: 90}
	ranked := Sort([]Recommendation{low, wishlist, high})
	got := capRankedRecommendations(ranked, 1)
	if len(got) != 2 || got[0].GameID != high.GameID || got[1].GameID != wishlist.GameID {
		t.Fatalf("post-score cap=%#v", got)
	}
}
