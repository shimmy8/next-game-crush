package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/recommendations"
)

type RecommendationReadService interface {
	Overview(context.Context, uuid.UUID) (recommendations.Overview, error)
	Latest(context.Context, uuid.UUID) ([]recommendations.Recommendation, error)
	Get(context.Context, uuid.UUID, uuid.UUID) (recommendations.Recommendation, error)
}

func getCalendar(service RecommendationReadService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		items, err := service.Latest(r.Context(), session.UserID)
		if err != nil {
			http.Error(w, "calendar unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, recommendations.BuildCalendar(items))
	}
}
func getRecommendation(service RecommendationReadService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "invalid recommendation ID", http.StatusBadRequest)
			return
		}
		item, err := service.Get(r.Context(), session.UserID, id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "recommendation unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, item)
	}
}
