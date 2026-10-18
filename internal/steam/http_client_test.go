package steam

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testAPIKey = "fixture-secret-api-key"

func TestPlayerSummaryContracts(t *testing.T) {
	const steamID = "76561197960287930"
	t.Run("public matching player", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ISteamUser/GetPlayerSummaries/v2/" || r.URL.Query().Get("key") != testAPIKey || r.URL.Query().Get("steamids") != steamID {
				t.Fatalf("request URL = %s", r.URL.String())
			}
			serveFixture(t, w, "player_summary_public.json")
		}), HTTPConfig{})
		summary, err := client.PlayerSummary(context.Background(), steamID)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Visibility != VisibilityPublic || summary.DisplayName != "Player" || summary.AvatarURL != "https://avatars.cloudflare.steamstatic.com/avatar.jpg" {
			t.Fatalf("summary = %#v", summary)
		}
	})

	t.Run("private matching player", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serveFixture(t, w, "player_summary_private.json")
		}), HTTPConfig{})
		summary, err := client.PlayerSummary(context.Background(), steamID)
		if err != nil || summary.Visibility != VisibilityPrivate {
			t.Fatalf("summary = %#v, err = %v", summary, err)
		}
	})

	t.Run("empty avatar is nullable identity", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"response":{"players":[{"steamid":"76561197960287930","communityvisibilitystate":3,"personaname":"Player","avatarfull":""}]}}`))
		}), HTTPConfig{})
		summary, err := client.PlayerSummary(context.Background(), steamID)
		if err != nil || summary.Visibility != VisibilityPublic || summary.DisplayName != "Player" || summary.AvatarURL != "" {
			t.Fatalf("summary = %#v, err = %v", summary, err)
		}
	})

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "missing player", body: `{"response":{"players":[]}}`},
		{name: "mismatched player", body: `{"response":{"players":[{"steamid":"76561197960287931","communityvisibilitystate":3,"personaname":"Other","avatarfull":"https://avatars.cloudflare.steamstatic.com/avatar.jpg"}]}}`},
		{name: "duplicate matching player", body: `{"response":{"players":[{"steamid":"76561197960287930","communityvisibilitystate":3,"personaname":"Player","avatarfull":"https://avatars.cloudflare.steamstatic.com/a.jpg"},{"steamid":"76561197960287930","communityvisibilitystate":3,"personaname":"Player","avatarfull":"https://avatars.cloudflare.steamstatic.com/b.jpg"}]}}`},
		{name: "unknown visibility", body: `{"response":{"players":[{"steamid":"76561197960287930","communityvisibilitystate":2,"personaname":"Player","avatarfull":"https://avatars.cloudflare.steamstatic.com/avatar.jpg"}]}}`},
		{name: "malformed JSON", body: `{"response":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.body)) }), HTTPConfig{})
			if _, err := client.PlayerSummary(context.Background(), steamID); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateAvatarURL(t *testing.T) {
	for _, host := range []string{"avatars.steamstatic.com", "avatars.cloudflare.steamstatic.com", "avatars.fastly.steamstatic.com", "avatars.akamai.steamstatic.com"} {
		raw := "https://" + host + "/avatar.jpg"
		if got, err := ValidateAvatarURL(raw); err != nil || got != raw {
			t.Fatalf("ValidateAvatarURL(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"http://avatars.cloudflare.steamstatic.com/avatar.jpg",
		"https://user@avatars.cloudflare.steamstatic.com/avatar.jpg",
		"https://avatars.cloudflare.steamstatic.com:443/avatar.jpg",
		"https://avatars.cloudflare.steamstatic.com.evil.example/avatar.jpg",
		"https://evil.example/avatar.jpg",
	} {
		if got, err := ValidateAvatarURL(raw); err == nil || got != "" {
			t.Fatalf("ValidateAvatarURL(%q) = %q, %v", raw, got, err)
		}
	}
}

func TestOwnedGamesPublicAndPrivateContracts(t *testing.T) {
	t.Run("public", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/IPlayerService/GetOwnedGames/v1/" {
				t.Fatalf("path = %q", r.URL.Path)
			}
			if got := r.URL.Query().Get("key"); got != testAPIKey {
				t.Fatalf("key = %q", got)
			}
			if got := r.URL.Query().Get("steamid"); got != "76561197960287930" {
				t.Fatalf("steamid = %q", got)
			}
			w.Header().Set("ETag", `"owned-v1"`)
			serveFixture(t, w, "owned_games.json")
		}), HTTPConfig{})

		games, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
		if err != nil {
			t.Fatal(err)
		}
		if availability != Available || len(games) != 2 {
			t.Fatalf("availability = %q, games = %#v", availability, games)
		}
		if games[0].AppID != 620 || games[0].Name != "Portal 2" || games[0].PlaytimeForever != 1234 || games[0].PlaytimeWindows != 1200 {
			t.Fatalf("first game = %#v", games[0])
		}
		metadata, ok := client.CacheMetadata(OwnedGamesCacheKey("76561197960287930"))
		if !ok || metadata.ETag != `"owned-v1"` || metadata.FetchedAt.IsZero() {
			t.Fatalf("cache metadata = %#v, ok = %v", metadata, ok)
		}
	})

	t.Run("private response is unavailable", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serveFixture(t, w, "private_profile.json")
		}), HTTPConfig{})
		games, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
		if err != nil || availability != Unavailable || games != nil {
			t.Fatalf("games = %#v, availability = %q, err = %v", games, availability, err)
		}
	})

	t.Run("explicitly empty library is available", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"response":{"game_count":0,"games":[]}}`))
		}), HTTPConfig{})
		games, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
		if err != nil || availability != Available || games == nil || len(games) != 0 {
			t.Fatalf("games = %#v, availability = %q, err = %v", games, availability, err)
		}
	})
}

func TestProbeOwnedGamesDoesNotPublishOrUseCacheValidators(t *testing.T) {
	const steamID = "76561197960287930"
	var calls atomic.Int32
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Fatalf("call %d If-None-Match = %q, want empty", call, got)
		}
		if got := r.Header.Get("If-Modified-Since"); got != "" {
			t.Fatalf("call %d If-Modified-Since = %q, want empty", call, got)
		}
		w.Header().Set("ETag", `"owned-v1"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		serveFixture(t, w, "owned_games.json")
	}), HTTPConfig{})

	probed, availability, err := client.ProbeOwnedGames(context.Background(), steamID)
	if err != nil || availability != Available || len(probed) != 2 {
		t.Fatalf("probe games = %#v, availability = %q, err = %v", probed, availability, err)
	}
	if metadata, ok := client.CacheMetadata(OwnedGamesCacheKey(steamID)); ok {
		t.Fatalf("probe published cache metadata = %#v", metadata)
	}

	imported, availability, err := client.OwnedGames(context.Background(), steamID)
	if err != nil || availability != Available || len(imported) != 2 {
		t.Fatalf("import games = %#v, availability = %q, err = %v", imported, availability, err)
	}
	metadata, ok := client.CacheMetadata(OwnedGamesCacheKey(steamID))
	if !ok || metadata.ETag != `"owned-v1"` || metadata.LastModified != "Wed, 21 Oct 2015 07:28:00 GMT" {
		t.Fatalf("import cache metadata = %#v, ok = %v", metadata, ok)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestRecentlyPlayedContracts(t *testing.T) {
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/IPlayerService/GetRecentlyPlayedGames/v1/" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		serveFixture(t, w, "recent_games.json")
	}), HTTPConfig{})

	games, availability, err := client.RecentlyPlayed(context.Background(), "76561197960287930")
	if err != nil || availability != Available || len(games) != 1 {
		t.Fatalf("games = %#v, availability = %q, err = %v", games, availability, err)
	}
	if games[0].AppID != 440 || games[0].PlaytimeTwoWeeks != 42 || games[0].LastPlayedAt.Unix() != 1_700_000_000 {
		t.Fatalf("game = %#v", games[0])
	}
}

