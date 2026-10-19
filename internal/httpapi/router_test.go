package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
)

func TestRouterReservesServerRoutesFromSPAFallback(t *testing.T) {
	router := NewRouter(Dependencies{Frontend: fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>Next Game Crush</title>")},
	}})
	for _, path := range []string{"/actions", "/actions/", "/actions/feedback/invalid-token", "/api/missing", "/auth/missing"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 for unhandled server route", recorder.Code)
			}
		})
	}
	for _, path := range []string{"/", "/calendar", "/actions-calendar"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 for frontend route", recorder.Code)
			}
		})
	}
}

func TestDemoSessionIsForbiddenFromEveryBrowserMutation(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	session.AccessMode = auth.AccessModeDemo
	sessions.sessions[session.ID] = session
	router := NewRouter(Dependencies{
		Auth: authService, Sync: &fakeSyncService{jobID: uuid.New()}, Preferences: &fakePreferenceService{},
		Feedback: &fakeFeedbackService{}, Digests: &digestStub{}, Data: &fakeDataService{},
	})
	id := uuid.NewString()
	for _, test := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/sync/preflight", ""},
		{http.MethodPost, "/api/sync", ""},
		{http.MethodPost, "/api/sync/refresh-if-stale", ""},
		{http.MethodDelete, "/api/preference-adjustments/" + id, ""},
		{http.MethodPost, "/api/recommendations/" + id + "/feedback", `{"action":"like","idempotencyKey":"demo"}`},
		{http.MethodPost, "/api/recommendations/" + id + "/reminder", `{"idempotencyKey":"demo"}`},
		{http.MethodPost, "/api/digests/test", ""},
		{http.MethodDelete, "/api/data", ""},
	} {
		req := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		req.Header.Set("X-CSRF-Token", session.CSRFToken)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s status=%d body=%s", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
}
