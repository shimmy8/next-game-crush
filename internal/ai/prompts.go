package ai

const (
	FeaturePromptVersion     = "game-features-v1"
	ProfilePromptVersion     = "profile-summary-v4-completion"
	ExplanationPromptVersion = "recommendation-explanation-v3"
	FeedbackPromptVersion    = "feedback-classification-v2"
	FeedbackSchemaVersion    = "feedback-structured-output-v2"
	SchemaVersion            = "structured-output-v1"
)

const featureInstructions = `Extract concise semantic game features only from the supplied normalized evidence. Cite only supplied evidence keys. Do not infer release dates or scores.`
const profileInstructions = `Write the summary in Russian using only the supplied aggregated evidence. The summary must be 400–600 Unicode characters. Keep each preferences and avoidances item concise and at most 120 Unicode characters. recent_minutes is Steam two-week activity and may support current-interest wording. completion is historical and does not by itself imply current interest; low playtime does not weaken completion. An overridden completion is not positive evidence and must not support positive preference prose. Treat playtime_minutes and achievements as historical lifetime activity and use last_played_at plus recent_minutes to distinguish historical preferences from current interests. The older last_played_at is, the less it indicates current taste; never describe a long-inactive game as a current interest solely because lifetime playtime, achievements, or completion are high; unknown dates do not imply recency. Separate uncertainty and cite only supplied evidence keys.`
const explanationInstructions = `Write a short Russian recommendation explanation using only the supplied human-readable evidence. Keep facts, inferences, and uncertainty grounded in that evidence. Cite only supplied evidence keys in the structured evidence_keys field, but do not mention those keys, internal scores, factor names, JSON, or release precision in the user-facing text. Do not claim novelty, a new experience, or that the game expands the user's interests. Treat wishlist membership as context only; it must not be treated as affinity or evidence that the user will enjoy the game.`
const feedbackInstructions = `Classify only the supplied feedback text into the fixed taxonomy and cite the supplied opaque evidence key. Return preference null unless the text explicitly requests avoiding a supported game mode with negative polarity, actionable true, and confidence at least 0.70. Canonical targets are co-op, competitive-multiplayer, massively-multiplayer, local-multiplayer, online-multiplayer, single-player, and multiplayer. Кооп, кооператив, coop, and co-op mean co-op. Output canonical targets only. A preference must use dimension mode and direction avoid. Use taxonomy single-player only for target single-player; taxonomy multiplayer for all other mode targets. Use low confidence when ambiguous.`
