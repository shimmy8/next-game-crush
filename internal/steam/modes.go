package steam

import (
	"next-game-crush/internal/preferences"
	"sort"
	"strings"
)

// CanonicalModes projects only explicit Steam category evidence. Recognized IDs
// take precedence over descriptions so contradictory metadata cannot add modes.
func CanonicalModes(categories []Category) []preferences.Mode {
	set := make(map[preferences.Mode]bool)
	for _, category := range categories {
		modes, known := categoryModes(category.ID)
		if !known {
			switch strings.ToLower(strings.Join(strings.Fields(category.Description), " ")) {
			case "single-player":
				modes, _ = categoryModes(2)
			case "multi-player", "cross-platform multiplayer":
				modes, _ = categoryModes(1)
			case "co-op":
				modes, _ = categoryModes(9)
			case "online co-op":
				modes, _ = categoryModes(38)
			case "shared/split screen co-op":
				modes, _ = categoryModes(39)
			case "online pvp":
				modes, _ = categoryModes(36)
			case "shared/split screen pvp":
				modes, _ = categoryModes(37)
			case "shared/split screen":
				modes, _ = categoryModes(24)
			case "mmo":
				modes, _ = categoryModes(20)
			case "pvp":
				modes = []preferences.Mode{preferences.ModeCompetitive}
			}
		}
		for _, mode := range modes {
			set[mode] = true
		}
	}
	result := make([]preferences.Mode, 0, len(set))
	for mode := range set {
		result = append(result, mode)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func categoryModes(id int) ([]preferences.Mode, bool) {
	switch id {
	case 2:
		return []preferences.Mode{preferences.ModeSinglePlayer}, true
	case 1, 27:
		return []preferences.Mode{preferences.ModeMultiplayer}, true
	case 9:
		return []preferences.Mode{preferences.ModeCoop}, true
	case 38:
		return []preferences.Mode{preferences.ModeCoop, preferences.ModeOnline}, true
	case 39:
		return []preferences.Mode{preferences.ModeCoop, preferences.ModeLocal}, true
	case 36:
		return []preferences.Mode{preferences.ModeOnline, preferences.ModeCompetitive}, true
	case 37:
		return []preferences.Mode{preferences.ModeLocal, preferences.ModeCompetitive}, true
	case 24:
		return []preferences.Mode{preferences.ModeLocal}, true
	case 20:
		return []preferences.Mode{preferences.ModeMassive}, true
	}
	return nil, false
}
