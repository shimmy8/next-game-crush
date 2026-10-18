package recommendations

import (
	"context"
	"reflect"
	"testing"

	"next-game-crush/internal/ai"
)

func TestWishlistEvidenceSurvivesZeroOrderingWeight(t *testing.T) {
	for name, factors := range map[string]Factors{
		"weak substantive signal":   {Wishlist: 1, TagAffinity: .01, Novelty: 1},
		"substantive slot reserved": {Wishlist: 1, TagAffinity: .01, PlaytimeAffinity: .001, Novelty: 1},
	} {
		t.Run(name, func(t *testing.T) {
			got := publicEvidence(Recommendation{Factors: factors, Weights: DefaultWeights()})
			want := []string{"wishlist", "tag-affinity", "novelty"}
			keys := make([]string, 0, len(got))
			for _, item := range got {
				keys = append(keys, item.ID)
			}
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("evidence keys=%v want=%v", keys, want)
			}
		})
	}
	discovery := publicEvidence(Recommendation{Factors: Factors{TagAffinity: .01, Novelty: 1}, Weights: DefaultWeights()})
	if len(discovery) != 2 || discovery[0].ID != "novelty" || discovery[1].ID != "tag-affinity" {
		t.Fatalf("discovery contribution order/membership=%#v", discovery)
	}
}

func TestMembershipOnlyExplanationDoesNotCallModel(t *testing.T) {
	for _, factors := range []Factors{{Wishlist: 1}, {Wishlist: 1, Novelty: 1}} {
		client := &countingExplanationClient{output: ai.Explanation{Text: "Эта игра подходит вашим интересам.", EvidenceKeys: []string{"wishlist"}}}
		evidence := legacyGroundingEvidence(publicEvidence(Recommendation{Factors: factors, Weights: DefaultWeights()}))
		got := Explain(context.Background(), client, "Game", evidence)
		if client.calls != 0 || got.Source != ExplanationSourceDeterministic || got.Text != "Вы уже добавили эту игру в желаемое — следим за датой выхода." {
			t.Fatalf("membership fallback=%#v calls=%d", got, client.calls)
		}
		if len(got.EvidenceKeys) != len(evidence) {
			t.Fatalf("fallback keys=%v evidence=%#v", got.EvidenceKeys, evidence)
		}
		for i, key := range got.EvidenceKeys {
			if key != evidence[i].ID {
				t.Fatalf("invented evidence=%v", got.EvidenceKeys)
			}
		}
	}
}

