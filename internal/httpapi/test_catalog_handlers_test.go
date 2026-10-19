package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"next-game-crush/internal/auth"
)

type testCatalogStub struct {
	runs     []string
	restarts int
	state    any
	err      error
}

func (s *testCatalogStub) StartTestCatalogRun(_ context.Context, kind string) (string, error) {
	s.runs = append(s.runs, kind)
	return "run-1", s.err
}
func (s *testCatalogStub) TestCatalogRun(context.Context, string) (any, error) {
	return s.state, s.err
}
func (s *testCatalogStub) RestartTestCatalogRuntime(context.Context) error {
	s.restarts++
	return s.err
}

func TestCatalogHarnessRoutesFailClosedAndRequireOwnerCSRF(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	owner := sessions.addSession()
	withoutHarness := NewRouter(Dependencies{Auth: authService})
	missing := httptest.NewRecorder()
	withoutHarness.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/test/catalog/runs", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("disabled harness status=%d, want 404", missing.Code)
	}

	service := &testCatalogStub{state: map[string]any{"status": "succeeded"}}
	router := NewRouter(Dependencies{Auth: authService, TestCatalog: service})
	for _, test := range []struct {
		name  string
		path  string
		setup func(*http.Request)
		want  int
	}{
		{name: "anonymous", path: "/api/test/catalog/runs", setup: func(*http.Request) {}, want: http.StatusUnauthorized},
		{name: "missing csrf", path: "/api/test/catalog/runs", setup: func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: owner.Token})
		}, want: http.StatusForbidden},
		{name: "owner", path: "/api/test/catalog/runs", setup: func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: owner.Token})
			r.Header.Set("X-CSRF-Token", owner.CSRFToken)
		}, want: http.StatusAccepted},
		{name: "restart", path: "/api/test/catalog/runtime/restart", setup: func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: owner.Token})
			r.Header.Set("X-CSRF-Token", owner.CSRFToken)
		}, want: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(`{"kind":"manual"}`))
			test.setup(req)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != test.want {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if len(service.runs) != 1 || service.runs[0] != "manual" || service.restarts != 1 {
		t.Fatalf("runs=%v restarts=%d", service.runs, service.restarts)
	}
}

func TestCatalogHarnessRunBodyIsStrictAndKindIsAllowlisted(t *testing.T) {
	service := &testCatalogStub{}
	handler := startTestCatalogRun(service)
	for _, test := range []struct {
		name, body, kind string
		want             int
	}{
		{name: "empty defaults manual", body: "", kind: "manual", want: http.StatusAccepted},
		{name: "object defaults manual", body: `{}`, kind: "manual", want: http.StatusAccepted},
		{name: "reconciliation", body: `{"kind":"reconciliation"}`, kind: "reconciliation", want: http.StatusAccepted},
		{name: "unknown kind", body: `{"kind":"full"}`, want: http.StatusBadRequest},
		{name: "unknown field", body: `{"kind":"manual","secret":"no"}`, want: http.StatusBadRequest},
		{name: "trailing object", body: `{} {}`, want: http.StatusBadRequest},
		{name: "malformed", body: `{`, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(service.runs)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/test/catalog/runs", bytes.NewBufferString(test.body)))
			if recorder.Code != test.want {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if test.want == http.StatusAccepted && (len(service.runs) != before+1 || service.runs[before] != test.kind) {
				t.Fatalf("runs=%v", service.runs)
			}
			if test.want != http.StatusAccepted && len(service.runs) != before {
				t.Fatalf("invalid request reached service: %v", service.runs)
			}
		})
	}
}

func TestCatalogHarnessErrorsAreSanitized(t *testing.T) {
	service := &testCatalogStub{err: errors.New("private catalog canary")}
	for _, handler := range []http.Handler{
		startTestCatalogRun(service),
		getTestCatalogRun(service),
		restartTestCatalogRuntime(service),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`)))
		if bytes.Contains(recorder.Body.Bytes(), []byte("private catalog canary")) {
			t.Fatalf("private error leaked: %s", recorder.Body.String())
		}
	}
}
