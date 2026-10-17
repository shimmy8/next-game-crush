package ai

const (
	FeaturePromptVersion     = "game-features-v1"
	ProfilePromptVersion     = "profile-summary-v1"
	ExplanationPromptVersion = "recommendation-explanation-v1"
	FeedbackPromptVersion    = "feedback-classification-v1"
	SchemaVersion            = "structured-output-v1"
)

const featureInstructions = `Extract concise semantic game features only from the supplied normalized evidence. Cite only supplied evidence keys. Do not infer release dates or scores.`
const profileInstructions = `Summarize preferences using only the supplied aggregated evidence. Separate uncertainty and cite only supplied evidence keys.`
const explanationInstructions = `Explain the saved deterministic recommendation factors. Separate facts, inferences, and uncertainty. Never change the supplied score or release information. Cite only supplied evidence keys.`
const feedbackInstructions = `Classify the feedback into the fixed taxonomy. Use low confidence when ambiguous and cite the supplied evidence key.`
