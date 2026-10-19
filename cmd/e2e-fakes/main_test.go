package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"next-game-crush/internal/steam"
)

func TestRunRequiresHarnessMode(t *testing.T) {
	t.Setenv("TEST_HARNESS_MODE", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "TEST_HARNESS_MODE=1") {
		t.Fatalf("run error = %v", err)
	}
}

func TestMutableFixtureControlsAreUnavailableOutsideHarness(t *testing.T) {
	response := fixtureRequest(newE2EFakeHandler(false), http.MethodPost, "/__test/search-mode?mode=healthy")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestCatalogSearchFixturesCoverFourFeedsAndReviewTransition(t *testing.T) {
	fixtureDay := time.Date(2032, time.March, 15, 0, 0, 0, 0, time.UTC)
	handler := newE2EFakeHandlerAt(true, fixtureDay)
	client, err := steam.NewClient(steam.HTTPConfig{
		APIKey:             "fixture-steam-key",
		StoreBaseURL:       "https://fixture.invalid",
		HTTPClient:         &http.Client{Transport: fixtureRoundTripper{handler: handler}},
		MinRequestInterval: -1,
		MaxRetries:         -1,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		feed            steam.SearchFeed
		filter, firstID string
		firstAppID      int64
		count           int
	}{
		{steam.FeedPopularUpcoming, "popularcomingsoon", "510001", 510001, 2},
		{steam.FeedPopularNew, "popularnew", "520001", 520001, 4},
		{steam.FeedRecent, "recent", "530001", 530001, 2},
		{steam.FeedComingSoon, "comingsoon", "540001", 540001, 2},
	} {
		body := searchFixtureResponse(t, handler, tc.filter, 0, 200)
		if body.Total != tc.count || body.PageSize != tc.count || !strings.Contains(body.HTML, `data-ds-appid="`+tc.firstID+`"`) {
			t.Fatalf("filter %s response = %+v", tc.filter, body)
		}
		page, searchErr := client.Search(context.Background(), steam.SearchRequest{Feed: tc.feed, Count: 200})
		if searchErr != nil || len(page.Items) != tc.count || page.Items[0].AppID != tc.firstAppID || page.HasMore {
			t.Fatalf("parsed feed %s page=%+v err=%v", tc.feed, page, searchErr)
		}
	}

	initial := searchFixtureResponse(t, handler, "popularnew", 0, 200)
	if !strings.Contains(initial.HTML, "Very Positive") || !strings.Contains(initial.HTML, "No user reviews") || !strings.Contains(initial.HTML, "Mixed") {
		t.Fatalf("initial review evidence missing: %s", initial.HTML)
	}
	postNoContent(t, handler, "/__test/search-reviews?mode=changed")
	changed := searchFixtureResponse(t, handler, "popularnew", 0, 200)
	if !strings.Contains(changed.HTML, "62%") || strings.Contains(changed.HTML, "91%") {
		t.Fatalf("review transition missing: %s", changed.HTML)
	}
	changedPage, err := client.Search(context.Background(), steam.SearchRequest{Feed: steam.FeedPopularNew, Count: 200})
	if err != nil || changedPage.Items[0].Review.Band != steam.ReviewMixedOrBelow || changedPage.Items[0].Review.PositivePercent == nil || *changedPage.Items[0].Review.PositivePercent != 62 {
		t.Fatalf("parsed review transition=%+v err=%v", changedPage, err)
	}
	postNoContent(t, handler, "/__test/search-reviews?mode=changed_expired")
	expiredPage, err := client.Search(context.Background(), steam.SearchRequest{Feed: steam.FeedPopularNew, Count: 200})
	if err != nil || expiredPage.Items[1].ReleaseText != fixtureSearchDate(fixtureDay.AddDate(0, 0, -8)) {
		t.Fatalf("unrated expiry transition=%+v err=%v", expiredPage, err)
	}

	page := searchFixtureResponse(t, handler, "popularnew", 2, 1)
	if page.Start != 2 || page.PageSize != 1 || !strings.Contains(page.HTML, `data-ds-appid="520003"`) {
		t.Fatalf("pagination response = %+v", page)
	}
}

func TestCatalogSearchCanHoldConcurrentPopularRequests(t *testing.T) {
	handler := newE2EFakeHandler(true)
	postNoContent(t, handler, "/__test/search-mode?mode=hold_popular")
	done := make(chan struct{}, 6)
	for range 6 {
		go func() {
			response := rawSearchResponse(handler, "popularcomingsoon")
			if response.Code == http.StatusOK {
				done <- struct{}{}
			}
		}()
	}
	var counters struct {
		ActiveSearch    int `json:"activeSearch"`
		MaxActiveSearch int `json:"maxActiveSearch"`
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		decodeGET(t, handler, "/__test/catalog-counters", &counters)
		if counters.ActiveSearch == 6 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if counters.ActiveSearch != 6 || counters.MaxActiveSearch != 6 {
		t.Fatalf("held search counters = %+v", counters)
	}
	postNoContent(t, handler, "/__test/search-mode?mode=healthy")
	for range 6 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("held search did not resume")
		}
	}
}

func TestCatalogSearchFailureModesAndCounters(t *testing.T) {
	handler := newE2EFakeHandler(true)

	postNoContent(t, handler, "/__test/search-mode?mode=popular_rate_limited")
	response := rawSearchResponse(handler, "popularcomingsoon")
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "7" {
		t.Fatalf("rate limit status=%d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}

	postNoContent(t, handler, "/__test/search-mode?mode=popular_parser_broken")
	broken := searchFixtureResponse(t, handler, "popularcomingsoon", 0, 200)
	if !strings.Contains(broken.HTML, "fixture-layout-changed") || strings.Contains(broken.HTML, "search_result_row") {
		t.Fatalf("parser breakage fixture = %+v", broken)
	}
	parserRetry := rawSearchResponse(handler, "popularcomingsoon")
	if parserRetry.Code != http.StatusTooManyRequests || parserRetry.Header().Get("Retry-After") != "1" {
		t.Fatalf("parser retry status=%d retry-after=%q", parserRetry.Code, parserRetry.Header().Get("Retry-After"))
	}
	postNoContent(t, handler, "/__test/search-mode?mode=tail_rate_limited")
	tail := rawSearchResponse(handler, "recent")
	if tail.Code != http.StatusTooManyRequests || tail.Header().Get("Retry-After") != "7" {
		t.Fatalf("tail rate limit status=%d retry-after=%q", tail.Code, tail.Header().Get("Retry-After"))
	}

	getOK(t, handler, "/api/appdetails?appids=520001")
	getOK(t, handler, "/api/appdetails?appids=520001")
	if response := fixtureJSONRequest(handler, "/v1/embeddings", `{"input":["Fixture Positive Released\nA cooperative exploration game."]}`); response.Code != http.StatusOK {
		t.Fatalf("POST /v1/embeddings status=%d", response.Code)
	}
	featureBody := `{"text":{"format":{"name":"game_features_v1"}},"input":"{\"name\":\"Fixture Positive Released\",\"evidence\":[{\"key\":\"title\",\"value\":\"Fixture Positive Released\"}]}"}`
	if response := fixtureJSONRequest(handler, "/v1/responses", featureBody); response.Code != http.StatusOK {
		t.Fatalf("POST /v1/responses status=%d", response.Code)
	}
	var counters struct {
		Search     map[string]int `json:"search"`
		AppDetails map[string]int `json:"appdetails"`
		AI         int            `json:"ai"`
		AIByApp    map[string]int `json:"aiByApp"`
	}
	decodeGET(t, handler, "/__test/catalog-counters", &counters)
	if counters.Search["popularcomingsoon"] != 3 || counters.AppDetails["520001"] != 2 || counters.AI != 2 || counters.AIByApp["520001"] != 2 {
		t.Fatalf("counters = %+v", counters)
	}

	postNoContent(t, handler, "/__test/reset")
	counters = struct {
		Search     map[string]int `json:"search"`
		AppDetails map[string]int `json:"appdetails"`
		AI         int            `json:"ai"`
		AIByApp    map[string]int `json:"aiByApp"`
	}{}
	decodeGET(t, handler, "/__test/catalog-counters", &counters)
	if len(counters.Search) != 0 || len(counters.AppDetails) != 0 || counters.AI != 0 || len(counters.AIByApp) != 0 {
		t.Fatalf("reset counters = %+v", counters)
	}
}

func TestRestartFixtureIntroducesFreshCandidateWhileTailsWait(t *testing.T) {
	handler := newE2EFakeHandlerAt(true, time.Date(2032, time.March, 15, 0, 0, 0, 0, time.UTC))
	postNoContent(t, handler, "/__test/search-mode?mode=restart_tail_rate_limited")
	popular := searchFixtureResponse(t, handler, "popularnew", 0, 200)
	if popular.Total != 5 || !strings.Contains(popular.HTML, `data-ds-appid="520005"`) {
		t.Fatalf("restart popular-new fixture = %+v", popular)
	}
	tail := rawSearchResponse(handler, "recent")
	if tail.Code != http.StatusTooManyRequests || tail.Header().Get("Retry-After") != "7" {
		t.Fatalf("restart tail status=%d retry-after=%q", tail.Code, tail.Header().Get("Retry-After"))
	}
	getOK(t, handler, "/api/appdetails?appids=520005")
}

func TestReconciliationFixtureCompletesAndCountsPages(t *testing.T) {
	handler := newE2EFakeHandler(true)
	first := fixtureRequest(handler, http.MethodGet, appListFixtureURL(0))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"have_more_results":true`) {
		t.Fatalf("first reconciliation page status=%d body=%q", first.Code, first.Body.String())
	}
	second := fixtureRequest(handler, http.MethodGet, appListFixtureURL(424246))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"have_more_results":false`) {
		t.Fatalf("last reconciliation page status=%d body=%q", second.Code, second.Body.String())
	}
	var counters struct {
		AppList int `json:"appList"`
	}
	decodeGET(t, handler, "/__test/catalog-counters", &counters)
	if counters.AppList != 2 {
		t.Fatalf("app list requests=%d", counters.AppList)
	}
}

func TestCatalogFixtureDatesStayRelativeAndSearchMatchesAppDetails(t *testing.T) {
	fixtureDay := time.Date(2032, time.March, 15, 23, 59, 0, 0, time.UTC)
	handler := newE2EFakeHandlerAt(true, fixtureDay)

	for _, tc := range []struct {
		appID  int64
		filter string
		offset int
	}{
		{510001, "popularcomingsoon", 28},
		{520001, "popularnew", -6},
		{520002, "popularnew", -7},
		{520004, "popularnew", -8},
		{530001, "recent", -1},
		{540001, "comingsoon", 2},
	} {
		search := searchFixtureResponse(t, handler, tc.filter, 0, 200)
		wantSearch := fixtureSearchDate(fixtureDay.AddDate(0, 0, tc.offset))
		if !strings.Contains(search.HTML, wantSearch) {
			t.Fatalf("AppID %d search release missing %q: %s", tc.appID, wantSearch, search.HTML)
		}
		response := fixtureRequest(handler, http.MethodGet, "/api/appdetails?appids="+strconv.FormatInt(tc.appID, 10))
		if response.Code != http.StatusOK {
			t.Fatalf("AppID %d appdetails status=%d", tc.appID, response.Code)
		}
		var payload map[string]struct {
			Data struct {
				ReleaseDate struct {
					Date string `json:"date"`
				} `json:"release_date"`
			} `json:"data"`
		}
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		wantStore := fixtureStoreDate(fixtureDay.AddDate(0, 0, tc.offset))
		if got := payload[strconv.FormatInt(tc.appID, 10)].Data.ReleaseDate.Date; got != wantStore {
			t.Fatalf("AppID %d appdetails release=%q, want %q", tc.appID, got, wantStore)
		}
	}
	postNoContent(t, handler, "/__test/search-reviews?mode=changed_expired")
	response := fixtureRequest(handler, http.MethodGet, "/api/appdetails?appids=520002")
	var expired map[string]struct {
		Data struct {
			ReleaseDate struct {
				Date string `json:"date"`
			} `json:"release_date"`
		} `json:"data"`
	}
	if response.Code != http.StatusOK || json.NewDecoder(response.Body).Decode(&expired) != nil || expired["520002"].Data.ReleaseDate.Date != fixtureStoreDate(fixtureDay.AddDate(0, 0, -8)) {
		t.Fatalf("expired appdetails status=%d payload=%+v", response.Code, expired)
	}
}

func TestExpiredReviewModeKeepsPositiveAnchor(t *testing.T) {
	fixtureDay := time.Date(2032, time.March, 15, 0, 0, 0, 0, time.UTC)
	handler := newE2EFakeHandlerAt(true, fixtureDay)
	postNoContent(t, handler, "/__test/search-reviews?mode=expired")
	page := searchFixtureResponse(t, handler, "popularnew", 0, 200)
	if !strings.Contains(page.HTML, "91%") || strings.Contains(page.HTML, "62%") || !strings.Contains(page.HTML, fixtureSearchDate(fixtureDay.AddDate(0, 0, -8))) {
		t.Fatalf("expired-only transition = %s", page.HTML)
	}
}

type searchFixtureEnvelope struct {
	Start    int    `json:"start"`
	PageSize int    `json:"pagesize"`
	Total    int    `json:"total_count"`
	HTML     string `json:"results_html"`
}

func searchFixtureResponse(t *testing.T, handler http.Handler, filter string, start, count int) searchFixtureEnvelope {
	t.Helper()
	response := rawSearchResponseWithPage(handler, filter, start, count)
	if response.Code != http.StatusOK {
		t.Fatalf("search status = %d", response.Code)
	}
	var result searchFixtureEnvelope
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func rawSearchResponse(handler http.Handler, filter string) *httptest.ResponseRecorder {
	return rawSearchResponseWithPage(handler, filter, 0, 200)
}

func rawSearchResponseWithPage(handler http.Handler, filter string, start, count int) *httptest.ResponseRecorder {
	query := url.Values{"filter": {filter}, "category1": {"998"}, "ignore_preferences": {"1"}, "infinite": {"1"}, "start": {strconv.Itoa(start)}, "count": {strconv.Itoa(count)}}
	return fixtureRequest(handler, http.MethodGet, "/search/results/?"+query.Encode())
}

func appListFixtureURL(lastAppID int) string {
	input := url.QueryEscape(`{"include_games":true,"last_appid":` + strconv.Itoa(lastAppID) + `,"max_results":200}`)
	return "/IStoreService/GetAppList/v1/?input_json=" + input
}

func postNoContent(t *testing.T, handler http.Handler, endpoint string) {
	t.Helper()
	response := fixtureRequest(handler, http.MethodPost, endpoint)
	if response.Code != http.StatusNoContent {
		t.Fatalf("POST %s status=%d body=%q", endpoint, response.Code, response.Body.String())
	}
}

func getOK(t *testing.T, handler http.Handler, endpoint string) {
	t.Helper()
	response := fixtureRequest(handler, http.MethodGet, endpoint)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d", endpoint, response.Code)
	}
}

func decodeGET(t *testing.T, handler http.Handler, endpoint string, target any) {
	t.Helper()
	response := fixtureRequest(handler, http.MethodGet, endpoint)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d", endpoint, response.Code)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func fixtureRequest(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func fixtureJSONRequest(handler http.Handler, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type fixtureRoundTripper struct{ handler http.Handler }

func (t fixtureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, request)
	return response.Result(), nil
}
