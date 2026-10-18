package steam

import "context"

// Client is the Steam boundary consumed by synchronization workflows.
type Client interface {
	PlayerSummary(context.Context, string) (PlayerSummary, error)
	ProbeOwnedGames(context.Context, string) ([]OwnedGame, Availability, error)
	OwnedGames(context.Context, string) ([]OwnedGame, Availability, error)
	RecentlyPlayed(context.Context, string) ([]RecentGame, Availability, error)
	Wishlist(context.Context, string) ([]WishlistItem, Availability, error)
	Achievements(context.Context, string, int64) (AchievementSummary, Availability, error)
	AchievementSchema(context.Context, int64) (AchievementSchema, Availability, error)
	GameDetails(context.Context, int64) (GameDetails, error)
	DeckCompatibility(context.Context, int64) (DeckCompatibility, Availability, error)
}

var _ Client = (*HTTPClient)(nil)
