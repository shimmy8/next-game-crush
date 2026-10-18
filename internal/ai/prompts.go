package ai

const (
	FeaturePromptVersion     = "game-features-v1"
	ProfilePromptVersion     = "profile-summary-v2"
	ExplanationPromptVersion = "recommendation-explanation-v2"
	FeedbackPromptVersion    = "feedback-classification-v1"
	SchemaVersion            = "structured-output-v1"
)

const featureInstructions = `Extract concise semantic game features only from the supplied normalized evidence. Cite only supplied evidence keys. Do not infer release dates or scores.`
const profileInstructions = `Write the summary in Russian using only the supplied aggregated evidence. The summary must be 400–600 Unicode characters. Keep each preferences and avoidances item concise and at most 120 Unicode characters. Separate uncertainty and cite only supplied evidence keys.`
const explanationInstructions = `Write a short Russian recommendation explanation using only the supplied human-readable evidence. Keep facts, inferences, and uncertainty grounded in that evidence. Cite only supplied evidence keys in the structured evidence_keys field, but do not mention those keys, internal scores, factor names, JSON, or release precision in the user-facing text.`
const feedbackInstructions = `Classify the feedback into the fixed taxonomy. Use low confidence when ambiguous and cite the supplied evidence key.`
