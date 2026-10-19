package ai

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDeterministicActivitySummaryUsesOnlyAggregateFacts(t *testing.T) {
	got := DeterministicActivitySummary(ActivityFacts{
		PlayedGames: 7, RecentlyPlayedGames: 2, GamesWithAchievements: 4, CompletedGames: 1,
	})
	for _, want := range []string{"7", "2", "4", "1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q does not contain aggregate %q", got, want)
		}
	}
	for _, forbidden := range []string{"Portal", "жанр", "механик", "wishlist", "реакц"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(forbidden)) {
			t.Fatalf("summary %q contains unsupported claim %q", got, forbidden)
		}
	}
	if strings.TrimSpace(got) == "" || !utf8.ValidString(got) || utf8.RuneCountInString(got) > 4000 {
		t.Fatalf("invalid deterministic summary %q", got)
	}
}
