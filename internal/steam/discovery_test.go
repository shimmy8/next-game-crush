package steam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetAppListContracts(t *testing.T) {
	since := time.Date(2026, 10, 18, 11, 55, 0, 0, time.UTC)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("wrong operation host"); w.WriteHeader(500) }))
	defer other.Close()
	storeHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("wrong Store metadata host"); w.WriteHeader(500) }))
	defer storeHost.Close()
	body := `{"response":{"apps":[{"appid":101,"name":"Game","last_modified":1792324500},{"appid":102,"last_modified":1792324500}],"last_appid":102,"have_more_results":true}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/IStoreService/GetAppList/v1/" || r.URL.Query().Get("key") != testAPIKey {
			t.Error("path/key contract")
		}
		var input map[string]any
		if json.Unmarshal([]byte(r.URL.Query().Get("input_json")), &input) != nil {
			t.Error("input_json missing")
		}
		if input["include_games"] != true || input["last_appid"] != float64(100) || input["max_results"] != float64(2) {
			t.Error("filters/cursor missing")
		}
		if input["if_modified_since"] != float64(since.Unix()) {
			t.Error("watermark mismatch")
		}
		for _, flag := range []string{"include_dlc", "include_software", "include_videos", "include_hardware"} {
			if input[flag] != false {
				t.Error("product flags")
			}
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	c, err := NewClient(HTTPConfig{APIKey: testAPIKey, WebAPIBaseURL: other.URL, StoreBaseURL: storeHost.URL, StoreServiceBaseURL: server.URL, MinRequestInterval: -1, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ListChangedGames(context.Background(), since, 100, 2)
	if err != nil || len(page.Apps) != 2 || page.NextAppID != 102 || !page.HasMore {
		t.Fatalf("page contract: %+v %v", page, err)
	}
	for _, invalid := range []string{`{}`, `{"response":{}}`, `{"response":{"apps":[],"have_more_results":true,"last_appid":100}}`, `{"response":{"apps":[],"have_more_results":"false"}}`} {
		body = invalid
		if _, err = c.ListChangedGames(context.Background(), since, 100, 2); err == nil {
			t.Fatalf("accepted invalid page")
		}
	}
	body = `{"response":{"apps":[],"have_more_results":false,"last_appid":100}}`
	if _, err = c.ListChangedGames(context.Background(), since, 100, 2); err != nil {
		t.Fatal(err)
	}
}

func TestGameDetailsBlankComingSoonDate(t *testing.T) {
	c := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"42":{"success":true,"data":{"type":"game","steam_appid":42,"name":"Game","required_age":0,"platforms":{"windows":true,"mac":false,"linux":false},"release_date":{"coming_soon":true,"date":""}}}}`))
	}), HTTPConfig{})
	if _, err := c.GameDetails(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
}

func TestGetAppListInvalidItemsAndBackfill(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		_ = json.Unmarshal([]byte(r.URL.Query().Get("input_json")), &input)
		if _, ok := input["if_modified_since"]; ok {
			t.Error("zero watermark sent")
		}
		_, _ = w.Write([]byte(`{"response":{"apps":[{"appid":1,"last_modified":1792324500},{"appid":4294967296,"last_modified":1792324500},{"appid":2,"last_modified":"bad"},{"appid":3,"last_modified":4294967296}],"have_more_results":false,"last_appid":3}}`))
	}))
	defer server.Close()
	c, err := NewClient(HTTPConfig{APIKey: testAPIKey, StoreServiceBaseURL: server.URL, MinRequestInterval: -1, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ListChangedGames(context.Background(), time.Time{}, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Apps) != 4 || page.Apps[0].AppID != 1 {
		t.Fatal("valid neighbor lost")
	}
	for _, app := range page.Apps[1:] {
		if app.AppID != 0 || app.Name != "" || !app.LastModified.IsZero() {
			t.Fatal("invalid item retained raw input")
		}
	}
	for _, tc := range []struct {
		since time.Time
		limit int
	}{{time.Time{}, 0}, {time.Time{}, 201}, {time.Unix(-1, 0), 1}, {time.Unix(4294967296, 0), 1}} {
		if _, err := c.ListChangedGames(context.Background(), tc.since, 0, tc.limit); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
}

func TestGetAppListTransportFailuresAreBoundedAndSafe(t *testing.T) {
	for _, kind := range []string{"rate_limit", "oversize", "cancel", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			forwarded := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded = true
				_, _ = w.Write([]byte(`{"response":{"apps":[],"have_more_results":false}}`))
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "rate_limit":
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(429)
					_, _ = w.Write([]byte(testAPIKey))
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat(testAPIKey, 100)))
				case "redirect":
					http.Redirect(w, r, target.URL+"?key="+testAPIKey, http.StatusFound)
				case "cancel":
					<-r.Context().Done()
				}
			}))
			defer source.Close()
			c, err := NewClient(HTTPConfig{APIKey: testAPIKey, HTTPClient: source.Client(), StoreServiceBaseURL: source.URL, RequestTimeout: 50 * time.Millisecond, MaxResponseBytes: 64, MaxRetries: 1, MinRequestInterval: -1})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			_, err = c.ListChangedGames(ctx, time.Time{}, 0, 1)
			if err == nil {
				t.Fatal("failed transport accepted")
			}
			if forwarded {
				t.Fatal("Steam key followed redirect")
			}
			if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), source.URL) {
				t.Fatal("unsafe error")
			}
			var requestErr *RequestError
			if !errors.As(err, &requestErr) || requestErr.Operation != AppListOperation {
				t.Fatal("untyped operation failure")
			}
			if kind == "rate_limit" && (requestErr.RetryAfter != time.Second || !requestErr.Temporary) {
				t.Fatal("Retry-After lost")
			}
			if kind == "oversize" && !errors.Is(err, ErrResponseTooLarge) {
				t.Fatal("size limit lost")
			}
		})
	}
}
