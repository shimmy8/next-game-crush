package config

import "testing"

func TestDiscoveryConfigBoundsAndOverrideGuard(t *testing.T) {
	setRequiredEnvironment(t)
	for _, name := range []string{"CATALOG_DISCOVERY_ENABLED", "CATALOG_HORIZON_DAYS", "CATALOG_DISCOVERY_INTERVAL_HOURS", "CATALOG_DISCOVERY_HOURLY_BUDGET", "CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS", "CATALOG_DISCOVERY_PAGE_SIZE", "STEAM_STORE_SERVICE_BASE_URL"} {
		t.Setenv(name, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogDiscovery.Enabled || cfg.CatalogDiscovery.HorizonDays != 90 || cfg.CatalogDiscovery.HourlyBudget != 200 {
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
