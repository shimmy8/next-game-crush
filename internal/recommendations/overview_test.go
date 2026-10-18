package recommendations

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/catalog"
)

func TestOverviewPartitionsAndCapsPublishedRanking(t *testing.T) {
	items := []Recommendation{}
	for i := 0; i < 8; i++ {
		for _, wishlist := range []float64{0, 1} {
			items = append(items, Recommendation{GameID: uuid.New(), Name: fmt.Sprintf("Game %d", i), Score: float64(i) + wishlist*100, Factors: Factors{Wishlist: wishlist}, RankingVersion: 9, Release: ReleaseWindow{Precision: "month"}})
		}
	}
	got := BuildOverview(items, WishlistSourceState{}, catalog.PresentationStatus{Status: "disabled"})
	if got.Discoveries.Featured == nil || got.Discoveries.Featured.Name != "Game 7" || got.Discoveries.Featured.Factors.Wishlist != 0 || len(got.Discoveries.Items) != 6 || len(got.Wishlist.Items) != 6 || got.Discoveries.Total != 8 || got.Wishlist.Total != 8 || len(got.Calendar.Month) != 16 {
		t.Fatalf("overview partition/caps = %#v", got)
	}
	seen := map[uuid.UUID]bool{got.Discoveries.Featured.GameID: true}
	for _, item := range append(got.Discoveries.Items, got.Wishlist.Items...) {
		if seen[item.GameID] || item.RankingVersion != 9 {
			t.Fatal("duplicate or mixed ranking")
		}
		seen[item.GameID] = true
	}
}

func TestOverviewHandlesEmptyAndSingleCollection(t *testing.T) {
	for _, source := range []string{"empty", "discovery", "wishlist"} {
		t.Run(source, func(t *testing.T) {
			items := []Recommendation{}
			if source != "empty" {
				item := Recommendation{GameID: uuid.New()}
				if source == "wishlist" {
					item.Factors.Wishlist = 1
				}
				items = append(items, item)
			}
			got := BuildOverview(items, WishlistSourceState{}, catalog.PresentationStatus{})
			if got.Discoveries.Items == nil || got.Wishlist.Items == nil {
				t.Fatal("null collection")
			}
			if source != "discovery" && got.Discoveries.Featured != nil {
				t.Fatal("wishlist became featured")
			}
			for _, bucket := range [][]Recommendation{got.Calendar.ExactDates, got.Calendar.Month, got.Calendar.Quarter, got.Calendar.Year, got.Calendar.ComingSoon, got.Calendar.TBA} {
				if bucket == nil {
					t.Fatal("null calendar bucket")
				}
			}
		})
	}
}

func TestOverviewTieOrder(t *testing.T) {
	ids := []uuid.UUID{uuid.MustParse("00000000-0000-0000-0000-000000000001"), uuid.MustParse("00000000-0000-0000-0000-000000000002")}
	for _, wishlist := range []float64{0, 1} {
		items := []Recommendation{{GameID: ids[1], Name: "alpha", Score: 2}, {GameID: ids[1], Name: "Alpha", Score: 2}, {GameID: ids[0], Name: "Alpha", Score: 2}, {GameID: uuid.New(), Name: "z", Score: 3}}
		for i := range items {
			items[i].Factors.Wishlist = wishlist
		}
		got := BuildOverview(items, WishlistSourceState{}, catalog.PresentationStatus{})
		ordered := got.Calendar.TBA
		if ordered[0].Name != "z" || ordered[1].GameID != ids[0] || ordered[2].Name != "Alpha" || ordered[3].Name != "alpha" {
			t.Fatalf("order=%#v", ordered)
		}
		if wishlist > 0 {
			if !reflect.DeepEqual(got.Wishlist.Items, ordered) {
				t.Fatal("wishlist order")
			}
		} else if got.Discoveries.Featured.GameID != ordered[0].GameID || !reflect.DeepEqual(got.Discoveries.Items, ordered[1:]) {
			t.Fatal("discovery order")
		}
	}
}

func TestOverviewPreservesAllReleasePrecisions(t *testing.T) {
	items := []Recommendation{}
	for _, precision := range []string{"day", "month", "quarter", "year", "coming_soon", "unknown"} {
		items = append(items, Recommendation{GameID: uuid.New(), Release: ReleaseWindow{Precision: precision, ExactDate: "2027-03-01"}})
	}
	items = append(items, Recommendation{GameID: uuid.New(), Release: ReleaseWindow{Precision: "day"}})
	got := BuildOverview(items, WishlistSourceState{}, catalog.PresentationStatus{}).Calendar
	if len(got.ExactDates) != 1 || len(got.Month) != 1 || len(got.Quarter) != 1 || len(got.Year) != 1 || len(got.ComingSoon) != 1 || len(got.TBA) != 2 || got.ExactDates[0].Release.ExactDate != "2027-03-01" {
		t.Fatalf("calendar=%#v", got)
	}
}

