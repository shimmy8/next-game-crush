package completion

import (
	"testing"

	"next-game-crush/internal/ai"
)

func TestClassifyLexicallyConservativeRules(t *testing.T) {
	if LexicalRuleVersion != "lexical-completion-v1" {
		t.Fatalf("LexicalRuleVersion = %q", LexicalRuleVersion)
	}
	tests := []struct {
		name, title, description string
		want                     ai.AchievementSemanticClass
		ok                       bool
	}{
		{name: "complete game", title: "Complete the game", want: ai.AchievementSemanticBaseGameCompletion, ok: true},
		{name: "completed main story", description: "COMPLETED   THE MAIN STORY!", want: ai.AchievementSemanticBaseGameCompletion, ok: true},
		{name: "finish campaign", title: "Finish campaign.", want: ai.AchievementSemanticBaseGameCompletion, ok: true},
		{name: "finished story punctuation", title: "Milestone", description: "Finished—the story", want: ai.AchievementSemanticBaseGameCompletion, ok: true},
		{name: "reach an ending", title: "Reach an ending", want: ai.AchievementSemanticBaseGameEnding, ok: true},
		{name: "reached the ending", description: "Reached the ending!", want: ai.AchievementSemanticBaseGameEnding, ok: true},
		{name: "reach epilogue", title: "Reach the epilogue", want: ai.AchievementSemanticBaseGameEnding, ok: true},
		{name: "sentence split", description: "A quiet reward. Finish the main game!", want: ai.AchievementSemanticBaseGameCompletion, ok: true},
		{name: "unicode sentence split", description: "A quiet reward…Finish the main game！", want: ai.AchievementSemanticBaseGameCompletion, ok: true},
		{name: "api name only", title: "Victory", description: "A reward", ok: false},
		{name: "non english", title: "Пройти игру", ok: false},
		{name: "vague", title: "Done", ok: false},
		{name: "unrelated", description: "Defeat the final boss", ok: false},
		{name: "substring", description: "You should finish the game before dawn", ok: false},
		{name: "chapter veto", title: "Finish the game", description: "Complete chapter 4", ok: false},
		{name: "act veto", title: "Complete the game", description: "Finish act two", ok: false},
		{name: "mission veto", title: "Finish the story", description: "Complete mission seven", ok: false},
		{name: "level veto", title: "Complete campaign", description: "Clear level 10", ok: false},
		{name: "dlc veto", title: "Finish the game", description: "DLC finale", ok: false},
		{name: "expansion veto", title: "Reach an ending", description: "Expansion story", ok: false},
		{name: "ng plus veto", title: "Finish the campaign", description: "NG+", ok: false},
		{name: "new game plus veto", title: "Finish the campaign", description: "New Game Plus", ok: false},
		{name: "difficulty veto", title: "Complete the game", description: "Hard difficulty", ok: false},
		{name: "speedrun veto", title: "Complete the game", description: "Speedrun", ok: false},
		{name: "collection veto", title: "Reach the ending", description: "Collection reward", ok: false},
		{name: "all endings veto", title: "Reach an ending", description: "Unlock all endings", ok: false},
		{name: "multiple endings veto", title: "Reach an ending", description: "See multiple endings", ok: false},
		{name: "unlock plural endings veto", title: "Reach an ending", description: "Unlock endings", ok: false},
		{name: "collect the endings veto", title: "Reach an ending", description: "Collect the endings", ok: false},
		{name: "meta veto", title: "Finish the game", description: "Meta achievement", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ClassifyLexically(test.title, test.description)
			if ok != test.ok || got != test.want && test.ok || !test.ok && got != ai.AchievementSemanticUnknown {
				t.Fatalf("ClassifyLexically(%q, %q) = (%q, %v), want (%q, %v)", test.title, test.description, got, ok, test.want, test.ok)
			}
		})
	}
}
