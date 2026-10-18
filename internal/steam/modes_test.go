package steam

import (
	"next-game-crush/internal/preferences"
	"reflect"
	"testing"
)

func TestCanonicalModes(t *testing.T) {
	tests := []struct {
		id   int
		name string
		want []preferences.Mode
	}{
		{2, "Single-player", []preferences.Mode{preferences.ModeSinglePlayer}},
		{1, "Multi-player", []preferences.Mode{preferences.ModeMultiplayer}},
		{27, "Cross-Platform Multiplayer", []preferences.Mode{preferences.ModeMultiplayer}},
		{9, "Co-op", []preferences.Mode{preferences.ModeCoop}},
		{38, "Online Co-op", []preferences.Mode{preferences.ModeCoop, preferences.ModeOnline}},
		{39, "Shared/Split Screen Co-op", []preferences.Mode{preferences.ModeCoop, preferences.ModeLocal}},
		{36, "Online PvP", []preferences.Mode{preferences.ModeCompetitive, preferences.ModeOnline}},
		{37, "Shared/Split Screen PvP", []preferences.Mode{preferences.ModeCompetitive, preferences.ModeLocal}},
		{24, "Shared/Split Screen", []preferences.Mode{preferences.ModeLocal}},
		{20, "MMO", []preferences.Mode{preferences.ModeMassive}},
		{999, "PvP", []preferences.Mode{preferences.ModeCompetitive}},
		{999, "Unknown", []preferences.Mode{}},
		{2, "Online Co-op", []preferences.Mode{preferences.ModeSinglePlayer}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalModes([]Category{{ID: tt.id, Description: tt.name}, {ID: tt.id, Description: tt.name}}); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			if tt.id != 2 || tt.name == "Single-player" {
				if got := CanonicalModes([]Category{{ID: 999, Description: tt.name}}); !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("description got %v want %v", got, tt.want)
				}
			}
			if tt.id != 999 {
				if got := CanonicalModes([]Category{{ID: tt.id, Description: "unrecognized description"}}); !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("known ID projection got %v want %v", got, tt.want)
				}
			}
		})
	}
	if got := CanonicalModes(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty categories = %v", got)
	}
	if got := CanonicalModes([]Category{{ID: 999, Description: "  ONLINE   CO-OP  "}}); !reflect.DeepEqual(got, []preferences.Mode{preferences.ModeCoop, preferences.ModeOnline}) {
		t.Fatalf("normalized explicit description = %v", got)
	}
	if got := CanonicalModes([]Category{{ID: 999, Description: "Co-op adventure"}}); len(got) != 0 {
		t.Fatalf("inferred arbitrary text: %v", got)
	}
	categories := []Category{{ID: 38}, {ID: 2}, {ID: 1}, {ID: 9}, {ID: 38}}
	want := []preferences.Mode{preferences.ModeCoop, preferences.ModeMultiplayer, preferences.ModeOnline, preferences.ModeSinglePlayer}
	if got := CanonicalModes(categories); !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted projection = %v", got)
	}
	for i, j := 0, len(categories)-1; i < j; i, j = i+1, j-1 {
		categories[i], categories[j] = categories[j], categories[i]
	}
	if got := CanonicalModes(categories); !reflect.DeepEqual(got, want) {
		t.Fatalf("order dependent projection = %v", got)
	}
}
