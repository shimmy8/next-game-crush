package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

type TestCatalogService interface {
	StartTestCatalogRun(context.Context, string) (string, error)
	TestCatalogRun(context.Context, string) (any, error)
	RestartTestCatalogRuntime(context.Context) error
}

func startTestCatalogRun(service TestCatalogService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind string `json:"kind"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body.Kind = strings.TrimSpace(body.Kind)
		if body.Kind == "" {
			body.Kind = "manual"
		}
		if body.Kind != "manual" && body.Kind != "reconciliation" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		id, err := service.StartTestCatalogRun(r.Context(), body.Kind)
		if err != nil {
			http.Error(w, "catalog run unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"runId": id})
	}
}

func getTestCatalogRun(service TestCatalogService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, err := service.TestCatalogRun(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, value)
	}
}

func restartTestCatalogRuntime(service TestCatalogService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := service.RestartTestCatalogRuntime(r.Context()); err != nil {
			http.Error(w, "catalog runtime unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "restarted"})
	}
}
