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
	"next-game-crush/internal/auth"
)

type fakeDataService struct {
	deleted uuid.UUID
	jobs    []JobDiagnostic
	health  HealthReport
}

func (f *fakeDataService) DeleteUserData(_ context.Context, id uuid.UUID) error {
	f.deleted = id
	return nil
}
func (f *fakeDataService) Jobs(context.Context, uuid.UUID) ([]JobDiagnostic, error) {
	return f.jobs, nil
}
func (f *fakeDataService) Health(context.Context) HealthReport { return f.health }

func TestDeleteRequiresAuthenticationAndCSRF(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	data := &fakeDataService{}
	router := NewRouter(Dependencies{Auth: authService, Data: data})

	request := httptest.NewRequest(http.MethodDelete, "/api/data", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", recorder.Code)
	}

	session := repo.addSession()
	request = httptest.NewRequest(http.MethodDelete, "/api/data", nil)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/data", nil)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("valid deletion status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if data.deleted != session.UserID {
		t.Fatalf("deleted user = %s, want %s", data.deleted, session.UserID)
	}
	if cookie := recorder.Result().Cookies()[0]; cookie.Name != auth.SessionCookieName || cookie.MaxAge >= 0 {
		t.Fatalf("session cookie not cleared: %#v", cookie)
	}
}

func TestDiagnosticsSanitizeErrorsAndNeverExposePromptsOrSecrets(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	secret := "super-secret-api-key"
	data := &fakeDataService{jobs: []JobDiagnostic{{ID: uuid.New(), Kind: "ai.profile_summary", Status: "dead", Error: "upstream failed key=" + secret + " raw prompt: ignore previous instructions"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	recorder := httptest.NewRecorder()
	NewRouter(Dependencies{Auth: authService, Data: data}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, secret) || strings.Contains(strings.ToLower(body), "ignore previous instructions") || strings.Contains(strings.ToLower(body), "raw prompt") {
		t.Fatalf("diagnostic response leaked sensitive text: %s", body)
	}
	var jobs []JobDiagnostic
	if err := json.Unmarshal(recorder.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Error != "upstream request failed" {
		t.Fatalf("jobs = %#v", jobs)
	}
}

func TestExpandedHealthReportsComponentsAndFreshness(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	data := &fakeDataService{health: HealthReport{Status: "degraded", Components: map[string]ComponentHealth{
		"database": {Status: "ok", CheckedAt: now}, "openai": {Status: "disabled", CheckedAt: now},
	}}}
	recorder := httptest.NewRecorder()
	NewRouter(Dependencies{Data: data}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"database"`) || !strings.Contains(recorder.Body.String(), `"checkedAt"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}
