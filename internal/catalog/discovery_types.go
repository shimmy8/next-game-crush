package catalog

import (
	"context"
	"fmt"
	"time"

	"next-game-crush/internal/steam"
)

type DiscoveryPage = steam.DiscoveryPage
type DiscoveredApp = steam.DiscoveredApp
type DiscoverySource interface {
	ListChangedGames(context.Context, time.Time, uint32, int) (DiscoveryPage, error)
}
type DiscoveryMode string

const (
	DiscoveryIncremental DiscoveryMode = "incremental"
	DiscoveryBackfill    DiscoveryMode = "backfill"
)

type Revision struct {
	AppID        int64
	LastModified time.Time
}
type Observation struct {
	Revision
	Status          string
	Window          ReleaseWindow
	Attempts        int
	RetryGeneration int
	NextRetryAt     *time.Time
	SafeError       string
}
type ScanState struct {
	Source          string
	Mode            DiscoveryMode
	CursorAppID     uint32
	WatermarkAt     *time.Time
	ScanStartedAt   *time.Time
	ScanCompletedAt *time.Time
	Availability    string
	LastError       string
}
type DiscoveryConfig struct {
	LookbackDays           int
	AppDetailsHourlyBudget int
	AIHourlyBudget         int
	WorkerConcurrency      int
	SearchPageSize         int
	SearchMaxPagesPerFeed  int
	SearchMaxNewAppsPerRun int
	RefreshCron            string
	RefreshTimezone        string
	ReconciliationCron     string
	Enabled                bool
	HorizonDays            int
	// Retained while the legacy coordinator drains during the search rollout.
	IntervalHours        int
	HourlyBudget         int
	UnknownFreshnessDays int
	PageSize             int
}

func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{
		LookbackDays: 30, HorizonDays: 30, AppDetailsHourlyBudget: 1000, AIHourlyBudget: 100,
		WorkerConcurrency: 4, SearchPageSize: 100, SearchMaxPagesPerFeed: 100, SearchMaxNewAppsPerRun: 20000,
		RefreshCron: "0 3 * * *", RefreshTimezone: "Europe/Moscow", ReconciliationCron: "0 4 * * 0",
		IntervalHours: 1, HourlyBudget: 200, UnknownFreshnessDays: 180, PageSize: 100,
	}
}
func (c DiscoveryConfig) Validate() error {
	for _, setting := range []struct {
		name       string
		value, max int
	}{
		{"CATALOG_LOOKBACK_DAYS", c.LookbackDays, 366}, {"CATALOG_HORIZON_DAYS", c.HorizonDays, 366},
		{"CATALOG_APPDETAILS_HOURLY_BUDGET", c.AppDetailsHourlyBudget, 100000}, {"CATALOG_AI_HOURLY_BUDGET", c.AIHourlyBudget, 100000},
		{"CATALOG_WORKER_CONCURRENCY", c.WorkerConcurrency, 16}, {"CATALOG_SEARCH_PAGE_SIZE", c.SearchPageSize, 200},
		{"CATALOG_SEARCH_MAX_PAGES_PER_FEED", c.SearchMaxPagesPerFeed, 1000}, {"CATALOG_SEARCH_MAX_NEW_APPS_PER_RUN", c.SearchMaxNewAppsPerRun, 100000},
		{"CATALOG_DISCOVERY_INTERVAL_HOURS", c.IntervalHours, 24}, {"CATALOG_DISCOVERY_HOURLY_BUDGET", c.HourlyBudget, 1000},
		{"CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS", c.UnknownFreshnessDays, 730}, {"CATALOG_DISCOVERY_PAGE_SIZE", c.PageSize, 200},
	} {
		if setting.value < 1 || setting.value > setting.max {
			return fmt.Errorf("%s is outside supported bounds", setting.name)
		}
	}
	for _, setting := range []struct{ name, expression string }{
		{"CATALOG_REFRESH_CRON", c.RefreshCron}, {"CATALOG_RECONCILIATION_CRON", c.ReconciliationCron},
	} {
		if _, _, err := ParseSchedule(setting.expression, c.RefreshTimezone); err != nil {
			return fmt.Errorf("%s or CATALOG_REFRESH_TIMEZONE is invalid", setting.name)
		}
	}
	return nil
}
