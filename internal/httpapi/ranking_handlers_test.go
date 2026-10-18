package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
)

func TestRankingReadEndpointsRequireAuthenticationAndUseSessionOwner(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	recommendationID := uuid.New()
	profiles := &fakeProfileReader{value: profile.Profile{ID: uuid.New(), Version: 3}}
	completionText := "Есть сходство с пройденной вами игрой SOMA."
	positivePercent := 91
	recommendationsReader := &fakeRecommendationReader{items: []recommendations.Recommendation{{ID: recommendationID, Name: "Upcoming", Platforms: &recommendations.Platforms{Windows: true, Linux: true}, DeckCompatibility: recommendations.DeckVerified, Factors: recommendations.Factors{CompletionAffinity: .5}, Weights: recommendations.DefaultWeights(), Reviews: &recommendations.ReviewSummary{Band: recommendations.ReviewPositive, Label: "Very Positive", PositivePercent: &positivePercent, Total: 1284, ObservedAt: time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC)}, Release: recommendations.ReleaseWindow{Precision: "month", Raw: "March 2027"}, Explanation: completionText, ExplanationSource: recommendations.ExplanationSourceDeterministic, Evidence: []recommendations.Evidence{{ID: "completion-anchor-1", Kind: recommendations.EvidenceCompletionAnchor, Text: completionText}}}}}
	router := NewRouter(Dependencies{Auth: authService, Profile: profiles, Recommendations: recommendationsReader})

	for _, path := range []string{"/api/profile", "/api/calendar", "/api/recommendations/" + recommendationID.String()} {
		unauthorized := httptest.NewRecorder()
		router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, path, nil))
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d, want 401", path, unauthorized.Code)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		if path != "/api/profile" {
			var body map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if path == "/api/calendar" {
				var month []map[string]json.RawMessage
				if err := json.Unmarshal(body["month"], &month); err != nil || len(month) != 1 {
					t.Fatalf("month=%#v err=%v", month, err)
				}
				body = month[0]
			}
			var platforms map[string]bool
			var deck string
			if err := json.Unmarshal(body["platforms"], &platforms); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body["deckCompatibility"], &deck); err != nil {
				t.Fatal(err)
			}
			_, macPresent := platforms["mac"]
			if len(platforms) != 3 || !platforms["windows"] || !macPresent || platforms["mac"] || !platforms["linux"] || deck != "verified" {
				t.Fatalf("%s compatibility=%#v/%q", path, platforms, deck)
			}
			var evidence []recommendations.Evidence
			if err := json.Unmarshal(body["evidence"], &evidence); err != nil || len(evidence) != 1 || evidence[0].Kind != recommendations.EvidenceCompletionAnchor || evidence[0].Text != completionText {
				t.Fatalf("%s completion evidence=%#v err=%v", path, evidence, err)
			}
			var factors, weights map[string]float64
			if err := json.Unmarshal(body["factors"], &factors); err != nil || factors["completionAffinity"] != .5 {
				t.Fatalf("%s completion factor=%#v err=%v", path, factors, err)
			}
			if err := json.Unmarshal(body["weights"], &weights); err != nil || weights["completionAffinity"] != 5 {
				t.Fatalf("%s completion weight=%#v err=%v", path, weights, err)
			}
			var reviews map[string]json.RawMessage
			if err := json.Unmarshal(body["reviews"], &reviews); err != nil {
				t.Fatalf("%s reviews=%s err=%v", path, body["reviews"], err)
			}
			if len(reviews) != 5 || string(reviews["band"]) != `"positive"` || string(reviews["label"]) != `"Very Positive"` || string(reviews["positivePercent"]) != "91" || string(reviews["total"]) != "1284" || string(reviews["observedAt"]) != `"2026-10-18T12:34:56Z"` {
				t.Fatalf("%s review contract=%s", path, body["reviews"])
			}
			for _, forbidden := range []string{"rawHtml", "html", "tooltip"} {
				if strings.Contains(strings.ToLower(string(body["reviews"])), strings.ToLower(forbidden)) {
					t.Fatalf("%s review leaked %s: %s", path, forbidden, body["reviews"])
				}
			}
		}
	}
	if profiles.userID != session.UserID || recommendationsReader.userID != session.UserID {
		t.Fatal("read endpoint did not scope query to authenticated user")
	}

	calendarReq := httptest.NewRequest(http.MethodGet, "/api/calendar", nil)
	calendarReq.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	calendarResponse := httptest.NewRecorder()
	router.ServeHTTP(calendarResponse, calendarReq)
	var calendar recommendations.Calendar
	if err := json.NewDecoder(calendarResponse.Body).Decode(&calendar); err != nil || len(calendar.Month) != 1 || len(calendar.ExactDates) != 0 {
		t.Fatalf("calendar=%#v err=%v", calendar, err)
	}
}

func TestIneligibleRecommendationDetailReturnsNotFound(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	recommendationID := uuid.New()
	router := NewRouter(Dependencies{Auth: authService, Recommendations: &fakeRecommendationReader{getErr: pgx.ErrNoRows}})
	req := httptest.NewRequest(http.MethodGet, "/api/recommendations/"+recommendationID.String(), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestProfileResponseExposesSummaryAvailability(t *testing.T) {
	for _, test := range []struct {
		name, summary, status string
	}{
		{name: "unavailable", status: "unavailable"},
		{name: "ready", summary: "Любит вдумчивые кооперативные игры.", status: "ready"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService, sessions := handlerService(t, "http://localhost:8080")
			session := sessions.addSession()
			profiles := &fakeProfileReader{value: profile.Profile{ID: uuid.New(), Version: 3, Summary: test.summary}}
			router := NewRouter(Dependencies{Auth: authService, Profile: profiles})
			req := httptest.NewRequest(http.MethodGet, "/api/profile", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			response := httptest.NewRecorder()

			router.ServeHTTP(response, req)

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var got struct {
				Summary       string `json:"summary"`
				SummaryStatus string `json:"summaryStatus"`
			}
			if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Summary != test.summary || got.SummaryStatus != test.status {
				t.Fatalf("profile summary = %q/%q, want %q/%q", got.Summary, got.SummaryStatus, test.summary, test.status)
			}
		})
	}
}

type fakeProfileReader struct {
	value  profile.Profile
	userID uuid.UUID
}

func (f *fakeProfileReader) Latest(_ context.Context, userID uuid.UUID) (profile.Profile, error) {
	f.userID = userID
	return f.value, nil
}

type fakeRecommendationReader struct {
	items  []recommendations.Recommendation
	userID uuid.UUID
	getErr error
}

func (f *fakeRecommendationReader) Latest(_ context.Context, userID uuid.UUID) ([]recommendations.Recommendation, error) {
	f.userID = userID
	return f.items, nil
}
func (f *fakeRecommendationReader) Overview(_ context.Context, userID uuid.UUID) (recommendations.Overview, error) {
	f.userID = userID
	return recommendations.BuildOverview(f.items, recommendations.WishlistSourceState{}, catalog.PresentationStatus{Status: "disabled"}), nil
}
func (f *fakeRecommendationReader) Get(_ context.Context, userID, id uuid.UUID) (recommendations.Recommendation, error) {
	f.userID = userID
	if f.getErr != nil {
		return recommendations.Recommendation{}, f.getErr
	}
	item := f.items[0]
	item.ID = id
	return item, nil
}
