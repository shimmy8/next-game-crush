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

func TestReleaseWindowInterval(t *testing.T) {
	for _, tc := range []struct{ raw, start, end string }{
		{"2024-02-29", "2024-02-29", "2024-02-29"},
		{"February 2024", "2024-02-01", "2024-02-29"},
		{"February 2023", "2023-02-01", "2023-02-28"},
		{"December 2026", "2026-12-01", "2026-12-31"},
		{"Q1 2024", "2024-01-01", "2024-03-31"},
		{"Q4 2026", "2026-10-01", "2026-12-31"},
		{"2024", "2024-01-01", "2024-12-31"},
		{"Coming Soon", "", ""}, {"TBA", "", ""}, {"invalid", "", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			window := ParseReleaseDate(tc.raw, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
			start, end, ok := window.Interval()
			if ok != (tc.start != "") {
				t.Fatalf("interval validity = %v", ok)
			}
			if ok && (start.Format(time.DateOnly) != tc.start || end.Format(time.DateOnly) != tc.end || start.Location() != time.UTC || end.Location() != time.UTC || start.Hour() != 0 || end.Hour() != 0) {
				t.Fatalf("interval = %s..%s, want %s..%s UTC midnight", start, end, tc.start, tc.end)
			}
			if window.Precision != PrecisionDay && window.Date != nil {
				t.Fatal("interval fabricated exact day")
			}
		})
	}
	for _, window := range []ReleaseWindow{
		{Precision: PrecisionDay}, {Precision: PrecisionMonth, Year: 0, Month: 1},
		{Precision: PrecisionMonth, Year: 2026, Month: 13}, {Precision: PrecisionQuarter, Year: 2026, Quarter: 0},
		{Precision: PrecisionQuarter, Year: 2026, Quarter: 5}, {Precision: PrecisionYear, Year: 10000},
	} {
		if _, _, ok := window.Interval(); ok {
			t.Fatalf("invalid interval accepted: %#v", window)
		}
	}
}
