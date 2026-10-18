package ai

import (
	"strings"
	"testing"
)

func TestFeedbackPromptDeclaresCanonicalNegativePreferences(t *testing.T) {
	if FeedbackPromptVersion != "feedback-classification-v2" || FeedbackSchemaVersion != "feedback-structured-output-v2" {
		t.Fatal("feedback versions must identify the preference contract")
	}
	for _, required := range []string{"preference null", "negative polarity", "0.70", "co-op", "single-player", "dimension mode", "direction avoid", "opaque evidence key"} {
		if !strings.Contains(feedbackInstructions, required) {
			t.Fatalf("feedback instructions missing %q", required)
		}
	}
}

func TestExplanationPromptRequiresSafeRussianPresentation(t *testing.T) {
	if ExplanationPromptVersion != "recommendation-explanation-v3" {
		t.Fatalf("ExplanationPromptVersion = %q", ExplanationPromptVersion)
	}
	for _, required := range []string{
		"short Russian recommendation explanation",
		"only the supplied human-readable evidence",
		"do not mention those keys",
		"internal scores",
		"factor names",
		"JSON",
		"release precision",
		"novelty",
		"wishlist membership",
		"must not be treated as affinity",
	} {
		if !strings.Contains(explanationInstructions, required) {
			t.Fatalf("explanation instructions do not contain %q: %q", required, explanationInstructions)
		}
	}
}

func TestProfilePromptContract(t *testing.T) {
	if ProfilePromptVersion != "profile-summary-v5-synthesis" {
		t.Fatalf("ProfilePromptVersion = %q", ProfilePromptVersion)
	}
	for _, required := range []string{
		"Russian",
		"reader-facing synthesis",
		"not a game-by-game inventory or data-quality audit",
		"650–900 Unicode characters",
		"3–5 observations",
		"at most four distinct game names",
		"preferences",
		"avoidances",
		"120 Unicode characters",
		"repeated only when the same supplied feature appears in multiple evidence items",
		"recent_minutes > 0",
		"direct_feedback=like",
		"wishlist-only evidence",
		"interest in the concept",
		"never as played experience or a confirmed preference",
		"lifetime activity is historical",
		"Low playtime does not weaken completion",
		"overridden completion is not positive",
		"unknown last_played_at does not imply recency",
		"never invent last-played or release dates",
		"at most one concise material caveat",
		"detailed caveats in uncertainty",
	} {
		if !strings.Contains(profileInstructions, required) {
			t.Fatalf("profile instructions do not contain %q: %q", required, profileInstructions)
		}
	}
}