func TestWishlistContracts(t *testing.T) {
	t.Run("public", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/IWishlistService/GetWishlist/v1/" {
				t.Fatalf("path = %q", r.URL.Path)
			}
			w.Header().Set("X-Eresult", "1")
			serveFixture(t, w, "wishlist.json")
		}), HTTPConfig{})
		items, availability, err := client.Wishlist(context.Background(), "76561197960287930")
		if err != nil || availability != Available || len(items) != 2 {
			t.Fatalf("items = %#v, availability = %q, err = %v", items, availability, err)
		}
		if items[0].AppID != 100 || items[0].Priority != 2 || items[0].AddedAt.Unix() != 1_700_000_000 {
			t.Fatalf("first item = %#v", items[0])
		}
	})

	t.Run("explicitly empty", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Eresult", "1")
			_, _ = w.Write([]byte(`{"response":{"items":[]}}`))
		}), HTTPConfig{})
		items, availability, err := client.Wishlist(context.Background(), "76561197960287930")
		if err != nil || availability != Available || items == nil || len(items) != 0 {
			t.Fatalf("items = %#v, availability = %q, err = %v", items, availability, err)
		}
	})

	for _, test := range []struct {
		name   string
		header string
		body   string
	}{
		{name: "access denied result", header: "15", body: `{"response":{}}`},
		{name: "missing items", header: "1", body: `{"response":{}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Eresult", test.header)
				_, _ = w.Write([]byte(test.body))
			}), HTTPConfig{})
			items, availability, err := client.Wishlist(context.Background(), "76561197960287930")
			if err != nil || availability != Unavailable || items != nil {
				t.Fatalf("items = %#v, availability = %q, err = %v", items, availability, err)
			}
		})
	}
}

func TestAchievementContracts(t *testing.T) {
	t.Run("available localized metadata", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("appid") != "620" || r.URL.Query().Get("l") != "en" {
				t.Fatalf("request URL = %s", r.URL.String())
			}
			serveFixture(t, w, "achievements.json")
		}), HTTPConfig{})
		summary, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
		if err != nil || availability != Available {
			t.Fatalf("summary = %#v, availability = %q, err = %v", summary, availability, err)
		}
		if summary.AppID != 620 || summary.Total != 3 || summary.Unlocked != 2 || summary.Percentage != 2.0/3.0 {
			t.Fatalf("summary = %#v", summary)
		}
		want := []Achievement{
			{APIName: "ACH.ONE", Name: "Wake Up", Description: "Finish the first test chamber.", Achieved: true, UnlockedAt: time.Unix(1600000000, 0).UTC()},
			{APIName: "ACH.TWO", Name: "Still Alive", Description: "Reach an ending.", Achieved: false},
			{APIName: "ACH.THREE", Name: "Partygoer", Description: "Complete the game.", Achieved: true, UnlockedAt: time.Unix(1650000000, 0).UTC()},
		}
		if fmt.Sprint(summary.Achievements) != fmt.Sprint(want) {
			t.Fatalf("achievements = %#v, want %#v", summary.Achievements, want)
		}
	})

	t.Run("missing stats", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"playerstats":{"steamID":"76561197960287930","gameName":"No Stats","success":false,"error":"Requested app has no stats"}}`))
		}), HTTPConfig{})
		summary, availability, err := client.Achievements(context.Background(), "76561197960287930", 10)
		if err != nil || availability != Unavailable || summary.AppID != 0 || len(summary.Achievements) != 0 {
			t.Fatalf("summary = %#v, availability = %q, err = %v", summary, availability, err)
		}
	})

	t.Run("private does not become zero", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serveFixture(t, w, "private_achievements.json")
		}), HTTPConfig{})
		summary, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
		if err != nil || availability != Unavailable || summary.AppID != 0 || len(summary.Achievements) != 0 {
			t.Fatalf("summary = %#v, availability = %q, err = %v", summary, availability, err)
		}
	})
}

