package steam

import "context"

type SearchFeed string

const (
	FeedPopularUpcoming SearchFeed = "popular_upcoming"
	FeedPopularNew      SearchFeed = "popular_new"
	FeedRecent          SearchFeed = "recent"
	FeedComingSoon      SearchFeed = "coming_soon"
)

type ReviewBand string

const (
	ReviewPositive     ReviewBand = "positive"
	ReviewMixedOrBelow ReviewBand = "mixed_or_below"
	ReviewUnrated      ReviewBand = "unrated"
	ReviewUnknown      ReviewBand = "unknown"
)

type ReviewSummary struct {
	Band            ReviewBand
	Label           string
	PositivePercent *int
	Total           int64
}

type SearchRequest struct {
	Feed         SearchFeed
	Start, Count int
}

type SearchObservation struct {
	AppID             int64
	Name, ReleaseText string
	Position          int
	Review            ReviewSummary
	Fingerprint       string
}

// Count is the consumed raw row count, including duplicates, so the durable
// next cursor is Start+Count even when Steam returns a short page. Position
// preserves the first row's zero-based absolute position; deduplication never
// reorders a feed. Empty natural-end pages have Count=0 and HasMore=false.
type SearchPage struct {
	Items               []SearchObservation
	Start, Count, Total int
	HasMore             bool
}

// SearchCatalog is separate from Client so existing user-sync clients do not
// acquire an unrelated catalog capability or change their canonical metadata.
type SearchCatalog interface {
	Search(context.Context, SearchRequest) (SearchPage, error)
}

const SearchOperation Operation = "search"
