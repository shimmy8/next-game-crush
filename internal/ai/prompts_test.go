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
	if ProfilePromptVersion != "profile-summary-v4-completion" {
		t.Fatalf("ProfilePromptVersion = %q", ProfilePromptVersion)
	}
	for _, required := range []string{
		"Russian",
		"400–600 Unicode characters",
		"preferences",
		"avoidances",
		"120 Unicode characters",
		"recent_minutes is Steam two-week activity",
		"completion is historical",
		"low playtime does not weaken completion",
		"overridden completion is not positive",
		"unknown dates do not imply recency",
	} {
		if !strings.Contains(profileInstructions, required) {
			t.Fatalf("profile instructions do not contain %q: %q", required, profileInstructions)
		}
	}
}
