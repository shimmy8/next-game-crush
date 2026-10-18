package config

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"next-game-crush/internal/catalog"
)

func resetDiscoveryEnvironment(t *testing.T) {
	t.Helper()
	setRequiredEnvironment(t)
	for _, name := range []string{
		"CATALOG_DISCOVERY_ENABLED", "CATALOG_LOOKBACK_DAYS", "CATALOG_HORIZON_DAYS",
		"CATALOG_APPDETAILS_HOURLY_BUDGET", "CATALOG_AI_HOURLY_BUDGET", "CATALOG_WORKER_CONCURRENCY",
		"CATALOG_SEARCH_PAGE_SIZE", "CATALOG_SEARCH_MAX_PAGES_PER_FEED", "CATALOG_SEARCH_MAX_NEW_APPS_PER_RUN",
		"CATALOG_REFRESH_CRON", "CATALOG_REFRESH_TIMEZONE", "CATALOG_RECONCILIATION_CRON",
		"CATALOG_DISCOVERY_INTERVAL_HOURS", "CATALOG_DISCOVERY_HOURLY_BUDGET",
		"CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS", "CATALOG_DISCOVERY_PAGE_SIZE", "STEAM_STORE_SERVICE_BASE_URL",
	} {
		t.Setenv(name, "")
	}
}

func TestDiscoveryConfigNewDefaults(t *testing.T) {
	resetDiscoveryEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := catalog.DiscoveryConfig{
		LookbackDays: 30, HorizonDays: 30, AppDetailsHourlyBudget: 1000, AIHourlyBudget: 100,
		WorkerConcurrency: 4, SearchPageSize: 100, SearchMaxPagesPerFeed: 100, SearchMaxNewAppsPerRun: 20000,
		RefreshCron: "0 3 * * *", RefreshTimezone: "Europe/Moscow", ReconciliationCron: "0 4 * * 0",
		IntervalHours: 1, HourlyBudget: 200, UnknownFreshnessDays: 180, PageSize: 100,
	}
	if !reflect.DeepEqual(cfg.CatalogDiscovery, want) || !reflect.DeepEqual(catalog.DefaultDiscoveryConfig(), want) {
		t.Fatal("discovery defaults differ from the scheduled release-window contract")
	}
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryConfigRejectsInvalidCronTimezoneAndBounds(t *testing.T) {
	for _, setting := range []struct {
		name string
		max  int
	}{
		{"CATALOG_LOOKBACK_DAYS", 366}, {"CATALOG_HORIZON_DAYS", 366},
		{"CATALOG_APPDETAILS_HOURLY_BUDGET", 100000}, {"CATALOG_AI_HOURLY_BUDGET", 100000},
		{"CATALOG_WORKER_CONCURRENCY", 16}, {"CATALOG_SEARCH_PAGE_SIZE", 200},
		{"CATALOG_SEARCH_MAX_PAGES_PER_FEED", 1000}, {"CATALOG_SEARCH_MAX_NEW_APPS_PER_RUN", 100000},
	} {
		t.Run(setting.name, func(t *testing.T) {
			resetDiscoveryEnvironment(t)
			for _, value := range []int{1, setting.max} {
				t.Setenv(setting.name, strconv.Itoa(value))
				if _, err := Load(); err != nil {
					t.Fatalf("valid boundary rejected: %v", err)
				}
			}
			for _, value := range []string{"0", "-1", strconv.Itoa(setting.max + 1), "private-invalid-number"} {
				t.Setenv(setting.name, value)
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), setting.name) || strings.Contains(err.Error(), "private-invalid") {
					t.Fatalf("expected sanitized named validation error, got %v", err)
				}
			}
		})
	}
	for _, name := range []string{"CATALOG_REFRESH_CRON", "CATALOG_RECONCILIATION_CRON", "CATALOG_REFRESH_TIMEZONE"} {
		t.Run(name, func(t *testing.T) {
			resetDiscoveryEnvironment(t)
			t.Setenv(name, "private-invalid-schedule")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "private-invalid") {
				t.Fatalf("expected sanitized named validation error, got %v", err)
			}
		})
	}
}

func TestNewDiscoverySettingsOverrideLegacyTransition(t *testing.T) {
	resetDiscoveryEnvironment(t)
	t.Setenv("CATALOG_DISCOVERY_HOURLY_BUDGET", "250")
	t.Setenv("CATALOG_DISCOVERY_PAGE_SIZE", "50")
	t.Setenv("CATALOG_DISCOVERY_INTERVAL_HOURS", "2")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogDiscovery.AppDetailsHourlyBudget != 250 || cfg.CatalogDiscovery.SearchPageSize != 50 {
		t.Fatal("legacy settings did not supply transition fallbacks")
	}
	t.Setenv("CATALOG_APPDETAILS_HOURLY_BUDGET", "1500")
	t.Setenv("CATALOG_SEARCH_PAGE_SIZE", "150")
	t.Setenv("CATALOG_REFRESH_CRON", "0 6 * * *")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogDiscovery.AppDetailsHourlyBudget != 1500 || cfg.CatalogDiscovery.SearchPageSize != 150 || cfg.CatalogDiscovery.RefreshCron != "0 6 * * *" {
		t.Fatal("new settings did not override legacy transition values")
	}
}

func TestDiscoveryConfigBoundsAndOverrideGuard(t *testing.T) {
	setRequiredEnvironment(t)
	for _, name := range []string{"CATALOG_DISCOVERY_ENABLED", "CATALOG_HORIZON_DAYS", "CATALOG_DISCOVERY_INTERVAL_HOURS", "CATALOG_DISCOVERY_HOURLY_BUDGET", "CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS", "CATALOG_DISCOVERY_PAGE_SIZE", "STEAM_STORE_SERVICE_BASE_URL"} {
		t.Setenv(name, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogDiscovery.Enabled || cfg.CatalogDiscovery.HorizonDays != 30 || cfg.CatalogDiscovery.HourlyBudget != 200 {
		t.Fatal("defaults changed")
	}
	for name, value := range map[string]string{"CATALOG_DISCOVERY_ENABLED": "yes", "CATALOG_HORIZON_DAYS": "367", "CATALOG_DISCOVERY_INTERVAL_HOURS": "0", "CATALOG_DISCOVERY_HOURLY_BUDGET": "1001", "CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS": "731", "CATALOG_DISCOVERY_PAGE_SIZE": "201"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(); err == nil {
				t.Fatal("accepted invalid discovery config")
			}
		})
	}
	t.Setenv("TEST_HARNESS_MODE", "0")
	t.Setenv("STEAM_STORE_SERVICE_BASE_URL", "http://localhost:1234")
	if _, err := Load(); err == nil {
		t.Fatal("accepted override outside harness")
	}
	t.Setenv("TEST_HARNESS_MODE", "1")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
