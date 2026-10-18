package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/preferences"
)

type PreferenceAdjustmentService interface {
	List(context.Context, uuid.UUID, bool) ([]preferences.Adjustment, error)
	Disable(context.Context, uuid.UUID, uuid.UUID) (preferences.DisableAccepted, error)
}
type AdjustmentView struct {
	ID         uuid.UUID        `json:"id"`
	Dimension  string           `json:"dimension"`
	Target     preferences.Mode `json:"target"`
	Label      string           `json:"label"`
	Effect     string           `json:"effect"`
	Confidence float64          `json:"confidence"`
	Active     bool             `json:"active"`
	CreatedAt  time.Time        `json:"createdAt"`
}

func adjustmentViews(values []preferences.Adjustment, includeInactive bool) []AdjustmentView {
	result := make([]AdjustmentView, 0, len(values))
	for _, a := range values {
		if !includeInactive && !a.Active {
			continue
		}
		result = append(result, AdjustmentView{ID: a.ID, Dimension: a.Dimension, Target: a.Target, Label: preferences.ModeLabel(a.Target), Effect: "понижаем в рекомендациях", Confidence: a.Confidence, Active: a.Active, CreatedAt: a.CreatedAt})
	}
	return result
}
func getPreferenceAdjustments(service PreferenceAdjustmentService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", 401)
			return
		}
		includeInactive := false
		values, exists := r.URL.Query()["includeInactive"]
		if exists {
			if len(values) != 1 || values[0] != "false" && values[0] != "true" {
				http.Error(w, "invalid preference query", 400)
				return
			}
			includeInactive = values[0] == "true"
		}
		adjustments, err := service.List(r.Context(), session.UserID, includeInactive)
		if err != nil {
			http.Error(w, "preferences unavailable", 500)
			return
		}
		writeJSON(w, adjustmentViews(adjustments, includeInactive))
	}
}
func disablePreferenceAdjustment(service PreferenceAdjustmentService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", 401)
			return
		}
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil || id == uuid.Nil {
			http.Error(w, "invalid preference adjustment", 400)
			return
		}
		accepted, err := service.Disable(r.Context(), session.UserID, id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "preference update unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, accepted)
	}
}
