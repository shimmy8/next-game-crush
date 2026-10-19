package ai

import "fmt"

type ActivityFacts struct {
	PlayedGames           int
	RecentlyPlayedGames   int
	GamesWithAchievements int
	CompletedGames        int
}

// DeterministicActivitySummary deliberately describes only counted activity
// classes. It does not infer tastes, genres, mechanics, or reactions.
func DeterministicActivitySummary(facts ActivityFacts) string {
	return fmt.Sprintf("Профиль собран по сохранённой игровой активности: игр с игровым временем — %d, с активностью за последние две недели — %d, с полученными достижениями — %d, с подтверждённым прохождением — %d. Это фактическая сводка сохранённых сигналов без дополнительных выводов.", facts.PlayedGames, facts.RecentlyPlayedGames, facts.GamesWithAchievements, facts.CompletedGames)
}
