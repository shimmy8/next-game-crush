package sync

import (
	"context"
	"sort"
	"sync"
	"time"

	"next-game-crush/internal/steam"
)

const achievementFetchConcurrency = 4

type UserGame struct {
	AppID           int64
	Name            string
	PlaytimeMinutes int64
	RecentMinutes   int64
	LastPlayedAt    time.Time
}

type achievementFetchResult struct {
	summary steam.AchievementSummary
	state   steam.Availability
	err     error
}

type achievementFetchFunc func(context.Context, int64) (steam.AchievementSummary, steam.Availability, error)

func SelectAchievementTargets(games []UserGame, cutoff time.Time) []int64 {
	if len(games) == 0 {
		return []int64{}
	}
	candidates := append([]UserGame(nil), games...)
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].LastPlayedAt.Equal(candidates[j].LastPlayedAt) {
			return candidates[i].LastPlayedAt.After(candidates[j].LastPlayedAt)
		}
		if candidates[i].PlaytimeMinutes != candidates[j].PlaytimeMinutes {
			return candidates[i].PlaytimeMinutes > candidates[j].PlaytimeMinutes
		}
		return candidates[i].AppID < candidates[j].AppID
	})
	result := make([]int64, 0, len(candidates))
	seen := make(map[int64]struct{}, len(candidates))
	for _, game := range candidates {
		if game.AppID <= 0 || game.LastPlayedAt.IsZero() || game.LastPlayedAt.Before(cutoff) {
			continue
		}
		if _, exists := seen[game.AppID]; exists {
			continue
		}
		seen[game.AppID] = struct{}{}
		result = append(result, game.AppID)
	}
	return result
}

func fetchAchievementResults(ctx context.Context, targets []int64, concurrency int, fetch achievementFetchFunc) []achievementFetchResult {
	results := make([]achievementFetchResult, len(targets))
	if len(targets) == 0 {
		return results
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(targets) {
		concurrency = len(targets)
	}

	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for index := range jobs {
				summary, state, err := fetch(ctx, targets[index])
				results[index] = achievementFetchResult{summary: summary, state: state, err: err}
			}
		}()
	}
	for index := range targets {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}
