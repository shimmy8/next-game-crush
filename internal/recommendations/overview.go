package recommendations

import (
	"context"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/catalog"
)

type DiscoveryCollection struct {
	Featured *Recommendation  `json:"featured"`
	Items    []Recommendation `json:"items"`
	Total    int              `json:"total"`
}

type WishlistSourceState struct {
	Availability          string
	LastSuccessfulFetchAt *time.Time
}

type WishlistCollection struct {
	Items                 []Recommendation `json:"items"`
	Total                 int              `json:"total"`
	Status                string           `json:"status"`
	Availability          string           `json:"availability"`
	LastSuccessfulFetchAt *time.Time       `json:"lastSuccessfulFetchAt"`
	DegradedReason        *string          `json:"degradedReason"`
}

type Overview struct {
	Discoveries   DiscoveryCollection        `json:"discoveries"`
	Wishlist      WishlistCollection         `json:"wishlist"`
	Calendar      Calendar                   `json:"calendar"`
	CatalogStatus catalog.PresentationStatus `json:"catalogStatus"`
}

type WishlistStateReader interface {
	WishlistSourceState(context.Context, uuid.UUID) (WishlistSourceState, error)
}

// BuildOverview uses the published factor snapshot for membership, independently
// of current upstream availability and dashboard card limits.
func BuildOverview(items []Recommendation, wishlist WishlistSourceState, status catalog.PresentationStatus) Overview {
	result := Overview{
		Discoveries:   DiscoveryCollection{Items: []Recommendation{}},
		Wishlist:      WishlistCollection{Items: []Recommendation{}, Status: "unknown", Availability: "unknown", LastSuccessfulFetchAt: wishlist.LastSuccessfulFetchAt},
		CatalogStatus: publicCatalogStatus(status),
	}
	ordered := Sort(items)
	for index := range ordered {
		item := ordered[index]
		item.Wishlisted = item.Wishlisted || clamp(item.Factors.Wishlist) > 0
		ordered[index] = item
		if item.Factors.Wishlist > 0 {
			result.Wishlist.Total++
			if len(result.Wishlist.Items) < 6 {
				result.Wishlist.Items = append(result.Wishlist.Items, item)
			}
		} else {
			result.Discoveries.Total++
			if result.Discoveries.Featured == nil {
				featured := item
				result.Discoveries.Featured = &featured
			} else if len(result.Discoveries.Items) < 6 {
				result.Discoveries.Items = append(result.Discoveries.Items, item)
			}
		}
	}
	result.Calendar = nonNullCalendar(BuildCalendar(ordered))
	switch wishlist.Availability {
	case "available", "not_modified":
		result.Wishlist.Availability = wishlist.Availability
		if wishlist.LastSuccessfulFetchAt != nil {
			result.Wishlist.Status = "ready"
		}
	case "unavailable", "request_failed":
		result.Wishlist.Availability = wishlist.Availability
		result.Wishlist.Status = "unavailable"
		if wishlist.LastSuccessfulFetchAt != nil {
			result.Wishlist.Status = "stale"
		}
		reason := wishlist.Availability
		if reason == "unavailable" {
			reason = "private_or_unavailable"
		}
		result.Wishlist.DegradedReason = &reason
	}
	return result
}

func nonNullCalendar(value Calendar) Calendar {
	if value.ExactDates == nil {
		value.ExactDates = []Recommendation{}
	}
	if value.Month == nil {
		value.Month = []Recommendation{}
	}
	if value.Quarter == nil {
		value.Quarter = []Recommendation{}
	}
	if value.Year == nil {
		value.Year = []Recommendation{}
	}
	if value.ComingSoon == nil {
		value.ComingSoon = []Recommendation{}
	}
	if value.TBA == nil {
		value.TBA = []Recommendation{}
	}
	return value
}

func publicCatalogStatus(value catalog.PresentationStatus) catalog.PresentationStatus {
	switch value.Status {
	case "ready", "backfilling", "stale", "unavailable", "disabled":
	case "updating":
		value.Status = "backfilling"
	case "degraded":
		value.Status = "unavailable"
	default:
		value.Status = "unavailable"
	}
	return value
}

// SetOverviewSources configures optional read dependencies before serving requests.
func (s *Service) SetOverviewSources(wishlist WishlistStateReader, discovery catalog.DiscoveryStatusReader) {
	s.wishlistSource = wishlist
	s.discoveryStatus = discovery
}

func (s *Service) Overview(ctx context.Context, userID uuid.UUID) (Overview, error) {
	items, err := s.Latest(ctx, userID)
	if err != nil {
		return Overview{}, err
	}
	wishlist := WishlistSourceState{}
	if s.wishlistSource != nil {
		wishlist, err = s.wishlistSource.WishlistSourceState(ctx, userID)
		if err != nil {
			return Overview{}, err
		}
	}
	status := catalog.PresentationStatus{Status: "disabled"}
	if s.discoveryStatus != nil {
		status, err = s.discoveryStatus.DiscoveryStatus(ctx)
		if err != nil {
			status = catalog.PresentationStatus{Status: "unavailable"}
		}
	}
	return BuildOverview(items, wishlist, status), nil
}
