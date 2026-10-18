package completion

import (
	"regexp"
	"strings"
	"unicode"

	"next-game-crush/internal/ai"
)

const LexicalRuleVersion = "lexical-completion-v1"

var sentenceBoundary = regexp.MustCompile(`[.!?;…。！？\n\r]+`)

var completionClauses = map[string]struct{}{
	"complete game": {}, "complete the game": {}, "completed game": {}, "completed the game": {},
	"finish game": {}, "finish the game": {}, "finished game": {}, "finished the game": {},
	"complete main game": {}, "complete the main game": {}, "completed main game": {}, "completed the main game": {},
	"finish main game": {}, "finish the main game": {}, "finished main game": {}, "finished the main game": {},
	"complete main story": {}, "complete the main story": {}, "completed main story": {}, "completed the main story": {},
	"finish main story": {}, "finish the main story": {}, "finished main story": {}, "finished the main story": {},
	"complete story": {}, "complete the story": {}, "completed story": {}, "completed the story": {},
	"finish story": {}, "finish the story": {}, "finished story": {}, "finished the story": {},
	"complete campaign": {}, "complete the campaign": {}, "completed campaign": {}, "completed the campaign": {},
	"finish campaign": {}, "finish the campaign": {}, "finished campaign": {}, "finished the campaign": {},
}

var endingClauses = map[string]struct{}{
	"reach an ending": {}, "reach the ending": {}, "reached an ending": {}, "reached the ending": {},
	"reach an epilogue": {}, "reach the epilogue": {}, "reached an epilogue": {}, "reached the epilogue": {},
}

var lexicalVetoes = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:chapter|act|mission|level)\b`),
	regexp.MustCompile(`\b(?:dlc|expansion)\b`),
	regexp.MustCompile(`\b(?:ng|new game plus)\b`),
	regexp.MustCompile(`\b(?:difficulty|speedrun|collection|mastery)\b`),
	regexp.MustCompile(`\b(?:all|multiple|every) endings?\b`),
	regexp.MustCompile(`\b(?:unlock|unlocked|unlocking|see|saw|seen|view|viewed|viewing|collect|collected|collecting)(?: the)? endings\b`),
	regexp.MustCompile(`\bmeta(?: achievement)?\b`),
}

func ClassifyLexically(name, description string) (ai.AchievementSemanticClass, bool) {
	raw := name + "." + description
	normalized := normalizeLexical(raw)
	for _, veto := range lexicalVetoes {
		if veto.MatchString(normalized) {
			return ai.AchievementSemanticUnknown, false
		}
	}
	for _, clause := range sentenceBoundary.Split(raw, -1) {
		clause = normalizeLexical(clause)
		if _, ok := completionClauses[clause]; ok {
			return ai.AchievementSemanticBaseGameCompletion, true
		}
		if _, ok := endingClauses[clause]; ok {
			return ai.AchievementSemanticBaseGameEnding, true
		}
	}
	return ai.AchievementSemanticUnknown, false
}

func normalizeLexical(value string) string {
	value = strings.ToLower(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return ' '
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
