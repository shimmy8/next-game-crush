package steam

import (
	"strconv"
	"time"
)

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
)

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

type AchievementSummary struct {
	AppID          int64
	GameName       string
	Total          int
	Unlocked       int
	Percentage     float64
	LastUnlockedAt time.Time
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
