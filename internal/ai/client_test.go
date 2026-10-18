package ai

import (
	"strings"
	"testing"
)

func TestDeterministicExplanationUsesRussianHumanReadableCopy(t *testing.T) {
	got := DeterministicExplanation([]string{
		"Игра есть в вашем списке желаемого",
		"Совпадает с вашими любимыми жанрами и метками",
	})

	if got.Generated {
		t.Fatal("deterministic explanation must not be marked generated")
	}
	if !strings.Contains(got.Text, "Рекомендуем") || !strings.Contains(got.Uncertainty, "ИИ") {
		t.Fatalf("fallback copy is not Russian: %#v", got)
	}
	for _, forbidden := range []string{"Recommended", "ranking factors", "AI explanation unavailable"} {
		if strings.Contains(got.Text+got.Uncertainty, forbidden) {
			t.Fatalf("fallback contains technical English copy %q: %#v", forbidden, got)
		}
	}
}
