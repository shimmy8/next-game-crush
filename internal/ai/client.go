package ai

import "context"

type Client interface {
	ExtractGameFeatures(context.Context, GameEvidence) (GameFeatures, error)
	SummarizeProfile(context.Context, ProfileEvidence) (ProfileSummary, error)
	ExplainRecommendation(context.Context, RecommendationEvidence) (Explanation, error)
	ClassifyFeedback(context.Context, FeedbackText) (FeedbackClassification, error)
	Embed(context.Context, []string) ([][]float32, error)
}

// DeterministicExplanation is the no-network fallback used when OpenAI is
// disabled or unavailable. Callers can render it without pretending it is a
// model-generated explanation.
func DeterministicExplanation(factors []string) Explanation {
	text := "Recommended from the saved ranking factors."
	if len(factors) > 0 {
		text = "Recommended because of: " + joinHuman(factors) + "."
	}
	return Explanation{Text: text, Facts: append([]string(nil), factors...), Uncertainty: "AI explanation unavailable", Generated: false}
}

func DeterministicProfileSummary() ProfileSummary {
	return ProfileSummary{Summary: "AI profile summary is unavailable; recommendations use saved deterministic signals.", Uncertainty: "No model-generated summary is available."}
}

func DeterministicFeedbackClassification() FeedbackClassification {
	return FeedbackClassification{Taxonomy: "other", Polarity: "neutral", Confidence: 0, Actionable: false}
}

func joinHuman(values []string) string {
	result := ""
	for i, value := range values {
		if i > 0 {
			result += ", "
		}
		result += value
	}
	return result
}
