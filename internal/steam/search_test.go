package steam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestSearchFeedFixturesAndRequests(t *testing.T) {
	for _, tc := range []struct {
		feed                  SearchFeed
		filter, sort, fixture string
		id                    int64
	}{
		{FeedPopularUpcoming, "popularcomingsoon", "", "search_popular_upcoming_page.json", 101},
		{FeedPopularNew, "popularnew", "", "search_popular_new_page.json", 102},
		{FeedRecent, "recent", "Released_DESC", "search_recent_page.json", 103},
		{FeedComingSoon, "comingsoon", "Released_ASC", "search_comingsoon_page.json", 104},
	} {
		t.Run(string(tc.feed), func(t *testing.T) {
			body := searchFixture(t, tc.fixture)
			c := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				for key, want := range map[string]string{"filter": tc.filter, "sort_by": tc.sort, "l": "english", "cc": "us", "category1": "998", "ignore_preferences": "1", "start": "0", "count": "2", "infinite": "1"} {
					if q.Get(key) != want {
						t.Errorf("%s=%q, want %q", key, q.Get(key), want)
					}
				}
				if r.URL.Path != "/search/results/" || r.Header.Get("Cookie") != "" || q.Has("key") {
					t.Error("search must be a non-personal Store request")
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write(body)
			}), HTTPConfig{})
			page, err := c.Search(context.Background(), SearchRequest{Feed: tc.feed, Count: 2})
			if err != nil {
				t.Fatal(err)
			}
			if page.Start != 0 || page.Count != 1 || page.Total != 1 || page.HasMore || len(page.Items) != 1 {
				t.Fatalf("page=%+v", page)
			}
			item := page.Items[0]
			if item.AppID != tc.id || item.Name != "Fixture & Game" || item.ReleaseText != "18 Oct, 2026" || item.Position != 0 || len(item.Fingerprint) != 64 {
				t.Fatalf("item=%+v", item)
			}
			if tc.feed == FeedPopularNew && (item.Review.Band != ReviewPositive || item.Review.Total != 1284 || item.Review.PositivePercent == nil || *item.Review.PositivePercent != 91) {
				t.Fatalf("review=%+v", item.Review)
			}
		})
	}
}

func TestNormalizeReview(t *testing.T) {
	for _, label := range []string{"Overwhelmingly Positive", "Very Positive", "Positive", "Mostly Positive"} {
		t.Run(label, func(t *testing.T) {
			got := NormalizeReview("  "+label+"  ", intPointer(91), intPointer(1284))
			if got.Band != ReviewPositive || got.Label != label || got.Total != 1284 || got.PositivePercent == nil || *got.PositivePercent != 91 {
				t.Fatalf("review=%+v", got)
			}
		})
	}
	for _, label := range []string{"Mixed", "Mostly Negative", "Negative", "Very Negative", "Overwhelmingly Negative"} {
		t.Run(label, func(t *testing.T) {
			if got := NormalizeReview(label, intPointer(20), intPointer(10)); got.Band != ReviewMixedOrBelow {
				t.Fatalf("review=%+v", got)
			}
		})
	}
	for _, tc := range []struct {
		label          string
		percent, total *int
		band           ReviewBand
	}{
		{"No user reviews", nil, intPointer(0), ReviewUnrated},
		{"No user reviews", nil, nil, ReviewUnrated},
		{"Very Positive", nil, intPointer(10), ReviewUnknown},
		{"", nil, nil, ReviewUnknown}, {"", nil, intPointer(0), ReviewUnknown},
		{"Unknown label", intPointer(90), intPointer(10), ReviewUnknown},
		{"Very Positive", intPointer(101), intPointer(10), ReviewUnknown},
		{"Mixed", intPointer(-1), intPointer(10), ReviewUnknown},
		{"Very Positive", intPointer(90), intPointer(-1), ReviewUnknown},
		{"Very Positive", intPointer(90), intPointer(0), ReviewUnknown},
		{"Very Positive", intPointer(90), nil, ReviewUnknown},
		{"No user reviews", intPointer(0), intPointer(0), ReviewUnknown},
		{"No user reviews", nil, intPointer(4), ReviewUnknown},
		{"Very Positive", intPointer(5), intPointer(10), ReviewUnknown},
		{"Mixed", intPointer(95), intPointer(10), ReviewUnknown},
		{strings.Repeat("x", 65), nil, intPointer(0), ReviewUnknown},
	} {
		if got := NormalizeReview(tc.label, tc.percent, tc.total); got.Band != tc.band {
			t.Errorf("label=%q review=%+v, want %s", tc.label, got, tc.band)
		}
	}
}

