package catalog

import (
	"math"
	"strings"
	"time"

	"next-game-crush/internal/steam"
)

type Qualification struct {
	Status string
	Window ReleaseWindow
}

func Qualify(app DiscoveredApp, details steam.GameDetails, now time.Time, cfg DiscoveryConfig) Qualification {
	window := ParseReleaseDate(details.ReleaseDate.Raw, now)
	if details.ReleaseDate.ComingSoon && strings.TrimSpace(details.ReleaseDate.Raw) == "" {
		window.Precision = PrecisionComingSoon
	}
	result := Qualification{Status: "failed", Window: window}
	if cfg.Validate() != nil || app.AppID <= 0 || app.AppID > math.MaxUint32 || details.AppID != app.AppID || app.LastModified.IsZero() || app.LastModified.Unix() < 1 || app.LastModified.Unix() > math.MaxUint32 {
		return result
	}
	if !strings.EqualFold(strings.TrimSpace(details.Type), "game") {
		result.Status = "not_game"
		return result
	}
	if !details.ReleaseDate.ComingSoon {
		result.Status = "released"
		return result
	}
	result.Status = "outside_horizon"
	if window.Precision == PrecisionUnknown || window.Precision == PrecisionComingSoon {
		if !app.LastModified.Before(now.AddDate(0, 0, -cfg.UnknownFreshnessDays)) {
			result.Status = "admitted"
		}
	} else if IntersectsHorizon(window, now, cfg.HorizonDays) {
		result.Status = "admitted"
	}
	return result
}

// IntersectsHorizon computes transient coarse boundaries without inventing a date.
func IntersectsHorizon(window ReleaseWindow, now time.Time, days int) bool {
	if days < 1 || days > 366 {
		return false
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	var start, end time.Time
	switch window.Precision {
	case PrecisionDay:
		if window.Date == nil {
			return false
		}
		d := window.Date.UTC()
		start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		end = start
	case PrecisionMonth:
		if window.Year < 1 || window.Month < 1 || window.Month > 12 {
			return false
		}
		start = time.Date(window.Year, window.Month, 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, -1)
	case PrecisionQuarter:
		if window.Year < 1 || window.Quarter < 1 || window.Quarter > 4 {
			return false
		}
		start = time.Date(window.Year, time.Month((window.Quarter-1)*3+1), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 3, -1)
	case PrecisionYear:
		if window.Year < 1 {
			return false
		}
		start = time.Date(window.Year, 1, 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(1, 0, -1)
	default:
		return false
	}
	return !end.Before(today) && !start.After(today.AddDate(0, 0, days))
}