func TestAchievementSchemaContracts(t *testing.T) {
	t.Run("available app scoped metadata", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ISteamUserStats/GetSchemaForGame/v2/" || r.URL.Query().Get("appid") != "620" || r.URL.Query().Get("l") != "en" {
				t.Fatalf("request URL = %s", r.URL.String())
			}
			w.Header().Set("ETag", `"schema-v1"`)
			serveFixture(t, w, "achievement_schema.json")
		}), HTTPConfig{})
		got, availability, err := client.AchievementSchema(context.Background(), 620)
		if err != nil || availability != Available {
			t.Fatalf("schema = %#v, availability = %q, err = %v", got, availability, err)
		}
		want := AchievementSchema{AppID: 620, Achievements: []AchievementDefinition{
			{APIName: "ACH.ONE", Name: "Wake Up", Description: "Finish the first test chamber."},
			{APIName: "ACH.TWO", Name: "Still Alive", Description: "Reach an ending."},
		}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("schema = %#v, want %#v", got, want)
		}
		metadata, ok := client.CacheMetadata(AchievementSchemaCacheKey(620))
		if !ok || metadata.ETag != `"schema-v1"` {
			t.Fatalf("cache metadata = %#v, present = %v", metadata, ok)
		}
		if _, ok := client.CacheMetadata(AchievementSchemaCacheKey(440)); ok {
			t.Fatal("schema metadata leaked between games")
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"game":{"gameName":"Portal 2"}}`))
		}), HTTPConfig{})
		got, availability, err := client.AchievementSchema(context.Background(), 620)
		if err != nil || availability != Unavailable || got.AppID != 0 || len(got.Achievements) != 0 {
			t.Fatalf("schema = %#v, availability = %q, err = %v", got, availability, err)
		}
	})

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "blank api name", body: `{"game":{"availableGameStats":{"achievements":[{"name":" ","displayName":"Blank","description":"Bad"}]}}}`},
		{name: "duplicate api name", body: `{"game":{"availableGameStats":{"achievements":[{"name":"ACH.ONE","displayName":"One"},{"name":"ACH.ONE","displayName":"Two"}]}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("ETag", `"invalid"`)
				_, _ = w.Write([]byte(test.body))
			}), HTTPConfig{})
			got, availability, err := client.AchievementSchema(context.Background(), 620)
			if got.AppID != 0 || len(got.Achievements) != 0 || availability != RequestFailed || !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("schema = %#v, availability = %q, err = %v", got, availability, err)
			}
			if _, ok := client.CacheMetadata(AchievementSchemaCacheKey(620)); ok {
				t.Fatal("invalid schema published cache metadata")
			}
		})
	}
}

