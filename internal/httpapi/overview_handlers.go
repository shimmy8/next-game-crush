package httpapi

import (
	"net/http"

	"next-game-crush/internal/auth"
)

func getRecommendationOverview(service RecommendationReadService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		value, err := service.Overview(r.Context(), session.UserID)
		if err != nil {
			http.Error(w, "recommendation overview unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, value)
	}
}
