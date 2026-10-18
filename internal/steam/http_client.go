package steam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultWebAPIBaseURL       = "https://api.steampowered.com"
	defaultStoreBaseURL        = "https://store.steampowered.com"
	defaultStoreServiceBaseURL = "https://partner.steam-api.com"
	defaultRequestTimeout      = 10 * time.Second
	defaultMaxResponseBytes    = int64(4 << 20)
	defaultRetryBase           = 250 * time.Millisecond
	defaultMinInterval         = 100 * time.Millisecond
	defaultMaxConcurrent       = 4
	defaultMaxRetries          = 2
)

var (
	ErrNotModified      = errors.New("Steam response not modified")
	ErrResponseTooLarge = errors.New("Steam response exceeds size limit")
	ErrUnavailable      = errors.New("Steam resource unavailable")
	ErrInvalidResponse  = errors.New("Steam returned an invalid response")
)

type HTTPConfig struct {
	APIKey              string
	HTTPClient          *http.Client
	WebAPIBaseURL       string
	StoreBaseURL        string
	StoreServiceBaseURL string
	RequestTimeout      time.Duration
	MaxResponseBytes    int64
	MaxRetries          int
	RetryBase           time.Duration
	MinRequestInterval  time.Duration
	MaxConcurrent       int
	CacheValidators     map[CacheKey]CacheValidator
	// HostThrottle is opt-in for a dedicated catalog client. Interactive
	// clients keep their own rate and concurrency limits.
	HostThrottle HostThrottle
}

// HostThrottle connects catalog transport to shared adaptive state. Implementors
// own persistence and gradual recovery; the HTTP client never stores raw URLs
// or response bodies in throttle observations.
type HostThrottle interface {
	Before(context.Context, string) error
	Observe(host string, status int, retryAfter *time.Duration, err error)
	Limit(host string) int
}

// NewHTTPClient is an explicit constructor alias for callers that name the
// concrete implementation rather than the Client boundary.
func NewHTTPClient(cfg HTTPConfig) (*HTTPClient, error) { return NewClient(cfg) }

// RequestError is safe to log. It intentionally contains neither the request
// URL nor the response body, both of which may contain credentials or private
// upstream diagnostics.
type RequestError struct {
	Operation  Operation
	StatusCode int
	RetryAfter time.Duration
	Temporary  bool
	cause      error
}

func (e *RequestError) Error() string {
	label := strings.ReplaceAll(string(e.Operation), "_", " ")
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s request failed with HTTP %d", label, e.StatusCode)
	}
	return fmt.Sprintf("%s request failed", label)
}

func (e *RequestError) Unwrap() error { return e.cause }

type HTTPClient struct {
	apiKey             string
	httpClient         *http.Client
	webBase            *url.URL
	storeBase          *url.URL
	storeServiceBase   *url.URL
	requestTimeout     time.Duration
	maxResponseBytes   int64
	maxRetries         int
	retryBase          time.Duration
	minRequestInterval time.Duration
	semaphore          chan struct{}
	validators         map[CacheKey]CacheValidator
	now                func() time.Time

	limiterMu    sync.Mutex
	lastCall     time.Time
	cacheMu      sync.RWMutex
	cache        map[CacheKey]CacheMetadata
	hostThrottle HostThrottle
	hostMu       sync.Mutex
	hostActive   map[string]int
	hostChanged  chan struct{}
}

