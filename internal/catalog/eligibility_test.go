package catalog

import (
	"reflect"
	"testing"
	"time"

	"next-game-crush/internal/steam"
)

func eligibilityTime(raw string) time.Time {
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(err)
	}
	return value
}

func eligibilityReview(label string) steam.ReviewSummary {
	percent, total := 90, 100
	if label == "No user reviews" {
		return steam.NormalizeReview(label, nil, nil)
	}
	switch label {
	case "Mixed", "Mostly Negative", "Negative", "Very Negative", "Overwhelmingly Negative":
		percent = 30
	}
	return steam.NormalizeReview(label, &percent, &total)
}

func TestReleaseWindowEligibilityTable(t *testing.T) {
	now := eligibilityTime("2026-10-18T16:00:00Z")
	for _, tc := range []struct {
		name, raw, phase, reason, next  string
		soon, popular, wishlist, active bool
	}{
		{"released left inclusive", "2026-09-18", "released", "admitted", "2026-10-19T00:00:00Z", false, false, false, true},
		{"released left excluded", "2026-09-17", "released", "outside_window", "", false, true, true, false},
		{"released today", "2026-10-18", "released", "admitted", "2026-11-18T00:00:00Z", false, false, false, true},
		{"released future", "2026-10-19", "released", "outside_window", "2026-10-19T00:00:00Z", false, false, false, false},
		{"upcoming today", "2026-10-18", "upcoming", "admitted", "2026-10-19T00:00:00Z", true, false, false, true},
		{"upcoming past", "2026-10-17", "upcoming", "outside_window", "", true, false, false, false},
		{"upcoming right inclusive", "2026-11-17", "upcoming", "admitted", "2026-11-18T00:00:00Z", true, false, false, true},
		{"upcoming right excluded", "2026-11-18", "upcoming", "outside_window", "2026-10-19T00:00:00Z", true, true, true, false},
		{"released month intersects", "September 2026", "released", "admitted", "2026-10-31T00:00:00Z", false, false, false, true},
		{"released month before", "August 2026", "released", "outside_window", "", false, true, true, false},
		{"upcoming month intersects", "November 2026", "upcoming", "admitted", "2026-12-01T00:00:00Z", true, false, false, true},
		{"upcoming month future", "December 2026", "upcoming", "outside_window", "2026-11-01T00:00:00Z", true, false, false, false},
		{"upcoming month past", "September 2026", "upcoming", "outside_window", "", true, false, false, false},
		{"released quarter intersects", "Q3 2026", "released", "admitted", "2026-10-31T00:00:00Z", false, false, false, true},
		{"upcoming quarter intersects", "Q4 2026", "upcoming", "admitted", "2027-01-01T00:00:00Z", true, false, false, true},
		{"upcoming quarter future", "Q1 2027", "upcoming", "outside_window", "2026-12-02T00:00:00Z", true, false, false, false},
		{"year lacks provenance", "2026", "upcoming", "outside_window", "", true, false, false, false},
		{"year popular", "2026", "upcoming", "admitted", "", true, true, false, true},
		{"year wishlist", "2026", "released", "admitted", "", false, false, true, true},
		{"coming soon lacks provenance", "Coming Soon", "upcoming", "outside_window", "", true, false, false, false},
		{"coming soon popular", "Coming Soon", "upcoming", "admitted", "", true, true, false, true},
		{"unknown wishlist", "TBA", "upcoming", "admitted", "", true, false, true, true},
		{"malformed stays unknown without provenance", "32 Smarch 20XX", "upcoming", "outside_window", "", true, false, false, false},
		{"malformed preserved with provenance", "32 Smarch 20XX", "upcoming", "admitted", "", true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			window := ParseReleaseDate(tc.raw, now)
			before := window
			got := EvaluateEligibility(EligibilityInput{Now: now, Window: window, ComingSoon: tc.soon, Review: eligibilityReview("Positive"), PopularUpcoming: tc.popular, Wishlisted: tc.wishlist}, DefaultDiscoveryConfig())
			if got.Active != tc.active || got.Phase != tc.phase || got.Reason != tc.reason {
				t.Fatalf("decision = %#v, want active=%v phase=%s reason=%s", got, tc.active, tc.phase, tc.reason)
			}
			assertEligibilityNext(t, got.NextChangeAt, tc.next)
			if !reflect.DeepEqual(window, before) || window.Precision != PrecisionDay && window.Date != nil {
				t.Fatal("policy changed or fabricated release precision")
			}
		})
	}
}

