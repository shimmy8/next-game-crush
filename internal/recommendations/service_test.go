package recommendations

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
)

func TestExplainFallsBackForRefusalAndRejectsUnknownCitation(t *testing.T) {
	evidence := []Evidence{{ID: "score", Text: "deterministic score 80"}, {ID: "wishlist", Text: "on wishlist"}}
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
