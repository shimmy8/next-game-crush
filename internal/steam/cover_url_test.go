package steam

import "testing"

func TestValidateCoverURL(t *testing.T) {
	t.Parallel()
	valid := []string{
		"https://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg",
		"https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/620/header.jpg",
		"https://steamstatic.com/steam/apps/620/header.jpg",
	}
	for _, raw := range valid {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateCoverURL(raw)
			if err != nil || got != raw {
				t.Fatalf("ValidateCoverURL(%q) = %q, %v", raw, got, err)
			}
		})
	}

	invalid := []string{
		"",
		"http://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg",
		"https://user@cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg",
		"https://cdn.cloudflare.steamstatic.com:443/steam/apps/620/header.jpg",
		"https://steamstatic.com.evil.example/steam/apps/620/header.jpg",
		"https://evil.example/steam/apps/620/header.jpg",
	}
	for _, raw := range invalid {
		raw := raw
		t.Run("reject_"+raw, func(t *testing.T) {
			t.Parallel()
			if got, err := ValidateCoverURL(raw); err == nil || got != "" {
				t.Fatalf("ValidateCoverURL(%q) = %q, %v", raw, got, err)
			}
		})
	}
}