func NewClient(cfg HTTPConfig) (*HTTPClient, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("Steam Web API key is required")
	}
	webBase, err := parseBaseURL(cfg.WebAPIBaseURL, defaultWebAPIBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Steam Web API base URL: %w", err)
	}
	storeBase, err := parseBaseURL(cfg.StoreBaseURL, defaultStoreBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Steam Store base URL: %w", err)
	}
	storeServiceBase, err := parseBaseURL(cfg.StoreServiceBaseURL, defaultStoreServiceBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Steam Store Service base URL: %w", err)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: defaultRequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	// Keep redirects disabled even when a caller supplies a transport/client.
	// A Steam request URL can contain the server's API key.
	clientCopy := *cfg.HTTPClient
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTPClient = &clientCopy
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.RequestTimeout < 0 {
		return nil, errors.New("Steam request timeout must be positive")
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = defaultMaxResponseBytes
	}
	if cfg.MaxResponseBytes < 0 {
		return nil, errors.New("Steam response limit must be positive")
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaultMaxRetries
	} else if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.RetryBase == 0 {
		cfg.RetryBase = defaultRetryBase
	}
	if cfg.RetryBase < 0 {
		return nil, errors.New("Steam retry base must not be negative")
	}
	if cfg.MinRequestInterval == 0 {
		cfg.MinRequestInterval = defaultMinInterval
	} else if cfg.MinRequestInterval < 0 {
		cfg.MinRequestInterval = 0
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	if cfg.MaxConcurrent < 0 {
		return nil, errors.New("Steam concurrency limit must be positive")
	}
	validators := make(map[CacheKey]CacheValidator, len(cfg.CacheValidators))
	cache := make(map[CacheKey]CacheMetadata, len(cfg.CacheValidators))
	for key, validator := range cfg.CacheValidators {
		validators[key] = validator
		cache[key] = CacheMetadata{ETag: validator.ETag, LastModified: validator.LastModified}
	}
	return &HTTPClient{
		apiKey: strings.TrimSpace(cfg.APIKey), httpClient: cfg.HTTPClient,
		webBase: webBase, storeBase: storeBase, storeServiceBase: storeServiceBase, requestTimeout: cfg.RequestTimeout,
		maxResponseBytes: cfg.MaxResponseBytes, maxRetries: cfg.MaxRetries,
		retryBase: cfg.RetryBase, minRequestInterval: cfg.MinRequestInterval,
		semaphore: make(chan struct{}, cfg.MaxConcurrent), validators: validators,
		now: time.Now, cache: cache,
		hostThrottle: cfg.HostThrottle, hostActive: make(map[string]int), hostChanged: make(chan struct{}),
	}, nil
}

func parseBaseURL(raw, fallback string) (*url.URL, error) {
	if raw == "" {
		raw = fallback
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return nil, errors.New("must be an absolute HTTP(S) URL without user information")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func (c *HTTPClient) CacheMetadata(key CacheKey) (CacheMetadata, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	metadata, ok := c.cache[key]
	return metadata, ok
}

func (c *HTTPClient) PlayerSummary(ctx context.Context, steamID string) (PlayerSummary, error) {
	cacheKey := CacheKey{Operation: PlayerSummaryOperation, Resource: steamID}
	values := url.Values{"key": {c.apiKey}, "steamids": {steamID}, "format": {"json"}}
	response, _, err := c.get(ctx, cacheKey, c.webBase, "/ISteamUser/GetPlayerSummaries/v2/", values)
	if err != nil {
		return PlayerSummary{}, err
	}
	var payload struct {
		Response struct {
			Players []struct {
				SteamID                  string `json:"steamid"`
				CommunityVisibilityState int    `json:"communityvisibilitystate"`
				DisplayName              string `json:"personaname"`
				AvatarURL                string `json:"avatarfull"`
			} `json:"players"`
		} `json:"response"`
	}
	if err := decode(response.body, &payload, PlayerSummaryOperation); err != nil {
		return PlayerSummary{}, err
	}
	var matching *struct {
		SteamID                  string `json:"steamid"`
		CommunityVisibilityState int    `json:"communityvisibilitystate"`
		DisplayName              string `json:"personaname"`
		AvatarURL                string `json:"avatarfull"`
	}
	for i := range payload.Response.Players {
		if payload.Response.Players[i].SteamID != steamID {
			continue
		}
		if matching != nil {
			return PlayerSummary{}, invalidResponse(PlayerSummaryOperation)
		}
		matching = &payload.Response.Players[i]
	}
	if matching == nil {
		return PlayerSummary{}, invalidResponse(PlayerSummaryOperation)
	}
	visibility := VisibilityUnknown
	switch matching.CommunityVisibilityState {
	case 3:
		visibility = VisibilityPublic
	case 1:
		visibility = VisibilityPrivate
	default:
		return PlayerSummary{}, invalidResponse(PlayerSummaryOperation)
	}
	displayName := strings.TrimSpace(matching.DisplayName)
	avatarURL := ""
	if strings.TrimSpace(matching.AvatarURL) != "" {
		avatarURL, err = ValidateAvatarURL(matching.AvatarURL)
	}
	if displayName == "" || err != nil {
		return PlayerSummary{}, invalidResponse(PlayerSummaryOperation)
	}
	return PlayerSummary{Visibility: visibility, DisplayName: displayName, AvatarURL: avatarURL}, nil
}

func (c *HTTPClient) OwnedGames(ctx context.Context, steamID string) ([]OwnedGame, Availability, error) {
	cacheKey := OwnedGamesCacheKey(steamID)
	values := url.Values{"key": {c.apiKey}, "steamid": {steamID}, "include_appinfo": {"1"}, "include_played_free_games": {"1"}, "format": {"json"}}
	response, availability, err := c.get(ctx, cacheKey, c.webBase, "/IPlayerService/GetOwnedGames/v1/", values)
	if err != nil {
		return nil, availability, err
	}
	games, availability, err := parseOwnedGames(response.body)
	if err != nil || availability != Available {
		return games, availability, err
	}
	c.recordMetadata(cacheKey, response.header, false)
	return games, Available, nil
}

// ProbeOwnedGames checks owned-games access without consuming or publishing
// cache validators. Preflight callers do not persist the returned body, so a
// probe must not make the following import conditional on that discarded body.
func (c *HTTPClient) ProbeOwnedGames(ctx context.Context, steamID string) ([]OwnedGame, Availability, error) {
	cacheKey := OwnedGamesCacheKey(steamID)
	values := url.Values{"key": {c.apiKey}, "steamid": {steamID}, "include_appinfo": {"1"}, "include_played_free_games": {"1"}, "format": {"json"}}
	response, availability, err := c.getWithoutCache(ctx, cacheKey, c.webBase, "/IPlayerService/GetOwnedGames/v1/", values)
	if err != nil {
		return nil, availability, err
	}
	return parseOwnedGames(response.body)
}

func parseOwnedGames(body []byte) ([]OwnedGame, Availability, error) {
	var payload struct {
		Response struct {
			GameCount *int `json:"game_count"`
			Games     *[]struct {
				AppID           int64  `json:"appid"`
				Name            string `json:"name"`
				PlaytimeForever int64  `json:"playtime_forever"`
				PlaytimeWindows int64  `json:"playtime_windows_forever"`
				PlaytimeMac     int64  `json:"playtime_mac_forever"`
				PlaytimeLinux   int64  `json:"playtime_linux_forever"`
				IconHash        string `json:"img_icon_url"`
				LastPlayed      int64  `json:"rtime_last_played"`
			} `json:"games"`
		} `json:"response"`
	}
	if err := decode(body, &payload, OwnedGamesOperation); err != nil {
		return nil, RequestFailed, err
	}
	if payload.Response.GameCount == nil {
		return nil, Unavailable, nil
	}
	if *payload.Response.GameCount == 0 && payload.Response.Games == nil {
		return []OwnedGame{}, Available, nil
	}
	if payload.Response.Games == nil || *payload.Response.GameCount < 0 || *payload.Response.GameCount != len(*payload.Response.Games) {
		return nil, RequestFailed, invalidResponse(OwnedGamesOperation)
	}
	games := make([]OwnedGame, 0, len(*payload.Response.Games))
	for _, game := range *payload.Response.Games {
		if game.AppID <= 0 || strings.TrimSpace(game.Name) == "" || game.PlaytimeForever < 0 || game.PlaytimeWindows < 0 || game.PlaytimeMac < 0 || game.PlaytimeLinux < 0 || game.LastPlayed < 0 {
			return nil, RequestFailed, invalidResponse(OwnedGamesOperation)
		}
		games = append(games, OwnedGame{AppID: game.AppID, Name: game.Name, PlaytimeForever: game.PlaytimeForever, PlaytimeWindows: game.PlaytimeWindows, PlaytimeMac: game.PlaytimeMac, PlaytimeLinux: game.PlaytimeLinux, IconHash: game.IconHash, LastPlayedAt: unixTime(game.LastPlayed)})
	}
	return games, Available, nil
}

func (c *HTTPClient) RecentlyPlayed(ctx context.Context, steamID string) ([]RecentGame, Availability, error) {
	cacheKey := RecentlyPlayedCacheKey(steamID)
	values := url.Values{"key": {c.apiKey}, "steamid": {steamID}, "format": {"json"}}
	response, availability, err := c.get(ctx, cacheKey, c.webBase, "/IPlayerService/GetRecentlyPlayedGames/v1/", values)
	if err != nil {
		return nil, availability, err
	}
	var payload struct {
		Response struct {
			TotalCount *int `json:"total_count"`
			Games      []struct {
				AppID            int64  `json:"appid"`
				Name             string `json:"name"`
				PlaytimeTwoWeeks int64  `json:"playtime_2weeks"`
				PlaytimeForever  int64  `json:"playtime_forever"`
				PlaytimeWindows  int64  `json:"playtime_windows_forever"`
				PlaytimeMac      int64  `json:"playtime_mac_forever"`
				PlaytimeLinux    int64  `json:"playtime_linux_forever"`
				IconHash         string `json:"img_icon_url"`
				LastPlayed       int64  `json:"rtime_last_played"`
			} `json:"games"`
		} `json:"response"`
	}
	if err := decode(response.body, &payload, RecentlyPlayedOperation); err != nil {
		return nil, RequestFailed, err
	}
	if payload.Response.TotalCount == nil {
		return nil, Unavailable, nil
	}
	if *payload.Response.TotalCount < len(payload.Response.Games) || (*payload.Response.TotalCount == 0 && len(payload.Response.Games) != 0) || (*payload.Response.TotalCount > 0 && len(payload.Response.Games) == 0) {
		return nil, RequestFailed, invalidResponse(RecentlyPlayedOperation)
	}
	games := make([]RecentGame, 0, len(payload.Response.Games))
	for _, game := range payload.Response.Games {
		if game.AppID <= 0 || strings.TrimSpace(game.Name) == "" || game.PlaytimeTwoWeeks < 0 || game.PlaytimeForever < 0 || game.PlaytimeWindows < 0 || game.PlaytimeMac < 0 || game.PlaytimeLinux < 0 || game.LastPlayed < 0 {
			return nil, RequestFailed, invalidResponse(RecentlyPlayedOperation)
		}
		games = append(games, RecentGame{AppID: game.AppID, Name: game.Name, PlaytimeTwoWeeks: game.PlaytimeTwoWeeks, PlaytimeForever: game.PlaytimeForever, PlaytimeWindows: game.PlaytimeWindows, PlaytimeMac: game.PlaytimeMac, PlaytimeLinux: game.PlaytimeLinux, IconHash: game.IconHash, LastPlayedAt: unixTime(game.LastPlayed)})
	}
	c.recordMetadata(cacheKey, response.header, false)
	return games, Available, nil
}

func (c *HTTPClient) Wishlist(ctx context.Context, steamID string) ([]WishlistItem, Availability, error) {
	cacheKey := WishlistCacheKey(steamID)
	values := url.Values{"steamid": {steamID}}
	response, availability, err := c.get(ctx, cacheKey, c.webBase, "/IWishlistService/GetWishlist/v1/", values)
	if err != nil {
		return nil, availability, err
	}
	if result := strings.TrimSpace(response.header.Get("X-Eresult")); result != "" && result != "1" {
		return nil, Unavailable, nil
	}
	var payload struct {
		Response struct {
			Items *[]struct {
				AppID     int64 `json:"appid"`
				Priority  int   `json:"priority"`
				DateAdded int64 `json:"date_added"`
			} `json:"items"`
		} `json:"response"`
	}
	if err := decode(response.body, &payload, WishlistOperation); err != nil {
		return nil, RequestFailed, err
	}
	if payload.Response.Items == nil {
		return nil, Unavailable, nil
	}
	items := make([]WishlistItem, 0, len(*payload.Response.Items))
	for _, item := range *payload.Response.Items {
		if item.AppID <= 0 || item.Priority < 0 || item.DateAdded < 0 {
			return nil, RequestFailed, invalidResponse(WishlistOperation)
		}
		items = append(items, WishlistItem{AppID: item.AppID, Priority: item.Priority, AddedAt: unixTime(item.DateAdded)})
	}
	c.recordMetadata(cacheKey, response.header, false)
	return items, Available, nil
}

func (c *HTTPClient) Achievements(ctx context.Context, steamID string, appID int64) (AchievementSummary, Availability, error) {
	if appID <= 0 {
		return AchievementSummary{}, RequestFailed, invalidResponse(AchievementsOperation)
	}
	cacheKey := AchievementsCacheKey(steamID, appID)
	values := url.Values{"key": {c.apiKey}, "steamid": {steamID}, "appid": {strconv.FormatInt(appID, 10)}, "l": {"en"}, "format": {"json"}}
	response, availability, err := c.get(ctx, cacheKey, c.webBase, "/ISteamUserStats/GetPlayerAchievements/v1/", values)
	if err != nil {
		return AchievementSummary{}, availability, err
	}
	var payload struct {
		PlayerStats *struct {
			GameName     string             `json:"gameName"`
			Success      *bool              `json:"success"`
			Achievements *[]json.RawMessage `json:"achievements"`
		} `json:"playerstats"`
	}
	if err := decode(response.body, &payload, AchievementsOperation); err != nil {
		return AchievementSummary{}, RequestFailed, err
	}
	if payload.PlayerStats == nil || payload.PlayerStats.Success == nil || !*payload.PlayerStats.Success || payload.PlayerStats.Achievements == nil {
		return AchievementSummary{}, Unavailable, nil
	}
	if strings.TrimSpace(payload.PlayerStats.GameName) == "" {
		return AchievementSummary{}, RequestFailed, invalidResponse(AchievementsOperation)
	}
	summary := AchievementSummary{
		AppID:        appID,
		GameName:     payload.PlayerStats.GameName,
		Achievements: make([]Achievement, 0, len(*payload.PlayerStats.Achievements)),
		Total:        len(*payload.PlayerStats.Achievements),
	}
	apiNames := make(map[string]struct{}, summary.Total)
	for _, rawAchievement := range *payload.PlayerStats.Achievements {
		var achievement struct {
			APIName     string `json:"apiname"`
			Achieved    int    `json:"achieved"`
			UnlockTime  int64  `json:"unlocktime"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := decodeStrict(rawAchievement, &achievement, AchievementsOperation); err != nil {
			return AchievementSummary{}, RequestFailed, err
		}
		apiName := strings.TrimSpace(achievement.APIName)
		_, duplicate := apiNames[apiName]
		if apiName == "" || duplicate || (achievement.Achieved != 0 && achievement.Achieved != 1) || achievement.UnlockTime < 0 || (achievement.Achieved == 0 && achievement.UnlockTime != 0) {
			return AchievementSummary{}, RequestFailed, invalidResponse(AchievementsOperation)
		}
		apiNames[apiName] = struct{}{}
		item := Achievement{
			APIName:     apiName,
			Name:        achievement.Name,
			Description: achievement.Description,
			Achieved:    achievement.Achieved == 1,
			UnlockedAt:  unixTime(achievement.UnlockTime),
		}
		summary.Achievements = append(summary.Achievements, item)
		if achievement.Achieved != 0 {
			summary.Unlocked++
			unlockedAt := unixTime(achievement.UnlockTime)
			if unlockedAt.After(summary.LastUnlockedAt) {
				summary.LastUnlockedAt = unlockedAt
			}
		}
	}
	if summary.Total > 0 {
		summary.Percentage = float64(summary.Unlocked) / float64(summary.Total)
	}
	c.recordMetadata(cacheKey, response.header, false)
	return summary, Available, nil
}

func (c *HTTPClient) AchievementSchema(ctx context.Context, appID int64) (AchievementSchema, Availability, error) {
	if appID <= 0 {
		return AchievementSchema{}, RequestFailed, invalidResponse(AchievementSchemaOperation)
	}
	cacheKey := AchievementSchemaCacheKey(appID)
	values := url.Values{"key": {c.apiKey}, "appid": {strconv.FormatInt(appID, 10)}, "l": {"en"}, "format": {"json"}}
	response, availability, err := c.get(ctx, cacheKey, c.webBase, "/ISteamUserStats/GetSchemaForGame/v2/", values)
	if err != nil {
		return AchievementSchema{}, availability, err
	}
	var payload struct {
		Game *struct {
			AvailableGameStats *struct {
				Achievements *[]struct {
					APIName     string `json:"name"`
					Name        string `json:"displayName"`
					Description string `json:"description"`
					Default     int    `json:"defaultvalue"`
					Hidden      int    `json:"hidden"`
					Icon        string `json:"icon"`
					IconGray    string `json:"icongray"`
				} `json:"achievements"`
			} `json:"availableGameStats"`
		} `json:"game"`
	}
	if err := decode(response.body, &payload, AchievementSchemaOperation); err != nil {
		return AchievementSchema{}, RequestFailed, err
	}
	if payload.Game == nil || payload.Game.AvailableGameStats == nil || payload.Game.AvailableGameStats.Achievements == nil {
		return AchievementSchema{}, Unavailable, nil
	}
	schema := AchievementSchema{AppID: appID, Achievements: make([]AchievementDefinition, 0, len(*payload.Game.AvailableGameStats.Achievements))}
	apiNames := make(map[string]struct{}, len(*payload.Game.AvailableGameStats.Achievements))
	for _, achievement := range *payload.Game.AvailableGameStats.Achievements {
		apiName := strings.TrimSpace(achievement.APIName)
		_, duplicate := apiNames[apiName]
		if apiName == "" || duplicate {
			return AchievementSchema{}, RequestFailed, invalidResponse(AchievementSchemaOperation)
		}
		apiNames[apiName] = struct{}{}
		schema.Achievements = append(schema.Achievements, AchievementDefinition{
			APIName:     apiName,
			Name:        achievement.Name,
			Description: achievement.Description,
		})
	}
	c.recordMetadata(cacheKey, response.header, false)
	return schema, Available, nil
}

func (c *HTTPClient) GameDetails(ctx context.Context, appID int64) (GameDetails, error) {
	if appID <= 0 {
		return GameDetails{}, invalidResponse(GameDetailsOperation)
	}
	cacheKey := GameDetailsCacheKey(appID)
	values := url.Values{"appids": {strconv.FormatInt(appID, 10)}, "cc": {"us"}, "l": {"en"}}
	response, _, err := c.get(ctx, cacheKey, c.storeBase, "/api/appdetails", values)
	if err != nil {
		return GameDetails{}, err
	}
	var payload map[string]struct {
		Success bool `json:"success"`
		Data    struct {
			Type             string          `json:"type"`
			Name             string          `json:"name"`
			AppID            int64           `json:"steam_appid"`
			RequiredAge      json.RawMessage `json:"required_age"`
			Free             bool            `json:"is_free"`
			ShortDescription string          `json:"short_description"`
			HeaderImage      string          `json:"header_image"`
			Developers       []string        `json:"developers"`
			Publishers       []string        `json:"publishers"`
			Platforms        *struct {
				Windows *bool `json:"windows"`
				Mac     *bool `json:"mac"`
				Linux   *bool `json:"linux"`
			} `json:"platforms"`
			Categories []struct {
				ID          int    `json:"id"`
				Description string `json:"description"`
			} `json:"categories"`
			Genres []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
			} `json:"genres"`
			ReleaseDate struct {
				ComingSoon bool   `json:"coming_soon"`
				Date       string `json:"date"`
			} `json:"release_date"`
		} `json:"data"`
	}
	if err := decode(response.body, &payload, GameDetailsOperation); err != nil {
		return GameDetails{}, err
	}
	record, ok := payload[strconv.FormatInt(appID, 10)]
	if !ok || !record.Success {
		return GameDetails{}, ErrUnavailable
	}
	requiredAge, ageErr := parseAge(record.Data.RequiredAge)
	if record.Data.AppID != appID || strings.TrimSpace(record.Data.Type) == "" || strings.TrimSpace(record.Data.Name) == "" || (strings.TrimSpace(record.Data.ReleaseDate.Date) == "" && !record.Data.ReleaseDate.ComingSoon) || ageErr != nil {
		return GameDetails{}, invalidResponse(GameDetailsOperation)
	}
	platforms := record.Data.Platforms
	if platforms == nil || platforms.Windows == nil || platforms.Mac == nil || platforms.Linux == nil {
		return GameDetails{}, invalidResponse(GameDetailsOperation)
	}
	details := GameDetails{AppID: appID, Type: record.Data.Type, Name: record.Data.Name, RequiredAge: requiredAge, Free: record.Data.Free, ShortDescription: record.Data.ShortDescription, HeaderImage: record.Data.HeaderImage, Developers: append([]string(nil), record.Data.Developers...), Publishers: append([]string(nil), record.Data.Publishers...), ReleaseDate: ReleaseDate{ComingSoon: record.Data.ReleaseDate.ComingSoon, Raw: record.Data.ReleaseDate.Date}}
	details.Platforms = Platforms{Windows: *platforms.Windows, Mac: *platforms.Mac, Linux: *platforms.Linux}
	for _, category := range record.Data.Categories {
		details.Categories = append(details.Categories, Category{ID: category.ID, Description: category.Description})
	}
	for _, genre := range record.Data.Genres {
		details.Genres = append(details.Genres, Genre{ID: genre.ID, Description: genre.Description})
	}
	c.recordMetadata(cacheKey, response.header, false)
	return details, nil
}

func (c *HTTPClient) DeckCompatibility(ctx context.Context, appID int64) (DeckCompatibility, Availability, error) {
	if appID <= 0 {
		return "", RequestFailed, invalidResponse(DeckCompatibilityOperation)
	}
	cacheKey := DeckCompatibilityCacheKey(appID)
	values := url.Values{"nAppID": {strconv.FormatInt(appID, 10)}}
	response, availability, err := c.get(ctx, cacheKey, c.storeBase, "/saleaction/ajaxgetdeckappcompatibilityreport", values)
	if err != nil {
		return "", availability, err
	}
	var payload struct {
		Success *int `json:"success"`
		Results *struct {
			ResolvedCategory *int `json:"resolved_category"`
		} `json:"results"`
	}
	if err := decode(response.body, &payload, DeckCompatibilityOperation); err != nil {
		return "", RequestFailed, err
	}
	if payload.Success == nil {
		return "", RequestFailed, invalidResponse(DeckCompatibilityOperation)
	}
	if *payload.Success != 1 {
		return "", Unavailable, nil
	}
	if payload.Results == nil || payload.Results.ResolvedCategory == nil {
		return "", RequestFailed, invalidResponse(DeckCompatibilityOperation)
	}
	var status DeckCompatibility
	switch *payload.Results.ResolvedCategory {
	case 3:
		status = DeckVerified
	case 2:
		status = DeckPlayable
	case 1:
		status = DeckUnsupported
	case 0:
		status = DeckUnknown
	default:
		return "", RequestFailed, invalidResponse(DeckCompatibilityOperation)
	}
	c.recordMetadata(cacheKey, response.header, false)
	return status, Available, nil
}

type httpResponse struct {
	body   []byte
	header http.Header
}

func (c *HTTPClient) get(ctx context.Context, cacheKey CacheKey, base *url.URL, requestPath string, values url.Values) (httpResponse, Availability, error) {
	return c.getWithCache(ctx, cacheKey, base, requestPath, values, true)
}

func (c *HTTPClient) getWithoutCache(ctx context.Context, cacheKey CacheKey, base *url.URL, requestPath string, values url.Values) (httpResponse, Availability, error) {
	return c.getWithCache(ctx, cacheKey, base, requestPath, values, false)
}

func (c *HTTPClient) getWithCache(ctx context.Context, cacheKey CacheKey, base *url.URL, requestPath string, values url.Values, useCache bool) (httpResponse, Availability, error) {
	operation := cacheKey.Operation
	operationCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	var lastError *RequestError
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.retryBase * time.Duration(1<<(attempt-1))
			if lastError != nil && lastError.RetryAfter > delay {
				delay = lastError.RetryAfter
			}
			if deadline, ok := operationCtx.Deadline(); ok && delay >= time.Until(deadline) {
				return httpResponse{}, RequestFailed, lastError
			}
			if err := waitContext(operationCtx, delay); err != nil {
				return httpResponse{}, RequestFailed, &RequestError{Operation: operation, Temporary: true, cause: err}
			}
		}
		if err := c.waitForRateLimit(operationCtx); err != nil {
			return httpResponse{}, RequestFailed, &RequestError{Operation: operation, Temporary: true, cause: err}
		}
		response, requestErr := c.getOnce(operationCtx, cacheKey, base, requestPath, values, useCache)
		if requestErr == nil {
			if response.statusCode == http.StatusNotModified {
				if useCache {
					c.recordMetadata(cacheKey, response.header, true)
				}
				return httpResponse{}, NotModified, ErrNotModified
			}
			return httpResponse{body: response.body, header: response.header}, Available, nil
		}
		lastError = requestErr
		if !requestErr.Temporary || attempt == c.maxRetries || operationCtx.Err() != nil {
			return httpResponse{}, RequestFailed, requestErr
		}
	}
	return httpResponse{}, RequestFailed, lastError
}

type rawResponse struct {
	body       []byte
	header     http.Header
	statusCode int
}

func (c *HTTPClient) getOnce(ctx context.Context, cacheKey CacheKey, base *url.URL, requestPath string, values url.Values, useCache bool) (result rawResponse, resultErr *RequestError) {
	operation := cacheKey.Operation
	if c.hostThrottle != nil {
		release, err := c.acquireHost(ctx, strings.ToLower(base.Hostname()))
		if err != nil {
			return rawResponse{}, &RequestError{Operation: operation, Temporary: true, cause: err}
		}
		defer release()
		if err := c.hostThrottle.Before(ctx, strings.ToLower(base.Hostname())); err != nil {
			return rawResponse{}, &RequestError{Operation: operation, Temporary: true, cause: err}
		}
	}
	select {
	case c.semaphore <- struct{}{}:
		defer func() { <-c.semaphore }()
	case <-ctx.Done():
		return rawResponse{}, &RequestError{Operation: operation, Temporary: true, cause: ctx.Err()}
	}
	requestURL := *base
	requestURL.Path = strings.TrimRight(base.Path, "/") + requestPath
	requestURL.RawQuery = values.Encode()
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return rawResponse{}, &RequestError{Operation: operation, cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "next-game-crush/1")
	if validator, ok := c.cacheValidator(cacheKey); useCache && ok {
		if validator.ETag != "" {
			req.Header.Set("If-None-Match", validator.ETag)
		}
		if validator.LastModified != "" {
			req.Header.Set("If-Modified-Since", validator.LastModified)
		}
	}
	client := c.httpClient
	if operation == SearchOperation {
		// Search is non-personal even when a caller supplies a client with a
		// cookie jar. Preserve the supplied client's behavior for other APIs.
		copy := *client
		copy.Jar = nil
		client = &copy
	}
	if c.hostThrottle != nil {
		defer func() {
			status := result.statusCode
			var hint *time.Duration
			var observedError error
			if resultErr != nil {
				status, observedError = resultErr.StatusCode, resultErr
				if resultErr.RetryAfter > 0 {
					delay := resultErr.RetryAfter
					hint = &delay
				}
			}
			c.hostThrottle.Observe(strings.ToLower(base.Hostname()), status, hint, observedError)
		}()
	}
	response, err := client.Do(req)
	if err != nil {
		return rawResponse{}, &RequestError{Operation: operation, Temporary: requestCtx.Err() != nil || isTemporaryTransport(err), cause: sanitizedTransportError(err)}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return rawResponse{header: response.Header.Clone(), statusCode: response.StatusCode}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return rawResponse{}, &RequestError{Operation: operation, StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), c.now()), Temporary: retryableStatus(response.StatusCode)}
	}
	responseLimit := c.maxResponseBytes
	if operation == SearchOperation && responseLimit > maxSearchResponseBytes {
		responseLimit = maxSearchResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return rawResponse{}, &RequestError{Operation: operation, Temporary: true, cause: errors.New("could not read response")}
	}
	if int64(len(body)) > responseLimit {
		return rawResponse{}, &RequestError{Operation: operation, cause: ErrResponseTooLarge}
	}
	return rawResponse{body: body, header: response.Header.Clone(), statusCode: response.StatusCode}, nil
}

func (c *HTTPClient) acquireHost(ctx context.Context, host string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		limit := max(1, min(c.hostThrottle.Limit(host), cap(c.semaphore)))
		c.hostMu.Lock()
		if c.hostActive[host] < limit {
			c.hostActive[host]++
			c.hostMu.Unlock()
			return func() {
				c.hostMu.Lock()
				c.hostActive[host]--
				close(c.hostChanged)
				c.hostChanged = make(chan struct{})
				c.hostMu.Unlock()
			}, nil
		}
		changed := c.hostChanged
		c.hostMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (c *HTTPClient) recordMetadata(key CacheKey, header http.Header, merge bool) {
	c.cacheMu.Lock()
	metadata := CacheMetadata{FetchedAt: c.now(), ETag: header.Get("ETag"), LastModified: header.Get("Last-Modified"), CacheControl: header.Get("Cache-Control"), Expires: header.Get("Expires")}
	if merge {
		previous := c.cache[key]
		if metadata.ETag == "" {
			metadata.ETag = previous.ETag
		}
		if metadata.LastModified == "" {
			metadata.LastModified = previous.LastModified
		}
		if metadata.CacheControl == "" {
			metadata.CacheControl = previous.CacheControl
		}
		if metadata.Expires == "" {
			metadata.Expires = previous.Expires
		}
	}
	c.cache[key] = metadata
	c.validators[key] = CacheValidator{ETag: metadata.ETag, LastModified: metadata.LastModified}
	c.cacheMu.Unlock()
}

func (c *HTTPClient) cacheValidator(key CacheKey) (CacheValidator, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	validator, ok := c.validators[key]
	return validator, ok && (validator.ETag != "" || validator.LastModified != "")
}

func (c *HTTPClient) waitForRateLimit(ctx context.Context) error {
	if c.minRequestInterval <= 0 {
		return nil
	}
	c.limiterMu.Lock()
	defer c.limiterMu.Unlock()
	wait := c.lastCall.Add(c.minRequestInterval).Sub(c.now())
	if wait > 0 {
		if err := waitContext(ctx, wait); err != nil {
			return err
		}
	}
	c.lastCall = c.now()
	return nil
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func decode(body []byte, target any, operation Operation) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return invalidResponse(operation)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return invalidResponse(operation)
	}
	return nil
}

func decodeStrict(body []byte, target any, operation Operation) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalidResponse(operation)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return invalidResponse(operation)
	}
	return nil
}

func invalidResponse(operation Operation) error {
	return &RequestError{Operation: operation, cause: ErrInvalidResponse}
}

func unixTime(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func parseAge(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, ErrInvalidResponse
	}
	var age int
	if json.Unmarshal(raw, &age) == nil {
		if age < 0 {
			return 0, ErrInvalidResponse
		}
		return age, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		age, err := strconv.Atoi(text)
		if err == nil && age >= 0 {
			return age, nil
		}
	}
	return 0, ErrInvalidResponse
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0
		}
		const maxSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if seconds > maxSeconds {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func isTemporaryTransport(err error) bool {
	type temporary interface{ Temporary() bool }
	var candidate temporary
	return errors.As(err, &candidate) && candidate.Temporary()
}

func sanitizedTransportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("transport error")
}
