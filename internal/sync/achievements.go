package sync

import (
	"sort"
	"time"
)

const AchievementTargetLimit = 30

type UserGame struct {
	AppID           int64
	Name            string
	PlaytimeMinutes int64
	RecentMinutes   int64
	LastPlayedAt    time.Time
}

func SelectAchievementTargets(games []UserGame, limit int) []int64 {
	if limit <= 0 || len(games) == 0 {
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
	if limit > len(candidates) {
		limit = len(candidates)
	}
	if limit > AchievementTargetLimit {
		limit = AchievementTargetLimit
	}
	result := make([]int64, 0, limit)
	seen := make(map[int64]struct{}, limit)
	for _, game := range candidates {
		if game.AppID <= 0 {
			continue
		}
		if _, exists := seen[game.AppID]; exists {
			continue
		}
		seen[game.AppID] = struct{}{}
		result = append(result, game.AppID)
		if len(result) == limit {
			break
		}
	}
	return result
}