func TestReleaseWindowEligibilityUTCAndCalendarBoundaries(t *testing.T) {
	for _, tc := range []struct{ now, raw, next string }{
		{"2024-03-01T01:00:00+03:00", "2024-01-30", "2024-03-01T00:00:00Z"},
		{"2024-03-30T12:00:00Z", "February 2024", "2024-03-31T00:00:00Z"},
		{"2027-01-30T12:00:00Z", "December 2026", "2027-01-31T00:00:00Z"},
	} {
		now := eligibilityTime(tc.now)
		got := EvaluateEligibility(EligibilityInput{Now: now, Window: ParseReleaseDate(tc.raw, now), Review: eligibilityReview("Positive")}, DefaultDiscoveryConfig())
		if !got.Active {
			t.Fatalf("%s at %s inactive: %#v", tc.raw, tc.now, got)
		}
		assertEligibilityNext(t, got.NextChangeAt, tc.next)
	}
}

func TestReleasedReviewGateTable(t *testing.T) {
	now := eligibilityTime("2026-10-18T23:59:59Z")
	for _, label := range []string{"Overwhelmingly Positive", "Very Positive", "Positive", "Mostly Positive", "Mixed", "Mostly Negative", "Negative", "Very Negative", "Overwhelmingly Negative"} {
		t.Run(label, func(t *testing.T) {
			review := eligibilityReview(label)
			input := EligibilityInput{Now: now, Window: ParseReleaseDate("2026-10-17", now), Review: review, PopularUpcoming: true, PopularNew: true, Wishlisted: true}
			got := EvaluateEligibility(input, DefaultDiscoveryConfig())
			wantActive, wantReason := review.Band == steam.ReviewPositive, "admitted"
			if !wantActive {
				wantReason = "mixed_or_below"
			}
			if got.Active != wantActive || got.Reason != wantReason {
				t.Fatalf("decision = %#v", got)
			}
			input.ComingSoon = true
			input.Window = ParseReleaseDate("2026-10-19", now)
			if upcoming := EvaluateEligibility(input, DefaultDiscoveryConfig()); !upcoming.Active {
				t.Fatalf("upcoming reviews are not required: %#v", upcoming)
			}
		})
	}
	for _, tc := range []struct {
		name, raw, reason, next string
		popular, active         bool
	}{
		{"release day popular", "2026-10-18", "admitted", "2026-10-26T00:00:00Z", true, true},
		{"day seven popular", "2026-10-11", "admitted", "2026-10-19T00:00:00Z", true, true},
		{"day eight popular", "2026-10-10", "unrated_grace_expired", "", true, false},
		{"not popular", "2026-10-18", "unrated_not_popular_new", "", false, false},
		{"removed popular", "2026-10-11", "unrated_not_popular_new", "", false, false},
		{"month imprecise", "October 2026", "unrated_grace_expired", "", true, false},
		{"quarter imprecise", "Q4 2026", "unrated_grace_expired", "", true, false},
		{"year imprecise", "2026", "unrated_grace_expired", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := EligibilityInput{Now: now, Window: ParseReleaseDate(tc.raw, now), Review: eligibilityReview("No user reviews"), PopularNew: tc.popular, Wishlisted: true, PopularUpcoming: true}
			got := EvaluateEligibility(input, DefaultDiscoveryConfig())
			if got.Active != tc.active || got.Reason != tc.reason {
				t.Fatalf("decision = %#v", got)
			}
			assertEligibilityNext(t, got.NextChangeAt, tc.next)
		})
	}
}

func TestReleasedReviewGateUnknownAndMalformedPreserveInput(t *testing.T) {
	now := eligibilityTime("2026-10-18T12:00:00Z")
	badPercent := 101
	for _, review := range []steam.ReviewSummary{
		{}, {Band: steam.ReviewUnknown},
		{Band: steam.ReviewPositive, Label: "Positive"},
		{Band: steam.ReviewPositive, Label: "Positive", Total: 3, PositivePercent: &badPercent},
		{Band: steam.ReviewUnrated, Label: "No user reviews", Total: 3},
		{Band: "invalid"},
	} {
		input := EligibilityInput{Now: now, Window: ParseReleaseDate("2026-10-18", now), Review: review, PopularNew: true, Wishlisted: true}
		before := input
		got := EvaluateEligibility(input, DefaultDiscoveryConfig())
		if got.Active || got.Reason != "review_unknown" || !reflect.DeepEqual(before, input) {
			t.Fatalf("unknown review admitted or mutated: %#v", got)
		}
		input.ComingSoon = true
		if got := EvaluateEligibility(input, DefaultDiscoveryConfig()); !got.Active {
			t.Fatalf("unknown reviews blocked upcoming: %#v", got)
		}
	}
}

