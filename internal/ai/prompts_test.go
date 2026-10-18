package ai

import (
	"strings"
	"testing"
)

func TestExplanationPromptRequiresSafeRussianPresentation(t *testing.T) {
	if ExplanationPromptVersion != "recommendation-explanation-v2" {
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
	} {
		if !strings.Contains(explanationInstructions, required) {
			t.Fatalf("explanation instructions do not contain %q: %q", required, explanationInstructions)
		}
	}
}

func TestProfilePromptDeclaresRussianLengthContract(t *testing.T) {
	if ProfilePromptVersion != "profile-summary-v2" {
		t.Fatalf("ProfilePromptVersion = %q", ProfilePromptVersion)
	}
	for _, required := range []string{
		"Russian",
		"400–600 Unicode characters",
		"preferences",
		"avoidances",
		"120 Unicode characters",
	} {
		if !strings.Contains(profileInstructions, required) {
			t.Fatalf("profile instructions do not contain %q: %q", required, profileInstructions)
		}
	}
}
