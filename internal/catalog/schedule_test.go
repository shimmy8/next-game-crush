package catalog

import (
	"strings"
	"testing"
	"time"
)

// Also run this test from its cross-compiled binary in the Alpine runtime image,
// where neither system zoneinfo nor the Go distribution's zoneinfo.zip exists.
func TestParseScheduleEmbeddedTimezone(t *testing.T) {
	t.Setenv("ZONEINFO", t.TempDir())
	cfg := DefaultDiscoveryConfig()
	location, schedule, err := ParseSchedule(cfg.RefreshCron, cfg.RefreshTimezone)
	if err != nil {
		t.Fatalf("default schedule requires an embedded timezone database: %v", err)
	}
	from := time.Date(2026, 10, 18, 23, 59, 0, 0, time.UTC)
	want := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if location.String() != "Europe/Moscow" || !schedule.Next(from).Equal(want) {
		t.Fatal("embedded default timezone did not preserve the Moscow schedule")
	}
}

func TestParseScheduleUsesConfiguredTimezone(t *testing.T) {
	for _, tc := range []struct {
		expression, timezone, from, want string
	}{
		{"0 3 * * *", "Europe/Moscow", "2026-10-18T23:59:00Z", "2026-10-19T00:00:00Z"},
		{"0 3 * * *", "UTC", "2026-10-18T23:59:00Z", "2026-10-19T03:00:00Z"},
		{"0 4 * * 0", "Europe/Moscow", "2026-10-18T00:30:00Z", "2026-10-18T01:00:00Z"},
		{"0 3 * * *", "America/New_York", "2026-11-01T00:00:00Z", "2026-11-01T08:00:00Z"},
	} {
		t.Run(tc.timezone+tc.expression, func(t *testing.T) {
			location, schedule, err := ParseSchedule(tc.expression, tc.timezone)
			if err != nil {
				t.Fatal(err)
			}
			from, _ := time.Parse(time.RFC3339, tc.from)
			want, _ := time.Parse(time.RFC3339, tc.want)
			if location.String() != tc.timezone || !schedule.Next(from).Equal(want) {
				t.Fatalf("timezone=%s next=%s want=%s", location, schedule.Next(from), want)
			}
		})
	}
	for _, tc := range []struct{ expression, timezone string }{
		{"private-invalid-expression", "UTC"}, {"0 0 3 * * *", "UTC"}, {"@daily", "UTC"},
		{"CRON_TZ=UTC 0 3 * * *", "UTC"}, {"0 3 * * *", "private-invalid-timezone"},
		{"0 3 * * *", ""},
	} {
		if _, _, err := ParseSchedule(tc.expression, tc.timezone); err == nil {
			t.Fatal("accepted invalid schedule")
		} else if strings.Contains(err.Error(), "private-invalid") {
			t.Fatal("schedule error exposed input")
		}
	}
}
