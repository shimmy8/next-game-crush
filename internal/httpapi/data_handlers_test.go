package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/catalog"
)

type fakeSearchDiagnosticsReader struct {
	value catalog.SearchDiagnostics
	err   error
}

func (f fakeSearchDiagnosticsReader) Diagnostics(context.Context) (catalog.SearchDiagnostics, error) {
	return f.value, f.err
}

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

func TestDiagnosticsExposeAggregateSearchStateOnly(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	service := NewSQLDataService(nil, false, false)
	service.SetSearchDiagnosticsReader(fakeSearchDiagnosticsReader{value: catalog.SearchDiagnostics{
		Status:    catalog.SearchDiagnosticsReady,
		Feeds:     map[string]catalog.SearchFeedDiagnostic{"popular_upcoming": {Status: "complete", Pages: 2, Discovered: 8, Deduplicated: 7}},
		Lanes:     catalog.SearchLaneDiagnostics{Catalog: catalog.LaneDiagnostic{Queued: 3}},
		Budgets:   catalog.SearchBudgetDiagnostics{Store: catalog.StoreBudgetDiagnostic{BudgetDiagnostic: catalog.BudgetDiagnostic{Used: 9, Remaining: 991, Limit: 1000}}},
		Throttles: map[string]catalog.ThrottleDiagnostic{"store": {Concurrency: 4}},
		FreshAt:   &now,
	}})
	// Health normally also reads component freshness. Use a fake DataService at
	// the HTTP boundary to pin the additive, privacy-safe JSON contract.
	report := HealthReport{Status: "ok", Components: map[string]ComponentHealth{}, CatalogSearch: func() *catalog.SearchDiagnostics {
		value, err := service.searchDiagnostics.Diagnostics(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return &value
	}()}
	recorder := httptest.NewRecorder()
	NewRouter(Dependencies{Data: &fakeDataService{health: report}}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"catalogSearch"`, `"popular_upcoming"`, `"remaining":991`, `"concurrency":4`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s: %s", want, body)
		}
	}
	for _, forbidden := range []string{"steamAppId", "appId", "payload", "rawBody", "gameName"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("leaked %q: %s", forbidden, body)
		}
	}
}

func TestSearchDiagnosticsFailureReturnsCompleteSafeFallback(t *testing.T) {
	db := openHTTPTestStore(t)
	service := NewSQLDataService(db, false, false)
	service.SetSearchDiagnosticsReader(fakeSearchDiagnosticsReader{err: errors.New("database detail that must not escape")})
	report := service.Health(context.Background())
	if report.Status != "degraded" || report.CatalogSearch == nil || report.CatalogSearch.Status != catalog.SearchDiagnosticsUnavailable {
		t.Fatalf("report=%+v", report)
	}
	if len(report.CatalogSearch.Feeds) != len(catalog.SearchDiagnosticFeeds) || len(report.CatalogSearch.Throttles) != 3 {
		t.Fatalf("incomplete fallback=%+v", report.CatalogSearch)
	}
	encoded, err := json.Marshal(report.CatalogSearch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "database detail") || !strings.Contains(string(encoded), `"budgets"`) || !strings.Contains(string(encoded), `"lanes"`) {
		t.Fatalf("unsafe fallback=%s", encoded)
	}
}
