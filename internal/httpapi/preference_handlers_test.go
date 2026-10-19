package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/preferences"
)

type fakePreferenceService struct {
	values   []preferences.Adjustment
	accepted preferences.DisableAccepted
	err      error
	user, id uuid.UUID
	inactive bool
}

func (f *fakePreferenceService) List(_ context.Context, user uuid.UUID, inactive bool) ([]preferences.Adjustment, error) {
	f.user = user
	f.inactive = inactive
	return f.values, f.err
}
func (f *fakePreferenceService) Disable(_ context.Context, user, id uuid.UUID) (preferences.DisableAccepted, error) {
	f.user = user
	f.id = id
	return f.accepted, f.err
}
func TestDisablePreferenceSecurityAndErrors(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	id := uuid.New()
	service := &fakePreferenceService{accepted: preferences.DisableAccepted{AdjustmentID: id, RebuildJobID: uuid.New(), PreviousRankingVersion: 7}}
	router := NewRouter(Dependencies{Auth: authService, Preferences: service})
	for _, test := range []struct {
		name, path, csrf, origin string
		authenticated            bool
		err                      error
		want                     int
	}{
		{name: "anonymous", path: id.String(), want: 401},
		{name: "missing csrf", path: id.String(), authenticated: true, want: 403},
		{name: "wrong csrf", path: id.String(), csrf: "wrong", authenticated: true, want: 403},
		{name: "foreign origin", path: id.String(), csrf: session.CSRFToken, origin: "https://foreign.invalid", authenticated: true, want: 403},
		{name: "bad uuid", path: "invalid", csrf: session.CSRFToken, authenticated: true, want: 400},
		{name: "foreign", path: id.String(), csrf: session.CSRFToken, authenticated: true, err: pgx.ErrNoRows, want: 404},
		{name: "storage", path: id.String(), csrf: session.CSRFToken, authenticated: true, err: errors.New("private-feedback-canary"), want: 500},
		{name: "accepted", path: id.String(), csrf: session.CSRFToken, authenticated: true, want: 202},
	} {
		t.Run(test.name, func(t *testing.T) {
			service.err = test.err
			req := httptest.NewRequest(http.MethodDelete, "/api/preference-adjustments/"+test.path, nil)
			if test.authenticated {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			}
			req.Header.Set("X-CSRF-Token", test.csrf)
			req.Header.Set("Origin", test.origin)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != test.want || strings.Contains(res.Body.String(), "private-feedback-canary") {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
			if test.want == 202 {
				var got preferences.DisableAccepted
				if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || got != service.accepted || service.user != session.UserID || service.id != id {
					t.Fatalf("accepted=%#v err=%v", got, err)
				}
			}
		})
	}
}
func TestPreferenceDiagnosticsBoundedAndDefaultReadsOnlyEffectiveActive(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	service := &fakePreferenceService{values: []preferences.Adjustment{{ID: uuid.New(), SourceFeedbackID: uuid.New(), SourceGameID: uuid.New(), Target: preferences.ModeCoop, Dimension: "mode", Confidence: .9, Active: false}}}
	router := NewRouter(Dependencies{Auth: authService, Preferences: service})
	unauthenticated := httptest.NewRecorder()
	router.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/preference-adjustments?includeInactive=true", nil))
	if unauthenticated.Code != 401 {
		t.Fatalf("diagnostics anonymous=%d", unauthenticated.Code)
	}
	for _, test := range []struct {
		query string
		want  int
	}{{"true", 200}, {"false", 200}, {"", 400}, {"yes", 400}, {"1", 400}, {"true&includeInactive=false", 400}} {
		req := httptest.NewRequest(http.MethodGet, "/api/preference-adjustments?includeInactive="+test.query, nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != test.want {
			t.Fatalf("query %s status=%d", test.query, res.Code)
		}
		if test.want == 200 {
			if service.user != session.UserID || service.inactive != (test.query == "true") {
				t.Fatal("wrong ownership/diagnostics")
			}
			if strings.Contains(res.Body.String(), service.values[0].SourceFeedbackID.String()) || strings.Contains(res.Body.String(), service.values[0].SourceGameID.String()) {
				t.Fatal("private provenance exposed")
			}
		}
	}
	defaultReq := httptest.NewRequest(http.MethodGet, "/api/preference-adjustments", nil)
	defaultReq.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	defaultRes := httptest.NewRecorder()
	router.ServeHTTP(defaultRes, defaultReq)
	if defaultRes.Code != http.StatusOK || service.inactive || strings.Contains(defaultRes.Body.String(), `"active":false`) {
		t.Fatalf("default status=%d inactive=%v body=%s", defaultRes.Code, service.inactive, defaultRes.Body.String())
	}
}
