package preferences

import "testing"

func TestNormalizeModeAlias(t *testing.T) {
	for _, alias := range []string{"кооп", "кооператив", "coop", "co-op", " CO-OP "} {
		if got, ok := NormalizeModeAlias(alias); !ok || got != ModeCoop {
			t.Errorf("alias %q = %q, %v", alias, got, ok)
		}
	}
	if _, ok := NormalizeModeAlias("не люблю коопы"); ok {
		t.Fatal("arbitrary feedback text recognized as a mode")
	}
	if _, ok := NormalizeModeAlias("unknown"); ok {
		t.Fatal("unknown alias accepted")
	}
	all := Modes()
	if len(all) != 7 {
		t.Fatalf("modes: %v", all)
	}
	for _, mode := range all {
		if !ValidMode(mode) || ModeLabel(mode) == "" || ModeEvidence(mode) == "" {
			t.Errorf("incomplete mode %q", mode)
		}
		if got, ok := NormalizeModeAlias(string(mode)); !ok || got != mode {
			t.Errorf("canonical mode %q did not normalize", mode)
		}
	}
	all[0] = "modified"
	if !ValidMode(Modes()[0]) {
		t.Fatal("Modes returned shared storage")
	}
	if ValidMode("unknown") || ModeLabel("unknown") != "" || ModeEvidence("unknown") != "" {
		t.Fatal("unknown mode accepted")
	}
}

func TestModeMatching(t *testing.T) {
	for _, mode := range Modes() {
		if !MatchesMode(mode, []Mode{mode}) {
			t.Errorf("mode %q does not match itself", mode)
		}
		if MatchesMode(ModeMultiplayer, []Mode{mode}) != (mode != ModeSinglePlayer) {
			t.Errorf("broad matching %q", mode)
		}
	}
	for _, evidence := range [][]Mode{nil, {ModeSinglePlayer}, {"unknown"}} {
		if MatchesMode(ModeMultiplayer, evidence) {
			t.Errorf("broad rule matched %v", evidence)
		}
	}
	if MatchesMode(ModeCoop, []Mode{ModeMultiplayer, ModeOnline}) || MatchesMode("unknown", []Mode{"unknown"}) {
		t.Fatal("specific mode inferred")
	}
	if !MatchesMode(ModeCoop, []Mode{ModeSinglePlayer, ModeCoop}) || !MatchesMode(ModeCoop, []Mode{ModeCoop, ModeSinglePlayer}) {
		t.Fatal("matching depends on order")
	}
}