func TestGameDetailsContractAndMalformedReleaseDate(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/appdetails" || r.URL.Query().Get("appids") != "620" {
				t.Fatalf("request URL = %s", r.URL.String())
			}
			serveFixture(t, w, "store_app.json")
		}), HTTPConfig{})
		details, err := client.GameDetails(context.Background(), 620)
		if err != nil {
			t.Fatal(err)
		}
		if details.AppID != 620 || details.Name != "Portal 2" || details.HeaderImage != "https://cdn.example/620.jpg" || !details.ReleaseDate.ComingSoon || details.ReleaseDate.Raw != "Coming soon-ish" {
			t.Fatalf("details = %#v", details)
		}
		if len(details.Categories) != 1 || len(details.Genres) != 1 {
			t.Fatalf("details categories/genres = %#v/%#v", details.Categories, details.Genres)
		}
	})

	t.Run("malformed date remains raw", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serveFixture(t, w, "store_app_malformed_date.json")
		}), HTTPConfig{})
		details, err := client.GameDetails(context.Background(), 620)
		if err != nil {
			t.Fatal(err)
		}
		if details.ReleaseDate.Raw != "32 Smarch 20XX" || details.ReleaseDate.ComingSoon {
			t.Fatalf("release date = %#v", details.ReleaseDate)
		}
	})
}

func TestHTTPFailuresAreTypedRetryableAndSanitized(t *testing.T) {
	t.Run("429 exposes Retry-After", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			serveFixture(t, w, "http_error.json")
		}), HTTPConfig{MaxRetries: -1})
		_, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
		var requestErr *RequestError
		if availability != RequestFailed || !errors.As(err, &requestErr) || requestErr.StatusCode != http.StatusTooManyRequests || requestErr.RetryAfter != 7*time.Second || !requestErr.Temporary {
			t.Fatalf("availability = %q, err = %#v", availability, err)
		}
		if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), "sanitized upstream fixture") {
			t.Fatalf("error leaks secrets: %v", err)
		}
	})

	t.Run("safe GET retries temporary response", func(t *testing.T) {
		var calls atomic.Int32
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			serveFixture(t, w, "recent_games.json")
		}), HTTPConfig{MaxRetries: 1, RetryBase: time.Millisecond})
		games, availability, err := client.RecentlyPlayed(context.Background(), "76561197960287930")
		if err != nil || availability != Available || len(games) != 1 || calls.Load() != 2 {
			t.Fatalf("calls = %d, availability = %q, err = %v", calls.Load(), availability, err)
		}
	})

	t.Run("malformed JSON is request failure", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serveFixture(t, w, "malformed_response.json")
		}), HTTPConfig{})
		_, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
		if availability != RequestFailed || !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("availability = %q, err = %v", availability, err)
		}
	})

	t.Run("response body is limited", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 65)))
		}), HTTPConfig{MaxResponseBytes: 64})
		_, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
		if availability != RequestFailed || !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("availability = %q, err = %v", availability, err)
		}
	})

	t.Run("request timeout", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}), HTTPConfig{RequestTimeout: 10 * time.Millisecond, MaxRetries: -1})
		_, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
		if availability != RequestFailed || err == nil || !strings.Contains(err.Error(), "owned games request failed") {
			t.Fatalf("availability = %q, err = %v", availability, err)
		}
		if strings.Contains(err.Error(), testAPIKey) {
			t.Fatalf("error leaks API key: %v", err)
		}
	})
}

