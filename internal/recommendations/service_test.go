package recommendations

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
)

func TestExplainFallsBackForRefusalAndRejectsUnknownCitation(t *testing.T) {
	evidence := []Evidence{{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"}, {ID: "tags", Text: "Совпадает с вашими любимыми жанрами и метками"}}
	for name, client := range map[string]ExplanationClient{
		"refusal":          fakeExplanationClient{err: ai.ErrRefusal},
		"unknown citation": fakeExplanationClient{output: ai.Explanation{Text: "invented", EvidenceKeys: []string{"not-supplied"}, Generated: true}},
	} {
		t.Run(name, func(t *testing.T) {
			got := Explain(context.Background(), client, "Game", evidence)
			if got.Generated || got.Text == "" || len(got.EvidenceKeys) == 0 {
				t.Fatalf("fallback = %#v", got)
			}
		})
	}
}

func TestExplainRejectsUnsafeGeneratedText(t *testing.T) {
	evidence := []Evidence{{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"}}
	unsafe := map[string]string{
		"English only":      "This game matches your preferences.",
		"score":             "Игра подходит: deterministic score 42.500.",
		"release precision": "Игра подходит, coming_soon: скоро.",
		"JSON":              `Игра подходит: {"wishlist":1}.`,
		"raw factor key":    "Игра подходит из-за tagAffinity.",
		"raw weight key":    "Игра подходит из-за semanticSimilarity.",
	}
	for name, text := range unsafe {
		t.Run(name, func(t *testing.T) {
			got := Explain(context.Background(), fakeExplanationClient{output: ai.Explanation{
				Text: text, EvidenceKeys: []string{"wishlist"}, Generated: true,
			}}, "Game", evidence)
			if got.Generated || got.Text != "Рекомендуем, потому что: Игра есть в вашем списке желаемого." {
				t.Fatalf("unsafe explanation accepted: %#v", got)
			}
		})
	}

	valid := Explain(context.Background(), fakeExplanationClient{output: ai.Explanation{
		Text: "Эта игра уже заинтересовала вас и подходит вашим предпочтениям.", EvidenceKeys: []string{"wishlist"}, Generated: true,
	}}, "Game", evidence)
	if !valid.Generated || valid.Text != "Эта игра уже заинтересовала вас и подходит вашим предпочтениям." {
		t.Fatalf("safe Russian explanation rejected: %#v", valid)
	}
}

func TestPublicEvidenceUsesTopThreePositiveContributions(t *testing.T) {
	r := Recommendation{
		Score: 99.125,
		Factors: Factors{
			PositiveSimilarity:     0.8,
			NegativeSimilarity:     0.2,
			Wishlist:               1,
			TagAffinity:            0.9,
			PlaytimeAffinity:       0.7,
			AchievementAffinity:    0.6,
			RecentActivityAffinity: 0.5,
			PositiveFeedback:       0.75,
			Novelty:                1,
			NegativeFeedback:       1,
			DateUncertainty:        1,
		},
		Weights: DefaultWeights(),
		Release: ReleaseWindow{Precision: "quarter", Raw: "Q2 2027"},
	}

	got := publicEvidence(r)
	want := []Evidence{
		{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"},
		{ID: "semantic-affinity", Text: "Похожа на игры, которые вам нравятся"},
		{ID: "positive-feedback", Text: "Учитывает ваши положительные оценки"},
	}
	if len(got) != len(want) {
		t.Fatalf("evidence count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evidence[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
	joined := strings.ToLower(got[0].Text + got[1].Text + got[2].Text)
	for _, forbidden := range []string{"{", "}", "positivesimilarity", "score", "99.125", "quarter", "q2 2027"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("public evidence leaks %q: %q", forbidden, joined)
		}
	}
}

func TestSafePresentationReplacesLegacyTechnicalEvidence(t *testing.T) {
	r := Recommendation{
		Score:                42.5,
		Factors:              Factors{Wishlist: 1, TagAffinity: 0.5},
		Weights:              DefaultWeights(),
		Release:              ReleaseWindow{Precision: "month", Raw: "May 2027"},
		Explanation:          `Recommended because of: deterministic score 42.500, {"wishlist":1}, month:May 2027.`,
		GeneratedExplanation: false,
		Evidence: []Evidence{
			{ID: "score", Text: "deterministic score 42.500"},
			{ID: "factors", Text: `{"wishlist":1}`},
			{ID: "release-window", Text: "month:May 2027"},
		},
	}

	got := safePresentation(r)
	if got.GeneratedExplanation {
		t.Fatal("legacy fallback must not be marked generated")
	}
	if len(got.Evidence) == 0 || len(got.Evidence) > 3 {
		t.Fatalf("safe evidence = %#v", got.Evidence)
	}
	public := strings.ToLower(got.Explanation)
	for _, forbidden := range []string{"{", "}", "wishlist\"", "score", "42.5", "month", "may 2027"} {
		if strings.Contains(public, forbidden) {
			t.Fatalf("safe explanation leaks %q: %q", forbidden, got.Explanation)
		}
	}
	if !strings.Contains(got.Explanation, "Рекомендуем") {
		t.Fatalf("fallback is not Russian: %q", got.Explanation)
	}
}

func TestSafePresentationKeepsGeneratedExplanationWithPublicCitation(t *testing.T) {
	r := Recommendation{
		Factors:              Factors{Wishlist: 1},
		Weights:              DefaultWeights(),
		Explanation:          "Эта игра уже заинтересовала вас.",
		GeneratedExplanation: true,
		Evidence:             []Evidence{{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"}},
	}

	got := safePresentation(r)
	if !got.GeneratedExplanation || got.Explanation != r.Explanation {
		t.Fatalf("generated explanation was replaced: %#v", got)
	}
}

func TestSafePresentationReplacesTechnicalGeneratedLegacyExplanation(t *testing.T) {
	unsafe := map[string]string{
		"English only":      "This game matches your preferences.",
		"score":             "Игра подходит: deterministic score 30.",
		"release precision": "Игра подходит, coming_soon: скоро.",
		"JSON":              `Игра подходит: {"wishlist":1}.`,
		"raw factor key":    "Игра подходит из-за positiveSimilarity.",
	}
	for name, explanation := range unsafe {
		t.Run(name, func(t *testing.T) {
			r := Recommendation{
				Factors:              Factors{Wishlist: 1},
				Weights:              DefaultWeights(),
				Explanation:          explanation,
				GeneratedExplanation: true,
				Evidence:             []Evidence{{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"}},
			}

			got := safePresentation(r)
			if got.GeneratedExplanation || got.Explanation != "Рекомендуем, потому что: Игра есть в вашем списке желаемого." {
				t.Fatalf("unsafe legacy explanation accepted: %#v", got)
			}
		})
	}
}

func TestCalendarSeparatesPrecisionsWithoutFabricatedDates(t *testing.T) {
	items := []Recommendation{
		{ID: uuid.New(), Release: ReleaseWindow{Precision: "day", ExactDate: "2027-01-02"}},
		{ID: uuid.New(), Release: ReleaseWindow{Precision: "month", Raw: "January 2027"}},
		{ID: uuid.New(), Release: ReleaseWindow{Precision: "quarter", Raw: "Q1 2027"}},
		{ID: uuid.New(), Release: ReleaseWindow{Precision: "year", Raw: "2027"}},
		{ID: uuid.New(), Release: ReleaseWindow{Precision: "unknown", Raw: "TBA"}},
	}
	calendar := BuildCalendar(items)
	if len(calendar.ExactDates) != 1 || len(calendar.Month) != 1 || len(calendar.Quarter) != 1 || len(calendar.Year) != 1 || len(calendar.TBA) != 1 {
		t.Fatalf("calendar buckets = %#v", calendar)
	}
	if calendar.Month[0].Release.ExactDate != "" || calendar.Quarter[0].Release.ExactDate != "" || calendar.Year[0].Release.ExactDate != "" {
		t.Fatal("imprecise windows received fabricated dates")
	}
}

type fakeExplanationClient struct {
	output ai.Explanation
	err    error
}

func (f fakeExplanationClient) ExplainRecommendation(context.Context, ai.RecommendationEvidence) (ai.Explanation, error) {
	return f.output, f.err
}
