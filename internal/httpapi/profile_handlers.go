package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/profile"
)

type ProfileReadService interface {
	Latest(context.Context, uuid.UUID) (profile.Profile, error)
}

func getProfile(service ProfileReadService, preferenceServices ...PreferenceAdjustmentService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		value, err := service.Latest(r.Context(), session.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "profile unavailable", http.StatusInternalServerError)
			return
		}
		adjustments := []AdjustmentView{}
		if len(preferenceServices) > 0 && preferenceServices[0] != nil {
			values, err := preferenceServices[0].List(r.Context(), session.UserID, false)
			if err != nil {
				http.Error(w, "profile unavailable", 500)
				return
			}
			adjustments = adjustmentViews(values, false)
		}
		writeJSON(w, struct {
			profile.Profile
			Adjustments []AdjustmentView `json:"adjustments"`
		}{profile.WithSummaryState(value), adjustments})
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