func TestGeneratedWishlistExplanationRequiresSubstantiveCitation(t *testing.T) {
	evidence := []GroundingEvidence{
		{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы: horror.", Strength: 5},
		{ID: "wishlist-context-1", Kind: EvidenceWishlistContext, PublicText: "Вы уже добавили эту игру в желаемое — следим за датой выхода.", Strength: 20},
	}
	for _, keys := range [][]string{{"wishlist-context-1"}, {"feature-overlap-1"}, {"wishlist-context-1", "feature-overlap-1"}} {
		client := &countingExplanationClient{output: ai.Explanation{Text: "Игра совпадает с вашими любимыми жанрами.", EvidenceKeys: keys}}
		got := Explain(context.Background(), client, "Game", evidence)
		wantGenerated := len(keys) == 1 && keys[0] == "feature-overlap-1"
		if client.calls != 1 || (got.Source == ExplanationSourceAI) != wantGenerated {
			t.Fatalf("keys=%v got=%#v calls=%d", keys, got, client.calls)
		}
	}
}

func TestSubstantiveEvidenceUsesOnlySupportedAffinitySignals(t *testing.T) {
	for _, id := range []string{"semantic-affinity", "tag-affinity", "playtime-affinity", "achievement-affinity", "recent-activity", "positive-feedback"} {
		if !hasSubstantiveEvidence([]Evidence{{ID: id}}) {
			t.Fatalf("substantive signal rejected: %s", id)
		}
	}
	for _, id := range []string{"wishlist", "novelty", "negative-feedback", "tags", "unknown"} {
		if hasSubstantiveEvidence([]Evidence{{ID: id}}) {
			t.Fatalf("non-affinity evidence accepted: %s", id)
		}
	}
}

func TestSafePresentationRejectsMembershipOnlyGeneratedAffinity(t *testing.T) {
	for _, factors := range []Factors{{Wishlist: 1}, {Wishlist: 1, Novelty: 1}, {Wishlist: 1, TagAffinity: .5}} {
		item := Recommendation{Factors: factors, Weights: DefaultWeights(), GeneratedExplanation: true,
			Explanation: "Эта игра подходит вашим интересам.", Evidence: []Evidence{{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"}}}
		got := safePresentation(item)
		if got.GeneratedExplanation || got.Explanation == item.Explanation {
			t.Fatalf("membership-only affinity survived=%#v", got)
		}
		if factors.TagAffinity == 0 && got.Explanation != "Игра уже в вашем списке желаемого — следим за её релизом." {
			t.Fatalf("watchlist copy=%q", got.Explanation)
		}
	}
}

func TestSafePresentationKeepsGroundedCompletionAnchor(t *testing.T) {
	text := "Есть сходство с пройденной вами игрой SOMA."
	got := safePresentation(Recommendation{
		ExplanationSource: ExplanationSourceDeterministic,
		Explanation:       text,
		Evidence:          []Evidence{{ID: "completion-anchor-1", Kind: EvidenceCompletionAnchor, Text: text}},
	})
	if got.ExplanationSource != ExplanationSourceDeterministic || len(got.Evidence) != 1 || got.Evidence[0].Kind != EvidenceCompletionAnchor {
		t.Fatalf("completion presentation=%#v", got)
	}
}

func TestDiscoveryExplanationNeverClaimsWishlistMembership(t *testing.T) {
	evidence := []GroundingEvidence{{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы: horror.", Strength: 5}}
	got := Explain(context.Background(), fakeExplanationClient{output: ai.Explanation{
		Text: "Эта игра уже в вашем списке желаемого.", EvidenceKeys: []string{"feature-overlap-1"},
	}}, "Game", evidence)
	if got.Source == ExplanationSourceAI || len(got.EvidenceKeys) != 1 || got.EvidenceKeys[0] != "feature-overlap-1" {
		t.Fatalf("discovery explanation=%#v", got)
	}
}

type countingExplanationClient struct {
	calls  int
	output ai.Explanation
}

func (f *countingExplanationClient) ExplainRecommendation(context.Context, ai.RecommendationEvidence) (ai.Explanation, error) {
	f.calls++
	return f.output, nil
}

func TestSteamPresentationFromStoredSnapshot(t *testing.T) {
	t.Parallel()

	cover, store := steamPresentation(620, []byte(`{"HeaderImage":"https://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg"}`))
	if cover != "https://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg" {
		t.Fatalf("cover = %q", cover)
	}
	if store != "https://store.steampowered.com/app/620/" {
		t.Fatalf("store = %q", store)
	}
}

func TestSteamPresentationRejectsUnsafeOrMalformedCover(t *testing.T) {
	t.Parallel()

	for _, snapshot := range [][]byte{
		nil,
		[]byte(`not-json`),
		[]byte(`{"HeaderImage":"http://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg"}`),
		[]byte(`{"HeaderImage":"https://steamstatic.com.evil.example/header.jpg"}`),
	} {
		cover, store := steamPresentation(620, snapshot)
		if cover != "" {
			t.Fatalf("snapshot %q cover = %q", snapshot, cover)
		}
		if store != "https://store.steampowered.com/app/620/" {
			t.Fatalf("snapshot %q store = %q", snapshot, store)
		}
	}
}

func TestSteamPresentationRejectsInvalidAppID(t *testing.T) {
	t.Parallel()
	cover, store := steamPresentation(0, []byte(`{"HeaderImage":"https://steamstatic.com/header.jpg"}`))
	if cover != "" || store != "" {
		t.Fatalf("cover=%q store=%q", cover, store)
	}
}
