package catalog

import (
	"time"

	"next-game-crush/internal/steam"
)

type EligibilityInput struct {
	Now             time.Time
	Window          ReleaseWindow
	ComingSoon      bool
	Review          steam.ReviewSummary
	PopularUpcoming bool
	PopularNew      bool
	Wishlisted      bool
}

type EligibilityDecision struct {
	Active       bool
	Reason       string
	Phase        string
	NextChangeAt *time.Time
}

// EvaluateEligibility evaluates canonical Store phase and normalized reviews.
// Feed/wishlist flags must describe current authoritative membership. Unknown
// reviews fail closed; callers retain and supply any prior successful snapshot.
// The legacy Qualify path deliberately remains separate during rollout.
func EvaluateEligibility(input EligibilityInput, cfg DiscoveryConfig) EligibilityDecision {
	decision := EligibilityDecision{Phase: "released", Reason: "malformed"}
	if input.ComingSoon {
		decision.Phase = "upcoming"
	}
	if cfg.Validate() != nil || input.Now.IsZero() {
		return decision
	}
	today := utcCalendarDay(input.Now)
	start, end, knownInterval := input.Window.Interval()
	switch input.Window.Precision {
	case PrecisionDay, PrecisionMonth, PrecisionQuarter, PrecisionYear:
		if !knownInterval {
			return decision
		}
	case PrecisionComingSoon, PrecisionUnknown:
	default:
		return decision
	}

	review := steam.ReviewUnknown
	if !input.ComingSoon {
		review = eligibilityReviewBand(input.Review)
		switch review {
		case steam.ReviewMixedOrBelow:
			decision.Reason = "mixed_or_below"
			return decision
		case steam.ReviewUnknown:
			decision.Reason = "review_unknown"
			return decision
		}
	}

	decision.Reason = "outside_window"
	switch input.Window.Precision {
	case PrecisionDay, PrecisionMonth, PrecisionQuarter:
		if input.ComingSoon {
			if end.Before(today) {
				return decision
			}
			if start.After(today.AddDate(0, 0, cfg.HorizonDays)) {
				decision.NextChangeAt = eligibilityBoundary(start.AddDate(0, 0, -cfg.HorizonDays))
				return decision
			}
			decision.NextChangeAt = eligibilityBoundary(end.AddDate(0, 0, 1))
		} else {
			if end.Before(today.AddDate(0, 0, -cfg.LookbackDays)) {
				return decision
			}
			if start.After(today) {
				decision.NextChangeAt = eligibilityBoundary(start)
				return decision
			}
			decision.NextChangeAt = eligibilityBoundary(end.AddDate(0, 0, cfg.LookbackDays+1))
		}
	case PrecisionYear, PrecisionComingSoon, PrecisionUnknown:
		if !input.PopularUpcoming && !input.Wishlisted {
			return decision
		}
	}

	if !input.ComingSoon && review == steam.ReviewUnrated {
		decision.NextChangeAt = nil
		if input.Window.Precision != PrecisionDay || today.After(start.AddDate(0, 0, 7)) {
			decision.Reason = "unrated_grace_expired"
			return decision
		}
		if !input.PopularNew {
			decision.Reason = "unrated_not_popular_new"
			return decision
		}
		// Both the grace and configured lookback can expire; wake at the first.
		expiry := start.AddDate(0, 0, 8)
		windowExpiry := end.AddDate(0, 0, cfg.LookbackDays+1)
		if windowExpiry.Before(expiry) {
			expiry = windowExpiry
		}
		decision.NextChangeAt = eligibilityBoundary(expiry)
	}
	decision.Active, decision.Reason = true, "admitted"
	return decision
}

func eligibilityBoundary(value time.Time) *time.Time {
	return &value
}

// Defend the pure policy boundary as well as the adapter boundary: a forged or
// inconsistent positive/unrated band cannot admit a released game.
func eligibilityReviewBand(review steam.ReviewSummary) steam.ReviewBand {
	if review.Total < 0 || int64(int(review.Total)) != review.Total {
		return steam.ReviewUnknown
	}
	total := int(review.Total)
	normalized := steam.NormalizeReview(review.Label, review.PositivePercent, &total)
	if normalized.Band != review.Band {
		return steam.ReviewUnknown
	}
	return normalized.Band
}
