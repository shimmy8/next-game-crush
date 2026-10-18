package sync

import (
	"testing"

	"next-game-crush/internal/steam"
)

func TestMergeAchievementSchemaUsesOnlyExactAPINamesAndFillsBlanks(t *testing.T) {
	summary := steam.AchievementSummary{Achievements: []steam.Achievement{
		{APIName: "finish", Name: "Player name"},
		{APIName: "case-sensitive"},
	}}
	schema := steam.AchievementSchema{Achievements: []steam.AchievementDefinition{
		{APIName: "finish", Name: "Schema name", Description: "Schema description"},
		{APIName: "CASE-SENSITIVE", Name: "Wrong", Description: "Wrong"},
	}}
	got := mergeAchievementSchema(summary, schema)
	if got.Achievements[0].Name != "Player name" || got.Achievements[0].Description != "Schema description" {
		t.Fatalf("matching definition merge = %+v", got.Achievements[0])
	}
	if got.Achievements[1].Name != "" || got.Achievements[1].Description != "" {
		t.Fatalf("non-exact API name merged = %+v", got.Achievements[1])
	}
	if !achievementMetadataMissing(got) {
		t.Fatal("unresolved metadata was reported complete")
	}
}