func TestReleaseWindowEligibilityInvalidInput(t *testing.T) {
	now := eligibilityTime("2026-10-18T12:00:00Z")
	for _, window := range []ReleaseWindow{
		{Precision: PrecisionDay}, {Precision: PrecisionMonth, Year: 2026, Month: 13},
		{Precision: PrecisionQuarter, Year: 2026, Quarter: 5}, {Precision: PrecisionYear},
		{Precision: "unexpected"},
	} {
		got := EvaluateEligibility(EligibilityInput{Now: now, Window: window, ComingSoon: true, Wishlisted: true}, DefaultDiscoveryConfig())
		if got.Active || got.Reason != "malformed" || got.NextChangeAt != nil {
			t.Fatalf("invalid window admitted: %#v", got)
		}
	}
	cfg := DefaultDiscoveryConfig()
	cfg.LookbackDays = 0
	if got := EvaluateEligibility(EligibilityInput{Now: now, Window: ParseReleaseDate("2026-10-18", now), ComingSoon: true}, cfg); got.Active || got.Reason != "malformed" {
		t.Fatalf("invalid config admitted: %#v", got)
	}
}

func TestReleaseWindowEligibilityClockTransitions(t *testing.T) {
	now := eligibilityTime("2026-10-18T23:59:59Z")
	for _, tc := range []struct {
		raw           string
		soon, unrated bool
	}{
		{"2026-09-18", false, false},
		{"2026-10-18", true, false},
		{"2026-11-18", true, false},
		{"2026-10-19", false, false},
		{"September 2026", false, false},
		{"December 2026", true, false},
		{"Q4 2026", true, false},
		{"2026-10-11", false, true},
	} {
		input := EligibilityInput{Now: now, Window: ParseReleaseDate(tc.raw, now), ComingSoon: tc.soon, Review: eligibilityReview("Positive"), PopularNew: true}
		if tc.unrated {
			input.Review = eligibilityReview("No user reviews")
		}
		decision := EvaluateEligibility(input, DefaultDiscoveryConfig())
		if decision.NextChangeAt == nil || !decision.NextChangeAt.After(now) {
			t.Fatalf("missing future clock boundary: %#v", decision)
		}
		input.Now = decision.NextChangeAt.Add(-time.Nanosecond)
		before := EvaluateEligibility(input, DefaultDiscoveryConfig())
		input.Now = *decision.NextChangeAt
		after := EvaluateEligibility(input, DefaultDiscoveryConfig())
		if before.Active != decision.Active || after.Active == decision.Active {
			t.Fatalf("%s: boundary did not change eligibility: initial=%#v before=%#v after=%#v", tc.raw, decision, before, after)
		}
	}
}

func TestReleaseWindowEligibilityConfiguredWindows(t *testing.T) {
	now := eligibilityTime("2026-10-18T12:00:00Z")
	cfg := DefaultDiscoveryConfig()
	cfg.LookbackDays, cfg.HorizonDays = 3, 7
	for _, tc := range []struct {
		raw          string
		soon, active bool
	}{
		{"2026-10-15", false, true}, {"2026-10-14", false, false},
		{"2026-10-25", true, true}, {"2026-10-26", true, false},
	} {
		input := EligibilityInput{Now: now, Window: ParseReleaseDate(tc.raw, now), ComingSoon: tc.soon, Review: eligibilityReview("Positive")}
		if got := EvaluateEligibility(input, cfg); got.Active != tc.active {
			t.Fatalf("configured window for %s = %#v", tc.raw, got)
		}
	}
	got := EvaluateEligibility(EligibilityInput{Now: now, Window: ParseReleaseDate("2026-10-18", now), Review: eligibilityReview("No user reviews"), PopularNew: true}, cfg)
	if !got.Active {
		t.Fatalf("unrated inside short lookback is inactive: %#v", got)
	}
	assertEligibilityNext(t, got.NextChangeAt, "2026-10-22T00:00:00Z")
}

func assertEligibilityNext(t *testing.T, got *time.Time, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("next change = %s, want nil", got)
		}
		return
	}
	expected := eligibilityTime(want)
	if got == nil || !got.Equal(expected) || got.Location() != time.UTC {
		t.Fatalf("next change = %v, want %s UTC", got, expected)
	}
}
