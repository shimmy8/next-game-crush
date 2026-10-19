package catalog

import (
	"fmt"
)

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
	ReconciliationPageSize int
	Enabled                bool
	HorizonDays            int
}

func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{
		LookbackDays: 30, HorizonDays: 30, AppDetailsHourlyBudget: 1000, AIHourlyBudget: 100,
		WorkerConcurrency: 4, SearchPageSize: 100, SearchMaxPagesPerFeed: 100, SearchMaxNewAppsPerRun: 20000,
		RefreshCron: "0 3 * * *", RefreshTimezone: "Europe/Moscow", ReconciliationCron: "0 4 * * 0",
		ReconciliationPageSize: 100,
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
		{"CATALOG_RECONCILIATION_PAGE_SIZE", c.ReconciliationPageSize, 200},
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
