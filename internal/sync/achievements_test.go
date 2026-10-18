package sync

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"next-game-crush/internal/steam"
)

func TestSelectAchievementTargetsUsesOneYearWindowWithoutCountCap(t *testing.T) {
	cutoff := time.Date(2025, 10, 17, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	games := []UserGame{
		{AppID: 50, PlaytimeMinutes: 900},
		{AppID: 40, PlaytimeMinutes: 900, LastPlayedAt: cutoff.Add(-time.Second)},
		{AppID: 30, PlaytimeMinutes: 10, LastPlayedAt: recent},
		{AppID: 20, PlaytimeMinutes: 10, LastPlayedAt: recent},
		{AppID: 10, PlaytimeMinutes: 1200, LastPlayedAt: cutoff},
	}
	got := SelectAchievementTargets(games, cutoff)
	want := []int64{20, 30, 10}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}

	many := make([]UserGame, 35)
	for i := range many {
		many[i] = UserGame{AppID: int64(100 + i), PlaytimeMinutes: int64(i), LastPlayedAt: recent}
	}
	if got := SelectAchievementTargets(many, cutoff); len(got) != len(many) {
		t.Fatalf("target count = %d, want %d", len(got), len(many))
	}
}

func TestFetchAchievementResultsLimitsConcurrencyAndPreservesOrder(t *testing.T) {
	targets := []int64{10, 20, 30, 40, 50, 60}
	started := make(chan struct{}, len(targets))
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32

	done := make(chan []achievementFetchResult, 1)
	go func() {
		done <- fetchAchievementResults(context.Background(), targets, 4, func(_ context.Context, appID int64) (steam.AchievementSummary, steam.Availability, error) {
			current := active.Add(1)
			for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return steam.AchievementSummary{AppID: appID}, steam.Available, nil
		})
	}()

	for range 4 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("four achievement requests did not start concurrently")
		}
	}
	select {
	case <-started:
		t.Fatal("more than four achievement requests ran concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	results := <-done
	if maximum.Load() != 4 {
		t.Fatalf("maximum concurrency = %d, want 4", maximum.Load())
	}
	for i, result := range results {
		if result.summary.AppID != targets[i] {
			t.Fatalf("result %d app ID = %d, want %d", i, result.summary.AppID, targets[i])
		}
	}
}
