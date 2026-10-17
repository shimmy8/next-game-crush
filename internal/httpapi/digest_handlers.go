package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
)

type DigestService interface {
	SendTestDigest(context.Context, uuid.UUID) (uuid.UUID, error)
	ConsumeAction(context.Context, string) (bool, error)
}

func sendTestDigest(service DigestService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		deliveryID, err := service.SendTestDigest(r.Context(), session.UserID)
		if err != nil {
			http.Error(w, "digest unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"deliveryId": deliveryID, "status": "pending"})
	}
}
