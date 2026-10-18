package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/recommendations"
)

type overviewHTTPReader struct {
	fakeRecommendationReader
	value         recommendations.Overview
	err           error
	overviewCalls int
}

func (f *overviewHTTPReader) Overview(_ context.Context, owner uuid.UUID) (recommendations.Overview, error) {
	f.userID = owner
	f.overviewCalls++
	return f.value, f.err
}

func TestOverviewRequiresAuthenticatedAllowlistedOwner(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	reader := &overviewHTTPReader{}
	router := NewRouter(Dependencies{Auth: authService, Recommendations: reader})
	path := "/api/recommendations/overview?userId=" + uuid.NewString()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusUnauthorized || reader.overviewCalls != 0 {
		t.Fatalf("unauth=%d calls=%d", response.Code, reader.overviewCalls)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK || reader.userID != session.UserID || reader.overviewCalls != 1 {
		t.Fatalf("authenticated=%d owner=%v calls=%d", response.Code, reader.userID, reader.overviewCalls)
	}
}

func TestOverviewDoesNotResolveAsRecommendationID(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	router := NewRouter(Dependencies{Auth: authService, Recommendations: &overviewHTTPReader{}})
	req := httptest.NewRequest(http.MethodGet, "/api/recommendations/overview", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOverviewJSONContract(t *testing.T) {
	stamp := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"mixed", "discovery-only", "wishlist-only", "empty", "stale"} {
		t.Run(name, func(t *testing.T) {
			items := []recommendations.Recommendation{}
			for i := 0; i < 8; i++ {
				for _, wish := range []float64{0, 1} {
					if name == "empty" || name == "discovery-only" && wish > 0 || name == "wishlist-only" && wish == 0 {
						continue
					}
					items = append(items, recommendations.Recommendation{GameID: uuid.New(), Name: "Game", Factors: recommendations.Factors{Wishlist: wish, CompletionAffinity: .75}, Weights: recommendations.DefaultWeights(), Release: recommendations.ReleaseWindow{Precision: "quarter", Raw: "Q2 2027"}})
				}
			}
			source := recommendations.WishlistSourceState{Availability: "available", LastSuccessfulFetchAt: &stamp}
			if name == "stale" {
				source.Availability = "unavailable"
			}
			value := recommendations.BuildOverview(items, source, catalog.PresentationStatus{Status: "disabled"})
			authService, sessions := handlerService(t, "http://localhost:8080")
			session := sessions.addSession()
			reader := &overviewHTTPReader{value: value}
			router := NewRouter(Dependencies{Auth: authService, Recommendations: reader})
			req := httptest.NewRequest(http.MethodGet, "/api/recommendations/overview", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 4 {
				t.Fatalf("keys=%v", body)
			}
			for _, key := range []string{"discoveries", "wishlist", "calendar", "catalogStatus"} {
				if _, ok := body[key]; !ok {
					t.Fatalf("missing %s", key)
				}
			}
			var wishlist, discovery, status, calendar map[string]json.RawMessage
			for key, dest := range map[string]*map[string]json.RawMessage{"wishlist": &wishlist, "discoveries": &discovery, "catalogStatus": &status, "calendar": &calendar} {
				if err := json.Unmarshal(body[key], dest); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"lastSuccessfulFetchAt", "degradedReason"} {
				if _, ok := wishlist[key]; !ok {
					t.Fatalf("wishlist missing %s", key)
				}
			}
			for _, key := range []string{"lastSuccessfulScanAt", "backfillCompletedAt"} {
				if string(status[key]) != "null" {
					t.Fatalf("coverage timestamp=%s", status[key])
				}
			}
			if _, ok := discovery["featured"]; !ok {
				t.Fatal("missing featured")
			}
			if len(calendar) != 6 {
				t.Fatal("calendar keys")
			}
			for _, key := range []string{"exactDates", "month", "quarter", "year", "comingSoon", "tba"} {
				if len(calendar[key]) == 0 || calendar[key][0] != '[' {
					t.Fatalf("calendar %s=%s", key, calendar[key])
				}
			}
			if name != "empty" {
				var quarter []map[string]json.RawMessage
				if err := json.Unmarshal(calendar["quarter"], &quarter); err != nil || len(quarter) == 0 {
					t.Fatalf("quarter contract=%#v err=%v", quarter, err)
				}
				var factors, weights map[string]float64
				if err := json.Unmarshal(quarter[0]["factors"], &factors); err != nil || factors["completionAffinity"] != .75 {
					t.Fatalf("completion factor=%s decoded=%#v err=%v", quarter[0]["factors"], factors, err)
				}
				if err := json.Unmarshal(quarter[0]["weights"], &weights); err != nil || weights["completionAffinity"] != 5 {
					t.Fatalf("completion weight=%s decoded=%#v err=%v", quarter[0]["weights"], weights, err)
				}
			}
			for _, token := range []string{"cursorAppID", "steamId", "userId", "internal upstream body"} {
				if strings.Contains(response.Body.String(), token) {
					t.Fatalf("leaked %s", token)
				}
			}
			var decoded recommendations.Overview
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			expectedJSON, _ := json.Marshal(value)
			var publicValue recommendations.Overview
			_ = json.Unmarshal(expectedJSON, &publicValue)
			if !reflect.DeepEqual(decoded, publicValue) {
				t.Fatalf("contract changed DTO")
			}
			if name == "mixed" && (decoded.Discoveries.Total != 8 || decoded.Wishlist.Total != 8 || len(decoded.Discoveries.Items) != 6 || len(decoded.Wishlist.Items) != 6 || len(decoded.Calendar.Quarter) != 16) {
				t.Fatal("total or precision truncated")
			}
		})
	}
}

func TestOverviewFailureIsSanitized(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	reader := &overviewHTTPReader{err: errors.New("internal private upstream body")}
	router := NewRouter(Dependencies{Auth: authService, Recommendations: reader})
	req := httptest.NewRequest(http.MethodGet, "/api/recommendations/overview", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError || response.Body.String() != "recommendation overview unavailable\n" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCalendarCompatibilityMatchesOverviewPublishedRanking(t *testing.T) {
	items := []recommendations.Recommendation{{GameID: uuid.New(), Release: recommendations.ReleaseWindow{Precision: "month"}}, {GameID: uuid.New(), Factors: recommendations.Factors{Wishlist: 1}, Release: recommendations.ReleaseWindow{Precision: "day", ExactDate: "2027-03-01"}}}
	reader := &overviewHTTPReader{fakeRecommendationReader: fakeRecommendationReader{items: items}, value: recommendations.BuildOverview(items, recommendations.WishlistSourceState{}, catalog.PresentationStatus{})}
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	router := NewRouter(Dependencies{Auth: authService, Recommendations: reader})
	var calendar recommendations.Calendar
	var overview recommendations.Overview
	for path, dest := range map[string]any{"/api/calendar": &calendar, "/api/recommendations/overview": &overview} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.Code)
		}
		if err := json.Unmarshal(response.Body.Bytes(), dest); err != nil {
			t.Fatal(err)
		}
	}
	want := recommendations.BuildOverview(items, recommendations.WishlistSourceState{}, catalog.PresentationStatus{}).Calendar
	// Legacy empty buckets may be null; compare their normalized complete contents.
	encoded, _ := json.Marshal(calendar)
	var legacy map[string][]recommendations.Recommendation
	_ = json.Unmarshal(encoded, &legacy)
	encoded, _ = json.Marshal(want)
	var full map[string][]recommendations.Recommendation
	_ = json.Unmarshal(encoded, &full)
	for key, entries := range full {
		if len(entries) != len(legacy[key]) {
			t.Fatalf("bucket %s truncated", key)
		}
		for i := range entries {
			if entries[i].GameID != legacy[key][i].GameID {
				t.Fatalf("bucket %s changed", key)
			}
		}
	}
	wantJSON, _ := json.Marshal(want)
	var publicWant recommendations.Calendar
	_ = json.Unmarshal(wantJSON, &publicWant)
	if !reflect.DeepEqual(overview.Calendar, publicWant) {
		t.Fatal("overview calendar changed")
	}
}
