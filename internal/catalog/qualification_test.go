package catalog

import (
	"testing"
	"time"

	"next-game-crush/internal/steam"
)

func TestQualificationPolicy(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cfg := DefaultDiscoveryConfig()
	// Keep this legacy policy fixture's historical 90-day candidate set.
	cfg.HorizonDays = 90
	for _, tc := range []struct {
		raw, product, status string
		soon                 bool
		age                  time.Duration
		precision            Precision
	}{
		{"2027-01-16", "game", "admitted", true, 0, PrecisionDay},
		{"2027-01-17", "game", "outside_horizon", true, 0, PrecisionDay},
		{"January 2027", "game", "admitted", true, 0, PrecisionMonth},
		{"Q1 2027", "game", "admitted", true, 0, PrecisionQuarter},
		{"2027", "game", "admitted", true, 0, PrecisionYear},
		{"September 2026", "game", "outside_horizon", true, 0, PrecisionMonth},
		{"Coming Soon", "game", "admitted", true, 180 * 24 * time.Hour, PrecisionComingSoon},
		{"Coming Soon", "game", "outside_horizon", true, 180*24*time.Hour + time.Second, PrecisionComingSoon},
		{"TBA", "game", "admitted", true, 180 * 24 * time.Hour, PrecisionUnknown},
		{"TBA", "game", "outside_horizon", true, 180*24*time.Hour + time.Second, PrecisionUnknown},
		{"", "game", "admitted", true, 0, PrecisionComingSoon},
		{"2027-01-16", "game", "released", false, 0, PrecisionDay},
		{"2027-01-16", "DLC", "not_game", true, 0, PrecisionDay},
		{"2027-01-16", "software", "not_game", true, 0, PrecisionDay},
		{"2027-01-16", "demo", "not_game", true, 0, PrecisionDay},
		{"2027-01-16", " GAME ", "admitted", true, 0, PrecisionDay},
	} {
		t.Run(tc.raw+tc.product+tc.status, func(t *testing.T) {
			app := DiscoveredApp{AppID: 42, LastModified: now.Add(-tc.age)}
			details := steam.GameDetails{AppID: 42, Type: tc.product, ReleaseDate: steam.ReleaseDate{Raw: tc.raw, ComingSoon: tc.soon}}
			got := Qualify(app, details, now, cfg)
			if got.Status != tc.status || got.Window.Precision != tc.precision {
				t.Fatalf("got %s/%s want %s/%s", got.Status, got.Window.Precision, tc.status, tc.precision)
			}
			if got.Window.Precision != PrecisionDay && got.Window.Date != nil {
				t.Fatal("invented exact date")
			}
		})
	}
	if got := Qualify(DiscoveredApp{AppID: 0}, steam.GameDetails{AppID: 0, Type: "game"}, now, cfg); got.Status != "failed" {
		t.Fatal("invalid app admitted")
	}
	if got := Qualify(DiscoveredApp{AppID: 42, LastModified: now}, steam.GameDetails{AppID: 43, Type: "game"}, now, cfg); got.Status != "failed" {
		t.Fatal("mismatched AppID admitted")
	}
}
