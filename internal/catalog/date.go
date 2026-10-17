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