func TestOverviewWishlistSourceStatus(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		availability               string
		success                    *time.Time
		status, reason, normalized string
	}{
		{"available", &now, "ready", "", "available"}, {"not_modified", &now, "ready", "", "not_modified"},
		{"unavailable", &now, "stale", "private_or_unavailable", "unavailable"}, {"request_failed", &now, "stale", "request_failed", "request_failed"},
		{"unavailable", nil, "unavailable", "private_or_unavailable", "unavailable"}, {"request_failed", nil, "unavailable", "request_failed", "request_failed"},
		{"available", nil, "unknown", "", "available"}, {"not_modified", nil, "unknown", "", "not_modified"}, {"", nil, "unknown", "", "unknown"}, {"bad private payload", &now, "unknown", "", "unknown"},
	} {
		t.Run(tc.availability+tc.status, func(t *testing.T) {
			got := BuildOverview(nil, WishlistSourceState{tc.availability, tc.success}, catalog.PresentationStatus{}).Wishlist
			if got.Status != tc.status || got.Availability != tc.normalized || got.LastSuccessfulFetchAt != tc.success || (tc.reason == "") != (got.DegradedReason == nil) || got.DegradedReason != nil && *got.DegradedReason != tc.reason {
				t.Fatalf("source=%#v", got)
			}
		})
	}
	preserved := Recommendation{GameID: uuid.New(), Factors: Factors{Wishlist: 1}}
	got := BuildOverview([]Recommendation{preserved}, WishlistSourceState{Availability: "unavailable", LastSuccessfulFetchAt: &now}, catalog.PresentationStatus{})
	if got.Wishlist.Status != "stale" || got.Wishlist.Total != 1 || got.Wishlist.Items[0].GameID != preserved.GameID || got.Discoveries.Total != 0 {
		t.Fatal("unavailable wishlist lost published membership")
	}
}

type overviewWishlistReader struct {
	state WishlistSourceState
	err   error
	owner uuid.UUID
}

func (r *overviewWishlistReader) WishlistSourceState(_ context.Context, owner uuid.UUID) (WishlistSourceState, error) {
	r.owner = owner
	return r.state, r.err
}

type overviewCatalogReader struct {
	status catalog.PresentationStatus
	err    error
}

func (r overviewCatalogReader) DiscoveryStatus(context.Context) (catalog.PresentationStatus, error) {
	return r.status, r.err
}

type overviewRankingReader struct {
	Repository
	items []Recommendation
	calls int
	owner uuid.UUID
}

func (r *overviewRankingReader) Latest(_ context.Context, owner uuid.UUID) ([]Recommendation, error) {
	r.calls++
	r.owner = owner
	return r.items, nil
}

func TestOverviewCatalogStatusMapping(t *testing.T) {
	for _, tc := range []struct{ provider, public string }{{"ready", "ready"}, {"updating", "backfilling"}, {"stale", "stale"}, {"degraded", "unavailable"}, {"disabled", "disabled"}, {"backfilling", "backfilling"}, {"unavailable", "unavailable"}, {"bad", "unavailable"}} {
		got := BuildOverview(nil, WishlistSourceState{}, catalog.PresentationStatus{Status: tc.provider, ActiveDiscoveredGames: 12})
		if got.CatalogStatus.Status != tc.public || got.CatalogStatus.ActiveDiscoveredGames != 12 {
			t.Fatalf("status=%#v", got.CatalogStatus)
		}
	}
	repo := &overviewRankingReader{}
	service := &Service{repository: repo, eligibility: allowAllCurrentEligibility{}, clock: recommendationTestClock}
	owner := uuid.New()
	got, err := service.Overview(context.Background(), owner)
	if err != nil || got.CatalogStatus.Status != "disabled" {
		t.Fatalf("nil provider=%#v, %v", got, err)
	}
	stamp := time.Now().UTC()
	wishlist := &overviewWishlistReader{}
	service.SetOverviewSources(wishlist, overviewCatalogReader{catalog.PresentationStatus{Status: "ready", ActiveDiscoveredGames: 7, LastSuccessfulScanAt: &stamp}, errors.New("internal upstream body")})
	got, err = service.Overview(context.Background(), owner)
	if err != nil || got.CatalogStatus.Status != "unavailable" || got.CatalogStatus.ActiveDiscoveredGames != 0 || got.CatalogStatus.LastSuccessfulScanAt != nil || repo.calls != 2 || repo.owner != owner || wishlist.owner != owner {
		t.Fatalf("degraded read=%#v err=%v calls=%d", got, err, repo.calls)
	}
	service.SetOverviewSources(wishlist, overviewCatalogReader{status: catalog.PresentationStatus{Status: "stale", ActiveDiscoveredGames: 7, LastSuccessfulScanAt: &stamp}})
	got, err = service.Overview(context.Background(), owner)
	if err != nil || got.CatalogStatus.Status != "stale" || got.CatalogStatus.ActiveDiscoveredGames != 7 || got.CatalogStatus.LastSuccessfulScanAt == nil {
		t.Fatalf("reported stale aggregate=%#v err=%v", got, err)
	}
	service.SetOverviewSources(wishlist, overviewCatalogReader{err: errors.New("failed")})
	got, err = service.Overview(context.Background(), owner)
	if err != nil || got.CatalogStatus.Status != "unavailable" {
		t.Fatalf("unavailable=%#v err=%v", got, err)
	}
	wishlist.err = errors.New("private database failure")
	if _, err = service.Overview(context.Background(), owner); err == nil {
		t.Fatal("wishlist failure hidden")
	}
}
