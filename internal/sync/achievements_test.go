package sync

import (
	"reflect"
	"testing"
	"time"
)

func TestSelectAchievementTargetsIsMeaningfulDeterministicAndCapped(t *testing.T) {
	recent := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	games := []UserGame{
		{AppID: 40, PlaytimeMinutes: 900},
		{AppID: 30, PlaytimeMinutes: 10, LastPlayedAt: recent},
		{AppID: 20, PlaytimeMinutes: 10, LastPlayedAt: recent},
		{AppID: 10, PlaytimeMinutes: 1200},
	}
	got := SelectAchievementTargets(games, 3)
	want := []int64{20, 30, 10}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}

	many := make([]UserGame, 50)
	for i := range many {
		many[i] = UserGame{AppID: int64(100 + i), PlaytimeMinutes: int64(i)}
	}
	if got := SelectAchievementTargets(many, 30); len(got) != 30 {
		t.Fatalf("target count = %d, want 30", len(got))
	}
	if got := SelectAchievementTargets(many, 100); len(got) != AchievementTargetLimit {
		t.Fatalf("intrinsic target cap = %d, want %d", len(got), AchievementTargetLimit)
	}
}
