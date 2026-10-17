package httpapi

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/notifications"
)

func emailFeedbackAction(service DigestService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		already, err := service.ConsumeAction(r.Context(), chi.URLParam(r, "token"))
		switch {
		case errors.Is(err, notifications.ErrExpired):
			http.Error(w, "This feedback link has expired.", http.StatusGone)
			return
		case errors.Is(err, notifications.ErrInvalidToken):
			http.Error(w, "This feedback link is invalid.", http.StatusBadRequest)
			return
		case errors.Is(err, pgx.ErrNoRows):
			http.NotFound(w, r)
			return
		case err != nil:
			http.Error(w, "Feedback is temporarily unavailable.", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		message := "Thanks — your feedback was saved."
		if already {
			message = "Thanks — this feedback was already saved."
		}
		_ = template.Must(template.New("result").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Next Game Crush</title></head><body><main><h1>Next Game Crush</h1><p>{{.}}</p><p>You can close this window.</p></main></body></html>`)).Execute(w, message)
	}
}
