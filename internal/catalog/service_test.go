package catalog

import "testing"

func TestLoadReviewedSeedContainsTwentyUniqueVersionedEntries(t *testing.T) {
	seed, err := LoadSeed("../../catalog_seed.json")
	if err != nil {
		t.Fatal(err)
	}
	if seed.Version != 1 || seed.ObservedDate != "2026-10-17" || len(seed.Entries) != 20 {
		t.Fatalf("seed header/count = version %d, observed %s, entries %d", seed.Version, seed.ObservedDate, len(seed.Entries))
	}
	seen := make(map[int64]bool, len(seed.Entries))
	for _, entry := range seed.Entries {
		if seen[entry.AppID] {
			t.Fatalf("duplicate seed AppID %d", entry.AppID)
		}
		seen[entry.AppID] = true
		if entry.SourceURL == "" || entry.ReleaseWindow == "" || entry.ObservedDate != seed.ObservedDate {
			t.Fatalf("seed AppID %d lacks source/window", entry.AppID)
		}
	}
}
