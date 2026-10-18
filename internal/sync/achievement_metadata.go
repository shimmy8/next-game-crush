package sync

import (
	"strings"

	"next-game-crush/internal/steam"
)

func achievementMetadataMissing(summary steam.AchievementSummary) bool {
	for _, achievement := range summary.Achievements {
		if strings.TrimSpace(achievement.Name) == "" || strings.TrimSpace(achievement.Description) == "" {
			return true
		}
	}
	return false
}

func mergeAchievementSchema(summary steam.AchievementSummary, schema steam.AchievementSchema) steam.AchievementSummary {
	definitions := make(map[string]steam.AchievementDefinition, len(schema.Achievements))
	for _, definition := range schema.Achievements {
		apiName := strings.TrimSpace(definition.APIName)
		if apiName != "" {
			definition.APIName = apiName
			definitions[apiName] = definition
		}
	}
	merged := summary
	merged.Achievements = append([]steam.Achievement(nil), summary.Achievements...)
	for index := range merged.Achievements {
		achievement := &merged.Achievements[index]
		definition, ok := definitions[strings.TrimSpace(achievement.APIName)]
		if !ok {
			continue
		}
		if strings.TrimSpace(achievement.Name) == "" {
			achievement.Name = strings.TrimSpace(definition.Name)
			if achievement.Name != "" {
				achievement.MetadataSource = "game_schema"
			}
		}
		if strings.TrimSpace(achievement.Description) == "" {
			achievement.Description = strings.TrimSpace(definition.Description)
			if achievement.Description != "" {
				achievement.MetadataSource = "game_schema"
			}
		}
	}
	return merged
}
