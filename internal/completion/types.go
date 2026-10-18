package completion

import (
	"context"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
)

type SnapshotChange struct {
	DefinitionSetHash      string
	DefinitionsChanged     bool
	HundredPercentObserved bool
}

const ClassificationJobKind = "achievement.classify"

const ClassificationJobPayloadVersion = 1

type ClassificationSource string

const (
	ClassificationSourceReused  ClassificationSource = "reused"
	ClassificationSourceLexical ClassificationSource = "lexical"
	ClassificationSourceOpenAI  ClassificationSource = "openai"
	ClassificationSourceUnknown ClassificationSource = "unknown"
)

type SemanticClassifier interface {
	ClassifyAchievementSemantics(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error)
}

type JobPayload struct {
	Version           int       `json:"version"`
	GameID            uuid.UUID `json:"game_id"`
	DefinitionSetHash string    `json:"definition_set_hash"`
}

type StoredClassification struct {
	APIName               string
	DefinitionContentHash string
	Class                 ai.AchievementSemanticClass
	InputContentHash      string
	Source                ClassificationSource
}

type StagedClassification struct {
	APIName               string
	DefinitionContentHash string
	Class                 ai.AchievementSemanticClass
	InputContentHash      string
	Source                ClassificationSource
}
