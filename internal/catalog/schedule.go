package catalog

import (
	"errors"
	"strings"
	"time"
	_ "time/tzdata" // The Alpine runtime does not install system timezone files.

	"github.com/robfig/cron/v3"
)

// ParseSchedule accepts a standard five-field cron expression in an IANA timezone.
// The timezone controls execution instants, never release-window calendar dates.
func ParseSchedule(expression, timezone string) (*time.Location, cron.Schedule, error) {
	if timezone == "" || timezone == "Local" {
		return nil, nil, errors.New("schedule timezone is invalid")
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, nil, errors.New("schedule timezone is invalid")
	}
	// Exclude cron descriptors, seconds, and inline timezone overrides.
	if len(strings.Fields(expression)) != 5 {
		return nil, nil, errors.New("schedule cron expression is invalid")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(expression)
	if err != nil {
		return nil, nil, errors.New("schedule cron expression is invalid")
	}
	schedule.(*cron.SpecSchedule).Location = location
	return location, schedule, nil
}
