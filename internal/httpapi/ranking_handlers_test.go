package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
)

func TestRankingReadEndpointsRequireAuthenticationAndUseSessionOwner(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	recommendationID := uuid.New()
	profiles := &fakeProfileReader{value: profile.Profile{ID: uuid.New(), Version: 3}}
	recommendationsReader := &fakeRecommendationReader{items: []recommendations.Recommendation{{ID: recommendationID, Name: "Upcoming", Release: recommendations.ReleaseWindow{Precision: "month", Raw: "March 2027"}}}}
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
}

func (f *fakeRecommendationReader) Latest(_ context.Context, userID uuid.UUID) ([]recommendations.Recommendation, error) {
	f.userID = userID
	return f.items, nil
}
func (f *fakeRecommendationReader) Get(_ context.Context, userID, id uuid.UUID) (recommendations.Recommendation, error) {
	f.userID = userID
	item := f.items[0]
	item.ID = id
	return item, nil
}
