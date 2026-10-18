package steam

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Visibility string

const (
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
	VisibilityUnknown Visibility = "unknown"
)

type PlayerSummary struct {
	Visibility  Visibility
	DisplayName string
	AvatarURL   string
}

var allowedAvatarHosts = map[string]struct{}{
	"avatars.steamstatic.com":            {},
	"avatars.cloudflare.steamstatic.com": {},
	"avatars.fastly.steamstatic.com":     {},
	"avatars.akamai.steamstatic.com":     {},
}

func ValidateAvatarURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || parsed.Host != parsed.Hostname() {
		return "", errors.New("invalid Steam avatar URL")
	}
	if _, ok := allowedAvatarHosts[strings.ToLower(parsed.Hostname())]; !ok {
		return "", errors.New("invalid Steam avatar URL")
	}
	return parsed.String(), nil
}

// ValidateCoverURL accepts only HTTPS assets served from Valve's Steam CDN.
// The URL is presentation data from a stored upstream snapshot, so callers
// must treat an invalid value as absent rather than proxying or rendering it.
func ValidateCoverURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || parsed.Host != parsed.Hostname() {
		return "", errors.New("invalid Steam cover URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "steamstatic.com" && !strings.HasSuffix(host, ".steamstatic.com") {
		return "", errors.New("invalid Steam cover URL")
	}
	return parsed.String(), nil
}

// Availability describes whether a Steam data source yielded a trustworthy
// snapshot. Callers must not replace a previous successful snapshot when the
// value is Unavailable, RequestFailed, or NotModified.
type Availability string

const (
	Available     Availability = "available"
	Unavailable   Availability = "unavailable"
	RequestFailed Availability = "request_failed"
	NotModified   Availability = "not_modified"
)

// Operation identifies a remote resource for cache metadata and validators.
type Operation string

const (
	OwnedGamesOperation     Operation = "owned_games"
	RecentlyPlayedOperation Operation = "recently_played"
	WishlistOperation       Operation = "wishlist"
	AchievementsOperation   Operation = "achievements"
	GameDetailsOperation    Operation = "game_details"
	PlayerSummaryOperation  Operation = "player_summary"
)

const AchievementSchemaOperation Operation = "achievement_schema"

// CacheKey identifies one cacheable upstream resource. Resource-scoped keys
// prevent validators for one game or user from being applied to another.
type CacheKey struct {
	Operation Operation
	Resource  string
}

func OwnedGamesCacheKey(steamID string) CacheKey {
	return CacheKey{Operation: OwnedGamesOperation, Resource: steamID}
}

func RecentlyPlayedCacheKey(steamID string) CacheKey {
	return CacheKey{Operation: RecentlyPlayedOperation, Resource: steamID}
}

func WishlistCacheKey(steamID string) CacheKey {
	return CacheKey{Operation: WishlistOperation, Resource: steamID}
}

func AchievementsCacheKey(steamID string, appID int64) CacheKey {
	return CacheKey{Operation: AchievementsOperation, Resource: steamID + ":" + strconv.FormatInt(appID, 10)}
}

func AchievementSchemaCacheKey(appID int64) CacheKey {
	return CacheKey{Operation: AchievementSchemaOperation, Resource: strconv.FormatInt(appID, 10)}
}

func GameDetailsCacheKey(appID int64) CacheKey {
	return CacheKey{Operation: GameDetailsOperation, Resource: strconv.FormatInt(appID, 10)}
}

type CacheValidator struct {
	ETag         string
	LastModified string
}

type CacheMetadata struct {
	FetchedAt    time.Time
	ETag         string
	LastModified string
	CacheControl string
	Expires      string
}

type OwnedGame struct {
	AppID           int64
	Name            string
	PlaytimeForever int64
	PlaytimeWindows int64
	PlaytimeMac     int64
	PlaytimeLinux   int64
	IconHash        string
	LastPlayedAt    time.Time
}

type RecentGame struct {
	AppID            int64
	Name             string
	PlaytimeTwoWeeks int64
	PlaytimeForever  int64
	PlaytimeWindows  int64
	PlaytimeMac      int64
	PlaytimeLinux    int64
	IconHash         string
	LastPlayedAt     time.Time
}

type WishlistItem struct {
	AppID    int64
	Priority int
	AddedAt  time.Time
}

type Achievement struct {
	APIName        string
	Name           string
	Description    string
	MetadataSource string
	Achieved       bool
	UnlockedAt     time.Time
}

type AchievementSummary struct {
	AppID          int64
	GameName       string
	Achievements   []Achievement
	Total          int
	Unlocked       int
	Percentage     float64
	LastUnlockedAt time.Time
}

type AchievementDefinition struct {
	APIName     string
	Name        string
	Description string
}

type AchievementSchema struct {
	AppID        int64
	Achievements []AchievementDefinition
}

type Category struct {
	ID          int
	Description string
}

type Genre struct {
	ID          string
	Description string
}

type ReleaseDate struct {
	ComingSoon bool
	Raw        string
}

type GameDetails struct {
	AppID            int64
	Type             string
	Name             string
	RequiredAge      int
	Free             bool
	ShortDescription string
	HeaderImage      string
	Developers       []string
	Publishers       []string
	Categories       []Category
	Genres           []Genre
	ReleaseDate      ReleaseDate
}
