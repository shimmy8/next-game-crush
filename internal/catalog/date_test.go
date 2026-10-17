package catalog

import (
	"testing"
	"time"
)

func TestParseReleaseDatePreservesPrecision(t *testing.T) {
	now := time.Date(2026, time.October, 17, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		raw       string
		precision Precision
		year      int
		month     time.Month
		quarter   int
		exact     string
	}{
		{"21 Oct, 2026", PrecisionDay, 2026, time.October, 0, "2026-10-21"},
		{"October 2026", PrecisionMonth, 2026, time.October, 0, ""},
		{"October, 2026", PrecisionMonth, 2026, time.October, 0, ""},
		{"Oct 2026", PrecisionMonth, 2026, time.October, 0, ""},
		{"Q4 2026", PrecisionQuarter, 2026, 0, 4, ""},
		{"2027", PrecisionYear, 2027, 0, 0, ""},
		{"Coming Soon", PrecisionComingSoon, 0, 0, 0, ""},
		{"TBA", PrecisionUnknown, 0, 0, 0, ""},
		{"32 Smarch 20XX", PrecisionUnknown, 0, 0, 0, ""},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			got := ParseReleaseDate(test.raw, now)
			if got.Precision != test.precision || got.Year != test.year || got.Month != test.month || got.Quarter != test.quarter {
				t.Fatalf("ParseReleaseDate(%q) = %#v", test.raw, got)
			}
			if test.exact == "" {
				if got.Date != nil {
					t.Fatalf("ParseReleaseDate(%q) fabricated exact date %s", test.raw, got.Date)
				}
			} else if got.Date == nil || got.Date.Format(time.DateOnly) != test.exact {
				t.Fatalf("ParseReleaseDate(%q) date = %v, want %s", test.raw, got.Date, test.exact)
			}
		})
	}
}
