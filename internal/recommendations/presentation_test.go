package recommendations

import "testing"

func TestSteamPresentationFromStoredSnapshot(t *testing.T) {
	t.Parallel()

	cover, store := steamPresentation(620, []byte(`{"HeaderImage":"https://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg"}`))
	if cover != "https://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg" {
		t.Fatalf("cover = %q", cover)
	}
	if store != "https://store.steampowered.com/app/620/" {
		t.Fatalf("store = %q", store)
	}
}

func TestSteamPresentationRejectsUnsafeOrMalformedCover(t *testing.T) {
	t.Parallel()

	for _, snapshot := range [][]byte{
		nil,
		[]byte(`not-json`),
		[]byte(`{"HeaderImage":"http://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg"}`),
		[]byte(`{"HeaderImage":"https://steamstatic.com.evil.example/header.jpg"}`),
	} {
		cover, store := steamPresentation(620, snapshot)
		if cover != "" {
			t.Fatalf("snapshot %q cover = %q", snapshot, cover)
		}
		if store != "https://store.steampowered.com/app/620/" {
			t.Fatalf("snapshot %q store = %q", snapshot, store)
		}
	}
}

func TestSteamPresentationRejectsInvalidAppID(t *testing.T) {
	t.Parallel()
	cover, store := steamPresentation(0, []byte(`{"HeaderImage":"https://steamstatic.com/header.jpg"}`))
	if cover != "" || store != "" {
		t.Fatalf("cover=%q store=%q", cover, store)
	}
}
