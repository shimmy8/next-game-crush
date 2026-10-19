package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
)

type allowAllCurrentEligibility struct{}

func (allowAllCurrentEligibility) Eligible(context.Context, uuid.UUID, time.Time) (bool, error) {
	return true, nil
}

func (allowAllCurrentEligibility) FilterEligibleGameIDs(_ context.Context, ids []uuid.UUID, _ time.Time) (map[uuid.UUID]bool, error) {
	result := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func recommendationTestClock() time.Time {
	return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
}

type selectiveCurrentEligibility struct {
	allowed map[uuid.UUID]bool
	err     error
}

func (r selectiveCurrentEligibility) Eligible(_ context.Context, id uuid.UUID, _ time.Time) (bool, error) {
	return r.allowed[id], r.err
}

func (r selectiveCurrentEligibility) FilterEligibleGameIDs(_ context.Context, ids []uuid.UUID, _ time.Time) (map[uuid.UUID]bool, error) {
	if r.err != nil {
		return nil, r.err
	}
	result := map[uuid.UUID]bool{}
	for _, id := range ids {
		if r.allowed[id] {
			result[id] = true
		}
	}
	return result, nil
}

type currentEligibilityReadRepository struct {
	Repository
	items []Recommendation
}

func (r currentEligibilityReadRepository) Latest(context.Context, uuid.UUID) ([]Recommendation, error) {
	return append([]Recommendation(nil), r.items...), nil
}

func (r currentEligibilityReadRepository) Get(_ context.Context, _ uuid.UUID, id uuid.UUID) (Recommendation, error) {
	for _, item := range r.items {
		if item.ID == id {
			return item, nil
		}
	}
	return Recommendation{}, pgx.ErrNoRows
}

func TestCurrentEligibilityProtectsEveryDeliveryPath(t *testing.T) {
	eligibleGame, expiredGame := uuid.New(), uuid.New()
	eligibleRecommendation, expiredRecommendation := uuid.New(), uuid.New()
	repo := currentEligibilityReadRepository{items: []Recommendation{
		{ID: eligibleRecommendation, GameID: eligibleGame, Name: "Eligible", Release: ReleaseWindow{Precision: "day", ExactDate: "2026-10-18"}},
		{ID: expiredRecommendation, GameID: expiredGame, Name: "Expired", Release: ReleaseWindow{Precision: "day", ExactDate: "2026-10-10"}},
	}}
	service := &Service{repository: repo, eligibility: selectiveCurrentEligibility{allowed: map[uuid.UUID]bool{eligibleGame: true}}, clock: recommendationTestClock}

	latest, err := service.Latest(context.Background(), uuid.New())
	if err != nil || len(latest) != 1 || latest[0].GameID != eligibleGame {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	overview, err := service.Overview(context.Background(), uuid.New())
	if err != nil || overview.Discoveries.Total != 1 || len(overview.Calendar.ExactDates) != 1 {
		t.Fatalf("overview=%#v err=%v", overview, err)
	}
	if _, err := service.Get(context.Background(), uuid.New(), expiredRecommendation); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired detail err=%v", err)
	}
	if len(repo.items) != 2 {
		t.Fatal("read-time gate mutated historical ranking rows")
	}

	service.eligibility = selectiveCurrentEligibility{err: errors.New("eligibility unavailable")}
	if _, err := service.Latest(context.Background(), uuid.New()); err == nil {
		t.Fatal("eligibility failure did not fail closed")
	}
}

func TestClassifyExplanationFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ExplanationFailureReason
	}{
		{name: "disabled", err: ai.ErrCapabilityDisabled, want: ExplanationFailureDisabled},
		{name: "rate limited", err: &ai.RequestError{StatusCode: http.StatusTooManyRequests, Temporary: true}, want: ExplanationFailureRateLimited},
		{name: "deadline", err: context.DeadlineExceeded, want: ExplanationFailureTimeout},
		{name: "request timeout", err: &ai.RequestError{StatusCode: http.StatusRequestTimeout, Temporary: true}, want: ExplanationFailureTimeout},
		{name: "server unavailable", err: &ai.RequestError{StatusCode: http.StatusBadGateway, Temporary: true}, want: ExplanationFailureProviderUnavailable},
		{name: "transport", err: ai.ErrHTTP, want: ExplanationFailureProviderUnavailable},
		{name: "refusal", err: ai.ErrRefusal, want: ExplanationFailureRefusal},
		{name: "invalid evidence", err: ai.ErrInvalidEvidence, want: ExplanationFailureInvalidCitation},
		{name: "invalid output", err: ai.ErrInvalidOutput, want: ExplanationFailureInvalidOutput},
		{name: "unknown", err: errors.New("private provider detail"), want: ExplanationFailureUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyExplanationFailure(test.err); got != test.want {
				t.Fatalf("classifyExplanationFailure() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExplainUsesOnlySelectedSubstantivePublicEvidence(t *testing.T) {
	selected := []GroundingEvidence{
		{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы: psychological horror.", Strength: 8, SourceGameIDs: []uuid.UUID{uuid.New()}},
		{ID: "similar-game-1", Kind: EvidenceSimilarGame, PublicText: "По описанию близка к SOMA.", Strength: 7, SourceGameIDs: []uuid.UUID{uuid.New()}},
		{ID: "wishlist-context-1", Kind: EvidenceWishlistContext, PublicText: "Вы уже добавили эту игру в желаемое — следим за датой выхода.", Strength: 20, SourceGameIDs: []uuid.UUID{uuid.New()}},
	}
	client := &recordingExplanationClient{output: ai.Explanation{Text: "Игра близка к SOMA по атмосфере psychological horror.", EvidenceKeys: []string{"similar-game-1", "feature-overlap-1"}}}
	got := Explain(context.Background(), client, "Игра", selected)
	if got.Source != ExplanationSourceAI || got.FailureReason != "" || !reflect.DeepEqual(got.EvidenceKeys, []string{"similar-game-1", "feature-overlap-1"}) {
		t.Fatalf("result = %#v", got)
	}
	if client.calls != 1 || client.input.EvidenceKey != "recommendation-evidence" || client.input.GameName != "Игра" {
		t.Fatalf("client call = %d %#v", client.calls, client.input)
	}
	want := []ai.EvidenceItem{
		{Key: "feature-overlap-1", Value: "Совпадают темы: psychological horror."},
		{Key: "similar-game-1", Value: "По описанию близка к SOMA."},
	}
	if !reflect.DeepEqual(client.input.Evidence, want) {
		t.Fatalf("model evidence = %#v, want %#v", client.input.Evidence, want)
	}
	encoded := string(mustTestJSON(t, client.input))
	for _, forbidden := range []string{"wishlist-context", "желаемое", "novelty", "strength", "sourceGameIds", "factor", "weight", selected[0].SourceGameIDs[0].String()} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("AI input leaks %q: %s", forbidden, encoded)
		}
	}
}

func TestExplainSkipsClientWithoutSubstantiveEvidence(t *testing.T) {
	tests := []struct {
		name     string
		selected []GroundingEvidence
		source   ExplanationSource
		text     string
	}{
		{name: "none", source: ExplanationSourceNone, text: "Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно."},
		{name: "wishlist only", selected: []GroundingEvidence{{ID: "wishlist-context-1", Kind: EvidenceWishlistContext, PublicText: "Вы уже добавили эту игру в желаемое — следим за датой выхода.", Strength: 20}}, source: ExplanationSourceDeterministic, text: "Вы уже добавили эту игру в желаемое — следим за датой выхода."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &recordingExplanationClient{}
			got := Explain(context.Background(), client, "Игра", test.selected)
			if client.calls != 0 || got.Source != test.source || got.FailureReason != "" || got.Text != test.text {
				t.Fatalf("result=%#v calls=%d", got, client.calls)
			}
		})
	}
}

func TestExplainFallsBackWithTypedValidationReason(t *testing.T) {
	selected := []GroundingEvidence{{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы: horror.", Strength: 8}}
	tests := []struct {
		name   string
		output ai.Explanation
		err    error
		reason ExplanationFailureReason
	}{
		{name: "disabled", err: ai.ErrCapabilityDisabled, reason: ExplanationFailureDisabled},
		{name: "duplicate citation", output: ai.Explanation{Text: "Подходит по темам.", EvidenceKeys: []string{"feature-overlap-1", "feature-overlap-1"}}, reason: ExplanationFailureInvalidCitation},
		{name: "unknown citation", output: ai.Explanation{Text: "Подходит по темам.", EvidenceKeys: []string{"unknown"}}, reason: ExplanationFailureInvalidCitation},
		{name: "empty citation", output: ai.Explanation{Text: "Подходит по темам."}, reason: ExplanationFailureInvalidCitation},
		{name: "unsafe language", output: ai.Explanation{Text: "This game matches your profile.", EvidenceKeys: []string{"feature-overlap-1"}}, reason: ExplanationFailureUnsafeText},
		{name: "novelty claim", output: ai.Explanation{Text: "Игра добавляет новое к вашим интересам.", EvidenceKeys: []string{"feature-overlap-1"}}, reason: ExplanationFailureUnsafeText},
		{name: "expanded interests claim", output: ai.Explanation{Text: "Игра расширит ваши игровые интересы.", EvidenceKeys: []string{"feature-overlap-1"}}, reason: ExplanationFailureUnsafeText},
		{name: "wishlist claim", output: ai.Explanation{Text: "Игра в вашем списке желаемого и поэтому вам понравится.", EvidenceKeys: []string{"feature-overlap-1"}}, reason: ExplanationFailureUnsafeText},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &recordingExplanationClient{output: test.output, err: test.err}
			got := Explain(context.Background(), client, "Игра", selected)
			if got.Source != ExplanationSourceDeterministic || got.FailureReason != test.reason || got.Text != DeterministicExplanation(selected) || !reflect.DeepEqual(got.EvidenceKeys, []string{"feature-overlap-1"}) {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

type recordingExplanationClient struct {
	calls  int
	input  ai.RecommendationEvidence
	output ai.Explanation
	err    error
}

type memoryExplanationCache struct {
	values map[string]ai.Explanation
}

func (c *memoryExplanationCache) GetExplanationByInput(_ context.Context, userID, _ uuid.UUID, model, hash string) (ai.Explanation, bool, error) {
	value, ok := c.values[userID.String()+":"+model+":"+hash]
	return value, ok, nil
}

func (c *memoryExplanationCache) PutExplanationByInput(_ context.Context, userID, _ uuid.UUID, model, hash string, value ai.Explanation) error {
	if c.values == nil {
		c.values = map[string]ai.Explanation{}
	}
	c.values[userID.String()+":"+model+":"+hash] = value
	return nil
}

func TestRankingReusesGroundedExplanationInput(t *testing.T) {
	client := &recordingExplanationClient{output: ai.Explanation{Text: "Совпадают важные для вас темы.", EvidenceKeys: []string{"feature-overlap-1"}}}
	service := &Service{explainer: client}
	cache := &memoryExplanationCache{}
	service.SetExplanationCache(cache, "model-a")
	userID := uuid.New()
	gameID := uuid.New()
	evidence := []GroundingEvidence{{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадают темы: horror.", Strength: 1}}
	for i := 0; i < 2; i++ {
		result, err := service.explainCached(context.Background(), userID, gameID, "Игра", evidence)
		if err != nil || result.Source != ExplanationSourceAI {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	}
	if client.calls != 1 {
		t.Fatalf("identical input calls=%d", client.calls)
	}
	evidence[0].PublicText = "Совпадают темы: survival horror."
	if _, err := service.explainCached(context.Background(), userID, gameID, "Игра", evidence); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("changed input calls=%d", client.calls)
	}
}

func (f *recordingExplanationClient) ExplainRecommendation(_ context.Context, input ai.RecommendationEvidence) (ai.Explanation, error) {
	f.calls++
	f.input = input
	return f.output, f.err
}

func mustTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestExplainFallsBackForRefusalAndRejectsUnknownCitation(t *testing.T) {
	evidence := []Evidence{{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"}, {ID: "tag-affinity", Text: "Совпадает с вашими любимыми жанрами и метками"}}
	for name, client := range map[string]ExplanationClient{
		"refusal":          fakeExplanationClient{err: ai.ErrRefusal},
		"unknown citation": fakeExplanationClient{output: ai.Explanation{Text: "Игра подходит вашим интересам.", EvidenceKeys: []string{"not-supplied"}, Generated: true}},
	} {
		t.Run(name, func(t *testing.T) {
			got := Explain(context.Background(), client, "Game", legacyGroundingEvidence(evidence))
			if got.Source == ExplanationSourceAI || got.Text == "" || len(got.EvidenceKeys) == 0 {
				t.Fatalf("fallback = %#v", got)
			}
		})
	}
}

func TestExplainRejectsUnsafeGeneratedText(t *testing.T) {
	evidence := []GroundingEvidence{{ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap, PublicText: "Совпадает с вашими любимыми жанрами и метками", Strength: 1}}
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
				Text: text, EvidenceKeys: []string{"feature-overlap-1"}, Generated: true,
			}}, "Game", evidence)
			if got.Source == ExplanationSourceAI || got.Text != DeterministicExplanation(evidence) {
				t.Fatalf("unsafe explanation accepted: %#v", got)
			}
		})
	}

	valid := Explain(context.Background(), fakeExplanationClient{output: ai.Explanation{
		Text: "Эта игра уже заинтересовала вас и подходит вашим предпочтениям.", EvidenceKeys: []string{"feature-overlap-1"}, Generated: true,
	}}, "Game", evidence)
	if valid.Source != ExplanationSourceAI || valid.Text != "Эта игра уже заинтересовала вас и подходит вашим предпочтениям." {
		t.Fatalf("safe Russian explanation rejected: %#v", valid)
	}
}

func TestPublicEvidenceUsesTopThreePositiveContributions(t *testing.T) {
	r := Recommendation{
		Score: 99.125,
		Factors: Factors{
			ActivitySimilarity:           0.8,
			NegativePreferenceSimilarity: 0.2,
			Wishlist:                     1,
			TagAffinity:                  0.9,
			PlaytimeAffinity:             0.7,
			AchievementAffinity:          0.6,
			RecentActivityAffinity:       0.5,
			PositiveDirectFeedback:       0.75,
			Novelty:                      1,
			NegativeDirectFeedback:       1,
			DateUncertainty:              1,
		},
		Weights: DefaultWeights(),
		Release: ReleaseWindow{Precision: "quarter", Raw: "Q2 2027"},
	}

	got := publicEvidence(r)
	want := []Evidence{
		{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"},
		{ID: "semantic-affinity", Text: "Похожа на игры, в которые вы играли"},
		{ID: "positive-feedback", Text: "Учитывает вашу положительную оценку этой игры"},
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
		Factors:              Factors{Wishlist: 1, TagAffinity: .5},
		Weights:              DefaultWeights(),
		Explanation:          "Эта игра уже заинтересовала вас.",
		GeneratedExplanation: true,
		Evidence:             []Evidence{{ID: "tag-affinity", Text: "Совпадает с вашими любимыми жанрами и метками"}},
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
			if got.GeneratedExplanation || got.Explanation != "Игра уже в вашем списке желаемого — следим за её релизом." {
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
