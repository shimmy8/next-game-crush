package catalog

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Precision string

const (
	PrecisionDay        Precision = "day"
	PrecisionMonth      Precision = "month"
	PrecisionQuarter    Precision = "quarter"
	PrecisionYear       Precision = "year"
	PrecisionComingSoon Precision = "coming_soon"
	PrecisionUnknown    Precision = "unknown"
)

type ReleaseWindow struct {
	Precision Precision  `json:"precision"`
	Date      *time.Time `json:"date,omitempty"`
	Year      int        `json:"year,omitempty"`
	Month     time.Month `json:"month,omitempty"`
	Quarter   int        `json:"quarter,omitempty"`
	Raw       string     `json:"raw"`
}

// Interval returns inclusive UTC calendar-day bounds for a known precision.
// These transient bounds never replace the stored release precision or Date.
func (w ReleaseWindow) Interval() (start, end time.Time, ok bool) {
	switch w.Precision {
	case PrecisionDay:
		if w.Date == nil || w.Date.UTC().Year() < 1 || w.Date.UTC().Year() > 9999 {
			return start, end, false
		}
		start = utcCalendarDay(*w.Date)
		return start, start, true
	case PrecisionMonth, PrecisionQuarter, PrecisionYear:
		if w.Year < 1 || w.Year > 9999 {
			return start, end, false
		}
	default:
		return start, end, false
	}
	switch w.Precision {
	case PrecisionMonth:
		if w.Month < time.January || w.Month > time.December {
			return start, end, false
		}
		start = time.Date(w.Year, w.Month, 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, -1)
	case PrecisionQuarter:
		if w.Quarter < 1 || w.Quarter > 4 {
			return start, end, false
		}
		start = time.Date(w.Year, time.Month((w.Quarter-1)*3+1), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 3, -1)
	case PrecisionYear:
		start = time.Date(w.Year, time.January, 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(1, 0, -1)
	}
	return start, end, true
}

func utcCalendarDay(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

var (
	quarterPattern = regexp.MustCompile(`(?i)^Q([1-4])\s*[,/-]?\s*(\d{4})$`)
	yearPattern    = regexp.MustCompile(`^\d{4}$`)
)

func ParseReleaseDate(raw string, _ time.Time) ReleaseWindow {
	trimmed := strings.TrimSpace(raw)
	window := ReleaseWindow{Precision: PrecisionUnknown, Raw: trimmed}
	if trimmed == "" {
		return window
	}
	lower := strings.ToLower(trimmed)
	if lower == "coming soon" || lower == "coming soon!" {
		window.Precision = PrecisionComingSoon
		return window
	}
	if matches := quarterPattern.FindStringSubmatch(trimmed); matches != nil {
		window.Precision = PrecisionQuarter
		window.Quarter, _ = strconv.Atoi(matches[1])
		window.Year, _ = strconv.Atoi(matches[2])
		return window
	}
	if yearPattern.MatchString(trimmed) {
		window.Precision = PrecisionYear
		window.Year, _ = strconv.Atoi(trimmed)
		return window
	}
	for _, layout := range []string{"2006-01-02", "2 Jan, 2006", "2 January, 2006", "Jan 2, 2006", "January 2, 2006", "Jan 2 2006", "January 2 2006", "02 Jan 2006", "2 January 2006"} {
		if parsed, err := time.ParseInLocation(layout, trimmed, time.UTC); err == nil {
			parsed = parsed.UTC()
			window.Precision, window.Date = PrecisionDay, &parsed
			window.Year, window.Month = parsed.Year(), parsed.Month()
			return window
		}
	}
	for _, layout := range []string{"Jan 2006", "January 2006", "Jan, 2006", "January, 2006", "2006-01"} {
		if parsed, err := time.ParseInLocation(layout, trimmed, time.UTC); err == nil {
			window.Precision, window.Year, window.Month = PrecisionMonth, parsed.Year(), parsed.Month()
			return window
		}
	}
	return window
}
