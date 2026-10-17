package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
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