func TestSearchReviewsAndFingerprint(t *testing.T) {
	body := searchFixture(t, "search_popular_new_page.json")
	c := searchBodyClient(t, &body)
	before, err := c.Search(context.Background(), SearchRequest{Feed: FeedPopularNew, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.ReplaceAll(strings.ReplaceAll(string(body), "91%", "92%"), "1,284", "1,300"))
	after, err := c.Search(context.Background(), SearchRequest{Feed: FeedPopularNew, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if after.Items[0].Fingerprint != before.Items[0].Fingerprint || after.Items[0].Review.Total != 1300 {
		t.Fatal("review-only updates changed material identity or failed to refresh review")
	}
	body = []byte(strings.ReplaceAll(string(body), "18 Oct, 2026", "19 Oct, 2026"))
	changed, err := c.Search(context.Background(), SearchRequest{Feed: FeedPopularNew, Count: 2})
	if err != nil || changed.Items[0].Fingerprint == before.Items[0].Fingerprint {
		t.Fatalf("release change not fingerprinted: %v", err)
	}
	body = searchFixture(t, "search_malformed_review.json")
	malformed, err := c.Search(context.Background(), SearchRequest{Feed: FeedPopularNew, Count: 2})
	if err != nil || malformed.Items[0].Review.Band != ReviewUnknown {
		t.Fatalf("malformed review must be unknown: %+v %v", malformed, err)
	}
	for _, markup := range []string{
		`<span class="search_review_summary" data-tooltip-html="No user reviews"></span>`,
		`<span class="search_review_summary" data-tooltip-html="No user reviews&lt;br&gt;0 user reviews"></span>`,
	} {
		body = searchEnvelope(0, 2, 1, searchRow("101", "Game", "Coming soon", markup))
		page, err := c.Search(context.Background(), SearchRequest{Feed: FeedPopularUpcoming, Count: 2})
		if err != nil || page.Items[0].Review.Band != ReviewUnrated {
			t.Fatalf("explicit zero review: %+v %v", page, err)
		}
	}
	for _, markup := range []string{"", `<span class="search_review_summary" data-tooltip-html="Very Positive"></span>`, `<span class="search_review_summary" data-tooltip-html="Very Positive&lt;br&gt;101% of the 10 user reviews for this game are positive."></span>`, `<span class="search_review_summary" data-tooltip-html="Very Positive&lt;br&gt;91% of the 1,2 user reviews for this game are positive."></span>`} {
		body = searchEnvelope(0, 2, 1, searchRow("101", "Game", "Coming soon", markup))
		page, err := c.Search(context.Background(), SearchRequest{Feed: FeedPopularUpcoming, Count: 2})
		if err != nil || page.Items[0].Review.Band != ReviewUnknown {
			t.Fatalf("missing/malformed must be unknown: %+v %v", page, err)
		}
	}
}

func TestSearchPaginationDuplicatesAndValidation(t *testing.T) {
	body := searchEnvelope(2, 3, 8, searchRow("102", "Second", "2026", "")+searchRow("101", "First", "October 2026", "")+searchRow("102", "Second", "2026", ""))
	c := searchBodyClient(t, &body)
	page, err := c.Search(context.Background(), SearchRequest{Feed: FeedRecent, Start: 2, Count: 3})
	if err != nil || !page.HasMore || page.Start != 2 || page.Count != 3 || page.Total != 8 || len(page.Items) != 2 || page.Items[0].AppID != 102 || page.Items[0].Position != 2 || page.Items[1].AppID != 101 || page.Items[1].Position != 3 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	for _, invalid := range [][]byte{
		[]byte(`{}`), []byte(`{"success":0,"start":0,"total_count":0,"results_html":""}`),
		[]byte(`{"success":1,"start":0,"total_count":0}`), []byte(`{"success":1,"start":0,"results_html":""}`),
		searchFixture(t, "search_layout_changed.json"),
		searchEnvelope(0, 2, 5, ""), searchEnvelope(1, 2, 1, ""),
		searchEnvelope(0, 2, 0, "<div>changed layout</div>"),
		searchEnvelope(0, 2, 1, searchRow("0", "Game", "2026", "")),
		searchEnvelope(0, 2, 1, searchRow("101,102", "Game", "2026", "")),
		searchEnvelope(0, 2, 1, searchRow("101", "", "2026", "")),
		searchEnvelope(0, 2, 1, searchRow("101", strings.Repeat("x", 513), "2026", "")),
		searchEnvelope(0, 2, 1, searchRow("101", "Game", strings.Repeat("x", 513), "")),
		searchEnvelope(0, 2, 1, searchRow("101", "Game", "2026", "")+searchRow("102", "Game", "2026", "")),
	} {
		body = invalid
		if _, err := c.Search(context.Background(), SearchRequest{Feed: FeedRecent, Count: 2}); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("invalid page accepted or untyped failure: %v", err)
		}
	}
	body = searchEnvelope(0, 2, 3, searchRow("101", "Game", "2026", ""))
	if page, err := c.Search(context.Background(), SearchRequest{Feed: FeedRecent, Count: 2}); err != nil || page.Count != 1 || !page.HasMore {
		t.Fatalf("short progressing page=%+v %v", page, err)
	}
	body = searchEnvelope(0, 2, 0, "")
	if page, err := c.Search(context.Background(), SearchRequest{Feed: FeedRecent, Count: 2}); err != nil || page.Count != 0 || len(page.Items) != 0 || page.HasMore {
		t.Fatalf("valid empty end=%+v %v", page, err)
	}
	for _, req := range []SearchRequest{{Feed: "bad", Count: 1}, {Feed: FeedRecent, Start: -1, Count: 1}, {Feed: FeedRecent, Count: 0}, {Feed: FeedRecent, Count: 201}, {Feed: FeedRecent, Start: int(^uint(0) >> 1), Count: 1}} {
		if _, err := c.Search(context.Background(), req); err == nil {
			t.Error("invalid request accepted")
		}
	}
}

func TestSearchDuplicateReviewConflictsAreRejected(t *testing.T) {
	positive := `<span class="search_review_summary" data-tooltip-html="Very Positive&lt;br&gt;91% of the 10 user reviews for this game are positive."></span>`
	mixed := `<span class="search_review_summary" data-tooltip-html="Mixed&lt;br&gt;55% of the 10 user reviews for this game are positive."></span>`
	for _, tc := range []struct {
		name, first, second string
	}{
		{"positive_then_mixed", positive, mixed},
		{"mixed_then_positive", mixed, positive},
		{"unknown_then_positive", "", positive},
		{"positive_then_unknown", positive, ""},
		{"different_percentage", positive, strings.Replace(positive, "91%", "92%", 1)},
		{"different_total", positive, strings.Replace(positive, "10 user", "11 user", 1)},
		{"different_label", positive, strings.Replace(positive, "Very Positive", "Positive", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := searchEnvelope(0, 2, 2, searchRow("101", "Game", "2026", tc.first)+searchRow("101", "Game", "2026", tc.second))
			page, err := parseSearchPage(body, SearchRequest{Feed: FeedPopularNew, Count: 2})
			if !errors.Is(err, ErrInvalidResponse) || len(page.Items) != 0 || page.Count != 0 || page.HasMore {
				t.Fatalf("conflicting duplicates must reject the entire page: page=%+v err=%v", page, err)
			}
		})
	}
}

func TestSearchConsistentDuplicateReviewsPreservePosition(t *testing.T) {
	for _, tc := range []struct {
		name, markup string
		band         ReviewBand
	}{
		{"positive", `<span class="search_review_summary" data-tooltip-html="Very Positive&lt;br&gt;91% of the 10 user reviews for this game are positive."></span>`, ReviewPositive},
		{"mixed", `<span class="search_review_summary" data-tooltip-html="Mixed&lt;br&gt;55% of the 10 user reviews for this game are positive."></span>`, ReviewMixedOrBelow},
		{"unrated", `<span class="search_review_summary" data-tooltip-html="No user reviews"></span>`, ReviewUnrated},
		{"unknown", "", ReviewUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := searchRow("101", "Game", "2026", tc.markup)
			body := searchEnvelope(4, 3, 7, row+searchRow("99", "Neighbor", "2026", "")+row)
			page, err := parseSearchPage(body, SearchRequest{Feed: FeedPopularNew, Start: 4, Count: 3})
			if err != nil || page.Count != 3 || page.HasMore || len(page.Items) != 2 {
				t.Fatalf("consistent duplicates must remain valid: page=%+v err=%v", page, err)
			}
			if page.Items[0].AppID != 101 || page.Items[0].Position != 4 || page.Items[0].Review.Band != tc.band || page.Items[1].AppID != 99 || page.Items[1].Position != 5 {
				t.Fatalf("deduplication lost review evidence or first position: %+v", page.Items)
			}
		})
	}
}

func TestSearchTransportBoundaries(t *testing.T) {
	for _, kind := range []string{"content_type", "missing_content_type", "oversize", "redirect", "rate_limit", "cookie_jar"} {
		t.Run(kind, func(t *testing.T) {
			forwarded := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded = true }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch kind {
				case "content_type":
					w.Header().Set("Content-Type", "text/html")
				case "missing_content_type":
					w.Header()["Content-Type"] = nil
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat(" ", (2<<20)+1)))
					return
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusFound)
					return
				case "rate_limit":
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(429)
					_, _ = w.Write([]byte("private upstream canary"))
					return
				case "cookie_jar":
					if r.Header.Get("Cookie") != "" {
						t.Error("cookie jar personalized search")
					}
				}
				_, _ = w.Write(searchEnvelope(0, 2, 0, ""))
			}))
			defer server.Close()
			jar, _ := cookiejar.New(nil)
			u, _ := url.Parse(server.URL)
			jar.SetCookies(u, []*http.Cookie{{Name: "sessionid", Value: "cookie-canary"}})
			httpClient := server.Client()
			httpClient.Jar = jar
			c, err := NewClient(HTTPConfig{APIKey: testAPIKey, HTTPClient: httpClient, StoreBaseURL: server.URL, MinRequestInterval: -1, MaxRetries: -1})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Search(context.Background(), SearchRequest{Feed: FeedRecent, Count: 2})
			if kind == "cookie_jar" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || forwarded {
				t.Fatalf("transport boundary lost: %v", err)
			}
			if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), testAPIKey) {
				t.Error("unsafe error")
			}
			if kind == "oversize" && !errors.Is(err, ErrResponseTooLarge) {
				t.Errorf("size error=%v", err)
			}
			if kind == "rate_limit" {
				var typed *RequestError
				if !errors.As(err, &typed) || !typed.Temporary || typed.RetryAfter.Seconds() != 60 {
					t.Errorf("retry hint lost: %v", err)
				}
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func searchFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func searchBodyClient(t *testing.T, body *[]byte) *HTTPClient {
	t.Helper()
	return fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(*body)
	}), HTTPConfig{})
}

func searchEnvelope(start, count, total int, markup string) []byte {
	body, _ := json.Marshal(map[string]any{"success": 1, "start": start, "pagesize": count, "total_count": total, "results_html": markup})
	return body
}

func searchRow(id, name, release, reviews string) string {
	return `<a class="search_result_row" data-ds-appid="` + id + `" href="https://store.steampowered.com/app/` + id + `/"><span class="title">` + name + `</span><div class="search_released">` + release + `</div>` + reviews + `</a>`
}
