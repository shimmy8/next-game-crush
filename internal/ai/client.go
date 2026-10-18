package ai

import "context"

type Client interface {
	ExtractGameFeatures(context.Context, GameEvidence) (GameFeatures, error)
	SummarizeProfile(context.Context, ProfileEvidence) (ProfileSummary, error)
	ExplainRecommendation(context.Context, RecommendationEvidence) (Explanation, error)
	ClassifyFeedback(context.Context, FeedbackText) (FeedbackClassification, error)
	ClassifyAchievementSemantics(context.Context, AchievementSemanticEvidence) (AchievementSemanticClassification, error)
	Embed(context.Context, []string) ([][]float32, error)
}

// DeterministicExplanation is the no-network fallback used when OpenAI is
// disabled or unavailable. Callers can render it without pretending it is a
// model-generated explanation.
func DeterministicExplanation(factors []string) Explanation {
	publicFactors := append([]string(nil), factors...)
	if len(publicFactors) > 3 {
		publicFactors = publicFactors[:3]
	}
	text := "Рекомендуем на основе ваших сохранённых игровых предпочтений."
	if len(publicFactors) > 0 {
		text = "Рекомендуем, потому что: " + joinHuman(publicFactors) + "."
	}
	return Explanation{Text: text, Facts: publicFactors, Uncertainty: "Объяснение ИИ сейчас недоступно.", Generated: false}
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
