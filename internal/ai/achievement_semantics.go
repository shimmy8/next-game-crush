package ai

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	AchievementSemanticsPromptVersion = "achievement-semantics-v1"
	AchievementSemanticsSchemaVersion = "achievement-semantics-structured-output-v1"
)

const achievementSemanticsInstructions = `Classify achievement semantics using only the supplied shared game and achievement metadata. Return exactly one result for every supplied opaque evidence key. Select only one allowed semantic class per result. Do not infer from user identity, unlock state, playtime, feedback, profile data, or ranking data. Do not return confidence, weights, scores, eligibility, or ordering.`

type AchievementSemanticClass string

const (
	AchievementSemanticBaseGameEnding        AchievementSemanticClass = "base_game_ending"
	AchievementSemanticBaseGameCompletion    AchievementSemanticClass = "base_game_completion"
	AchievementSemanticDLCEndingOrCompletion AchievementSemanticClass = "dlc_ending_or_completion"
	AchievementSemanticMetaEndingCollection  AchievementSemanticClass = "meta_ending_collection"
	AchievementSemanticPartialProgress       AchievementSemanticClass = "partial_progress"
	AchievementSemanticOptionalChallenge     AchievementSemanticClass = "optional_challenge"
	AchievementSemanticOther                 AchievementSemanticClass = "other"
	AchievementSemanticUnknown               AchievementSemanticClass = "unknown"
)

type AchievementSemanticItem struct {
	EvidenceKey string `json:"evidence_key"`
	APIName     string `json:"api_name"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type AchievementSemanticEvidence struct {
	EvidenceKey  string                    `json:"evidence_key"`
	GameName     string                    `json:"game_name"`
	Achievements []AchievementSemanticItem `json:"achievements"`
}

type AchievementSemanticResult struct {
	EvidenceKey string                   `json:"evidence_key"`
	Class       AchievementSemanticClass `json:"class"`
}

type AchievementSemanticClassification struct {
	Results []AchievementSemanticResult `json:"results"`
}

func (client *OpenAI) ClassifyAchievementSemantics(ctx context.Context, input AchievementSemanticEvidence) (AchievementSemanticClassification, error) {
	var output AchievementSemanticClassification
	normalized, allowedKeys, err := normalizeAchievementSemanticEvidence(input)
	if err != nil {
		return output, err
	}
	if err := client.structured(ctx, "achievement_semantics_v1", achievementSemanticsInstructions, AchievementSemanticsPromptVersion, achievementSemanticsSchema(allowedKeys), normalized, &output); err != nil {
		return AchievementSemanticClassification{}, err
	}
	if err := ValidateAchievementSemanticClassification(output, allowedKeys); err != nil {
		return AchievementSemanticClassification{}, err
	}
	return output, nil
}

func normalizeAchievementSemanticEvidence(input AchievementSemanticEvidence) (AchievementSemanticEvidence, []string, error) {
	if !validSemanticText(input.GameName, 500) {
		return AchievementSemanticEvidence{}, nil, fmt.Errorf("%w: invalid achievement semantic evidence", ErrInvalidInput)
	}
	input.EvidenceKey = strings.TrimSpace(input.EvidenceKey)
	input.GameName = sanitizeText(input.GameName)
	if !validOpaqueKey(input.EvidenceKey) || input.GameName == "" || len(input.Achievements) == 0 || len(input.Achievements) > 200 {
		return AchievementSemanticEvidence{}, nil, fmt.Errorf("%w: invalid achievement semantic evidence", ErrInvalidInput)
	}
	seen := make(map[string]struct{}, len(input.Achievements))
	for i := range input.Achievements {
		item := &input.Achievements[i]
		if !validSemanticText(item.APIName, 500) || !validSemanticText(item.Name, 1000) || !validSemanticText(item.Description, 4000) {
			return AchievementSemanticEvidence{}, nil, fmt.Errorf("%w: invalid achievement semantic item", ErrInvalidInput)
		}
		item.EvidenceKey = strings.TrimSpace(item.EvidenceKey)
		item.APIName = sanitizeText(item.APIName)
		item.Name = sanitizeText(item.Name)
		item.Description = sanitizeText(item.Description)
		if !validOpaqueKey(item.EvidenceKey) || item.APIName == "" || item.Name == "" && item.Description == "" {
			return AchievementSemanticEvidence{}, nil, fmt.Errorf("%w: invalid achievement semantic item", ErrInvalidInput)
		}
		if _, duplicate := seen[item.EvidenceKey]; duplicate {
			return AchievementSemanticEvidence{}, nil, fmt.Errorf("%w: duplicate achievement evidence key", ErrInvalidInput)
		}
		seen[item.EvidenceKey] = struct{}{}
	}
	sort.Slice(input.Achievements, func(i, j int) bool { return input.Achievements[i].EvidenceKey < input.Achievements[j].EvidenceKey })
	keys := make([]string, len(input.Achievements))
	for i, item := range input.Achievements {
		keys[i] = item.EvidenceKey
	}
	return input, keys, nil
}

func validSemanticText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxRunes
}

func ValidateAchievementSemanticClassification(output AchievementSemanticClassification, expectedKeys []string) error {
	if output.Results == nil {
		return ErrInvalidOutput
	}
	if len(expectedKeys) == 0 {
		return ErrInvalidEvidence
	}
	expected := make(map[string]struct{}, len(expectedKeys))
	for _, key := range expectedKeys {
		if !validOpaqueKey(key) {
			return ErrInvalidEvidence
		}
		if _, duplicate := expected[key]; duplicate {
			return ErrInvalidEvidence
		}
		expected[key] = struct{}{}
	}
	if len(output.Results) != len(expected) {
		return ErrInvalidEvidence
	}
	seen := make(map[string]struct{}, len(output.Results))
	for _, result := range output.Results {
		if !validAchievementSemanticClass(result.Class) {
			return ErrInvalidOutput
		}
		if _, known := expected[result.EvidenceKey]; !known {
			return ErrInvalidEvidence
		}
		if _, duplicate := seen[result.EvidenceKey]; duplicate {
			return ErrInvalidEvidence
		}
		seen[result.EvidenceKey] = struct{}{}
	}
	return nil
}

func allAchievementSemanticClasses() []AchievementSemanticClass {
	return []AchievementSemanticClass{
		AchievementSemanticBaseGameEnding,
		AchievementSemanticBaseGameCompletion,
		AchievementSemanticDLCEndingOrCompletion,
		AchievementSemanticMetaEndingCollection,
		AchievementSemanticPartialProgress,
		AchievementSemanticOptionalChallenge,
		AchievementSemanticOther,
		AchievementSemanticUnknown,
	}
}

func validAchievementSemanticClass(value AchievementSemanticClass) bool {
	for _, allowed := range allAchievementSemanticClasses() {
		if value == allowed {
			return true
		}
	}
	return false
}

func achievementSemanticsSchema(allowedKeys []string) map[string]any {
	classes := allAchievementSemanticClasses()
	classValues := make([]string, len(classes))
	for i, class := range classes {
		classValues[i] = string(class)
	}
	result := objectSchema(map[string]any{
		"evidence_key": map[string]any{"type": "string", "enum": append([]string(nil), allowedKeys...)},
		"class":        map[string]any{"type": "string", "enum": classValues},
	}, "evidence_key", "class")
	results := arraySchema(result, len(allowedKeys))
	results["minItems"] = len(allowedKeys)
	return objectSchema(map[string]any{"results": results}, "results")
}
