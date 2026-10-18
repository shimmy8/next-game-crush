// Package preferences defines deterministic preference targets shared by catalog
// evidence, feedback validation and ranking.
package preferences

import "strings"

type Mode string

const (
	ModeCoop         Mode = "co-op"
	ModeCompetitive  Mode = "competitive-multiplayer"
	ModeMassive      Mode = "massively-multiplayer"
	ModeLocal        Mode = "local-multiplayer"
	ModeOnline       Mode = "online-multiplayer"
	ModeSinglePlayer Mode = "single-player"
	ModeMultiplayer  Mode = "multiplayer"
)

// Modes returns a fresh, lexically ordered copy of the canonical vocabulary.
func Modes() []Mode {
	return []Mode{ModeCoop, ModeCompetitive, ModeLocal, ModeMassive, ModeMultiplayer, ModeOnline, ModeSinglePlayer}
}

func ValidMode(mode Mode) bool {
	switch mode {
	case ModeCoop, ModeCompetitive, ModeMassive, ModeLocal, ModeOnline, ModeSinglePlayer, ModeMultiplayer:
		return true
	}
	return false
}

// NormalizeModeAlias recognizes whole aliases, never a title or feedback sentence.
func NormalizeModeAlias(value string) (Mode, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if mode := Mode(value); ValidMode(mode) {
		return mode, true
	}
	switch value {
	case "кооп", "кооператив", "coop", "co op":
		return ModeCoop, true
	case "соревновательный мультиплеер", "pvp":
		return ModeCompetitive, true
	case "mmo":
		return ModeMassive, true
	case "локальный мультиплеер":
		return ModeLocal, true
	case "онлайн-мультиплеер":
		return ModeOnline, true
	case "single player", "одиночная игра":
		return ModeSinglePlayer, true
	case "multi-player", "мультиплеер":
		return ModeMultiplayer, true
	}
	return "", false
}

func MatchesMode(target Mode, evidence []Mode) bool {
	if !ValidMode(target) {
		return false
	}
	for _, mode := range evidence {
		if !ValidMode(mode) {
			continue
		}
		if mode == target || target == ModeMultiplayer && mode != ModeSinglePlayer {
			return true
		}
	}
	return false
}

func ModeLabel(mode Mode) string {
	switch mode {
	case ModeCoop:
		return "Кооперативные игры"
	case ModeCompetitive:
		return "Соревновательные сетевые игры"
	case ModeMassive:
		return "Массовые многопользовательские игры"
	case ModeLocal:
		return "Локальные многопользовательские игры"
	case ModeOnline:
		return "Сетевые многопользовательские игры"
	case ModeSinglePlayer:
		return "Одиночные игры"
	case ModeMultiplayer:
		return "Многопользовательские игры"
	}
	return ""
}

func ModeEvidence(mode Mode) string {
	label := ModeLabel(mode)
	if label == "" {
		return ""
	}
	return "Вы просили реже предлагать " + strings.ToLower(label)
}

type Target struct {
	Dimension string `json:"dimension"`
	Target    Mode   `json:"target"`
	Direction string `json:"direction"`
}