func TestRequestParametersAreEncodedNotInjected(t *testing.T) {
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("steamid"); got != "123&key=attacker" {
			t.Fatalf("steamid = %q", got)
		}
		if got := r.URL.Query().Get("key"); got != testAPIKey {
			t.Fatalf("key = %q", got)
		}
		_, _ = w.Write([]byte(`{"response":{"game_count":0,"games":[]}}`))
	}), HTTPConfig{})
	_, _, err := client.OwnedGames(context.Background(), "123&key=attacker")
	if err != nil {
		t.Fatal(err)
	}
}

func TestConditionalCacheValidator(t *testing.T) {
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"owned-v1"` || r.Header.Get("If-Modified-Since") != "Wed, 21 Oct 2015 07:28:00 GMT" {
			t.Fatalf("conditional headers = %#v", r.Header)
		}
		w.WriteHeader(http.StatusNotModified)
	}), HTTPConfig{CacheValidators: map[CacheKey]CacheValidator{
		OwnedGamesCacheKey("76561197960287930"): {ETag: `"owned-v1"`, LastModified: "Wed, 21 Oct 2015 07:28:00 GMT"},
	}})
	_, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
	if availability != NotModified || !errors.Is(err, ErrNotModified) {
		t.Fatalf("availability = %q, err = %v", availability, err)
	}
	metadata, ok := client.CacheMetadata(OwnedGamesCacheKey("76561197960287930"))
	if !ok || metadata.ETag != `"owned-v1"` || metadata.LastModified != "Wed, 21 Oct 2015 07:28:00 GMT" || metadata.FetchedAt.IsZero() {
		t.Fatalf("merged 304 metadata = %#v, ok = %v", metadata, ok)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2015, time.October, 21, 7, 27, 0, 0, time.UTC)
	if got := parseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT", now); got != time.Minute {
		t.Fatalf("Retry-After duration = %s", got)
	}
}

func TestCacheMetadataIsResourceScopedAndOnlyPublishedAfterValidation(t *testing.T) {
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appID := r.URL.Query().Get("appids")
		w.Header().Set("ETag", `"app-`+appID+`"`)
		if appID == "999" {
			serveFixture(t, w, "malformed_response.json")
			return
		}
		_, _ = fmt.Fprintf(w, `{"%s":{"success":true,"data":{"type":"game","name":"Game %s","steam_appid":%s,"required_age":0,"release_date":{"coming_soon":true,"date":"Coming Soon"}}}}`, appID, appID, appID)
	}), HTTPConfig{})
	if _, err := client.GameDetails(context.Background(), 620); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GameDetails(context.Background(), 440); err != nil {
		t.Fatal(err)
	}
	first, firstOK := client.CacheMetadata(GameDetailsCacheKey(620))
	second, secondOK := client.CacheMetadata(GameDetailsCacheKey(440))
	if !firstOK || !secondOK || first.ETag != `"app-620"` || second.ETag != `"app-440"` {
		t.Fatalf("cache entries = %#v/%v, %#v/%v", first, firstOK, second, secondOK)
	}
	if _, err := client.GameDetails(context.Background(), 999); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("malformed response error = %v", err)
	}
	if _, ok := client.CacheMetadata(GameDetailsCacheKey(999)); ok {
		t.Fatal("malformed response published cache metadata")
	}
}

func TestSuccessfulResponsesAdvanceCacheValidator(t *testing.T) {
	var calls atomic.Int32
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		expected := ""
		if call == 2 {
			expected = `"recent-v1"`
		} else if call == 3 {
			expected = `"recent-v2"`
		}
		if got := r.Header.Get("If-None-Match"); got != expected {
			t.Fatalf("call %d If-None-Match = %q, want %q", call, got, expected)
		}
		w.Header().Set("ETag", fmt.Sprintf(`"recent-v%d"`, call))
		serveFixture(t, w, "recent_games.json")
	}), HTTPConfig{})
	for range 3 {
		if _, _, err := client.RecentlyPlayed(context.Background(), "76561197960287930"); err != nil {
			t.Fatal(err)
		}
	}
	metadata, ok := client.CacheMetadata(RecentlyPlayedCacheKey("76561197960287930"))
	if !ok || metadata.ETag != `"recent-v3"` {
		t.Fatalf("cache metadata = %#v, ok = %v", metadata, ok)
	}
}

func TestRetryAfterCannotOutliveOperationTimeout(t *testing.T) {
	var calls atomic.Int32
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusServiceUnavailable)
	}), HTTPConfig{RequestTimeout: 20 * time.Millisecond, MaxRetries: 1})
	started := time.Now()
	_, availability, err := client.RecentlyPlayed(context.Background(), "76561197960287930")
	if availability != RequestFailed || err == nil || calls.Load() != 1 || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("calls = %d, elapsed = %s, availability = %q, err = %v", calls.Load(), time.Since(started), availability, err)
	}
}

func TestStructuredResponseValidation(t *testing.T) {
	t.Run("contradictory recent count", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"response":{"total_count":0,"games":[{"appid":440}]}}`))
		}), HTTPConfig{})
		_, availability, err := client.RecentlyPlayed(context.Background(), "76561197960287930")
		if availability != RequestFailed || !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("availability = %q, err = %v", availability, err)
		}
	})

	t.Run("missing store fields", func(t *testing.T) {
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"620":{"success":true,"data":{"steam_appid":620,"required_age":"unknown"}}}`))
		}), HTTPConfig{})
		if _, err := client.GameDetails(context.Background(), 620); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("error = %v", err)
		}
	})

	invalidCases := []struct {
		name                  string
		call                  func(*HTTPClient) (Availability, error)
		body                  string
		checkAchievementCache bool
	}{
		{
			name: "owned negative playtime",
			body: `{"response":{"game_count":1,"games":[{"appid":620,"name":"Portal 2","playtime_forever":-1}]}}`,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.OwnedGames(context.Background(), "76561197960287930")
				return availability, err
			},
		},
		{
			name: "recent blank name",
			body: `{"response":{"total_count":1,"games":[{"appid":440,"name":""}]}}`,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.RecentlyPlayed(context.Background(), "76561197960287930")
				return availability, err
			},
		},
		{
			name: "wishlist negative priority",
			body: `{"response":{"items":[{"appid":100,"priority":-1,"date_added":1700000000}]}}`,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Wishlist(context.Background(), "76561197960287930")
				return availability, err
			},
		},
		{
			name:                  "achievement negative timestamp",
			body:                  `{"playerstats":{"gameName":"Portal 2","success":true,"achievements":[{"apiname":"ACH.ONE","achieved":1,"unlocktime":-1}]}}`,
			checkAchievementCache: true,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
				return availability, err
			},
		},
		{
			name:                  "achievement empty api name",
			body:                  `{"playerstats":{"gameName":"Portal 2","success":true,"achievements":[{"apiname":" ","achieved":0,"unlocktime":0}]}}`,
			checkAchievementCache: true,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
				return availability, err
			},
		},
		{
			name:                  "achievement duplicate api name",
			body:                  `{"playerstats":{"gameName":"Portal 2","success":true,"achievements":[{"apiname":"ACH.ONE","achieved":0,"unlocktime":0},{"apiname":"ACH.ONE","achieved":1,"unlocktime":1}]}}`,
			checkAchievementCache: true,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
				return availability, err
			},
		},
		{
			name:                  "achievement invalid achieved flag",
			body:                  `{"playerstats":{"gameName":"Portal 2","success":true,"achievements":[{"apiname":"ACH.ONE","achieved":2,"unlocktime":0}]}}`,
			checkAchievementCache: true,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
				return availability, err
			},
		},
		{
			name:                  "locked achievement nonzero unlock time",
			body:                  `{"playerstats":{"gameName":"Portal 2","success":true,"achievements":[{"apiname":"ACH.ONE","achieved":0,"unlocktime":1}]}}`,
			checkAchievementCache: true,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
				return availability, err
			},
		},
		{
			name:                  "achievement trailing member",
			body:                  `{"playerstats":{"gameName":"Portal 2","success":true,"achievements":[{"apiname":"ACH.ONE","achieved":0,"unlocktime":0,"unexpected":"reject"}]}}`,
			checkAchievementCache: true,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
				return availability, err
			},
		},
		{
			name:                  "achievement blank game name",
			body:                  `{"playerstats":{"gameName":" ","success":true,"achievements":[]}}`,
			checkAchievementCache: true,
			call: func(client *HTTPClient) (Availability, error) {
				_, availability, err := client.Achievements(context.Background(), "76561197960287930", 620)
				return availability, err
			},
		},
	}
	for _, test := range invalidCases {
		t.Run(test.name, func(t *testing.T) {
			client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("ETag", `"invalid"`)
				_, _ = w.Write([]byte(test.body))
			}), HTTPConfig{})
			availability, err := test.call(client)
			if availability != RequestFailed || !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("availability = %q, err = %v", availability, err)
			}
			if test.checkAchievementCache {
				if _, ok := client.CacheMetadata(AchievementsCacheKey("76561197960287930", 620)); ok {
					t.Fatal("invalid achievements response published cache metadata")
				}
			}
		})
	}
}

func TestConcurrencyAndRateLimits(t *testing.T) {
	t.Run("concurrent requests are bounded", func(t *testing.T) {
		entered := make(chan struct{}, 4)
		release := make(chan struct{})
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			<-release
			serveFixture(t, w, "recent_games.json")
		}), HTTPConfig{MaxConcurrent: 2})

		var workers sync.WaitGroup
		errorsSeen := make(chan error, 4)
		for range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				_, _, err := client.RecentlyPlayed(context.Background(), "76561197960287930")
				errorsSeen <- err
			}()
		}
		for range 2 {
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("two requests did not reach the server")
			}
		}
		select {
		case <-entered:
			t.Fatal("more than two requests reached the server concurrently")
		case <-time.After(30 * time.Millisecond):
		}
		close(release)
		workers.Wait()
		close(errorsSeen)
		for err := range errorsSeen {
			if err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("requests are paced", func(t *testing.T) {
		calls := make(chan time.Time, 2)
		client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls <- time.Now()
			serveFixture(t, w, "recent_games.json")
		}), HTTPConfig{MinRequestInterval: 30 * time.Millisecond})
		for range 2 {
			if _, _, err := client.RecentlyPlayed(context.Background(), "76561197960287930"); err != nil {
				t.Fatal(err)
			}
		}
		first, second := <-calls, <-calls
		if delta := second.Sub(first); delta < 20*time.Millisecond {
			t.Fatalf("requests were bursty: separation = %s", delta)
		}
	})
}

func fixtureClient(t *testing.T, handler http.Handler, overrides HTTPConfig) *HTTPClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	overrides.APIKey = testAPIKey
	overrides.HTTPClient = server.Client()
	overrides.WebAPIBaseURL = server.URL
	overrides.StoreBaseURL = server.URL
	if overrides.RequestTimeout == 0 {
		overrides.RequestTimeout = time.Second
	}
	if overrides.MinRequestInterval == 0 {
		overrides.MinRequestInterval = -1
	}
	client, err := NewClient(overrides)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func serveFixture(t *testing.T, w http.ResponseWriter, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  HTTPConfig
	}{
		{name: "missing key", cfg: HTTPConfig{}},
		{name: "invalid web base", cfg: HTTPConfig{APIKey: "key", WebAPIBaseURL: "://bad"}},
		{name: "invalid store base", cfg: HTTPConfig{APIKey: "key", StoreBaseURL: "file:///tmp"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewClient(test.cfg)
			if err == nil || (test.cfg.APIKey != "" && strings.Contains(fmt.Sprint(err), test.cfg.APIKey)) {
				t.Fatalf("NewClient error = %v", err)
			}
		})
	}
}
