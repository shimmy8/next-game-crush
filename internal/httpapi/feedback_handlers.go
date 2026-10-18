package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/feedback"
)

type FeedbackService interface {
	Record(context.Context, feedback.Command) (feedback.Accepted, error)
	Diff(context.Context, uuid.UUID, int64, uuid.UUID) (feedback.RankingDiff, error)
}

type feedbackRequest struct {
	Action         string          `json:"action"`
	Reason         json.RawMessage `json:"reason"`
	Reasons        json.RawMessage `json:"reasons"`
	FreeText       string          `json:"freeText"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

func recordFeedback(service FeedbackService, reminder bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		recommendationID, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "invalid recommendation ID", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input feedbackRequest
		if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid feedback request", http.StatusBadRequest)
			return
		}
		if input.Reason != nil && input.Reasons != nil {
			http.Error(w, "invalid feedback request", http.StatusBadRequest)
			return
		}
		var reason string
		if input.Reason != nil && strings.TrimSpace(string(input.Reason)) != "null" {
			if json.Unmarshal(input.Reason, &reason) != nil || strings.TrimSpace(reason) == "" {
				http.Error(w, "invalid feedback request", http.StatusBadRequest)
				return
			}
		}
		var reasons []string
		if input.Reasons != nil && strings.TrimSpace(string(input.Reasons)) != "null" {
			if json.Unmarshal(input.Reasons, &reasons) != nil {
				http.Error(w, "invalid feedback request", http.StatusBadRequest)
				return
			}
		}
		if reminder {
			input.Action = feedback.ActionRemind
			reason = ""
			reasons = nil
			input.FreeText = ""
		}
		accepted, err := service.Record(r.Context(), feedback.Command{UserID: session.UserID, RecommendationID: recommendationID, Action: input.Action, Reason: reason, Reasons: reasons, FreeText: input.FreeText, IdempotencyKey: input.IdempotencyKey})
		switch {
		case errors.Is(err, feedback.ErrInvalidCommand):
			http.Error(w, "invalid feedback", http.StatusBadRequest)
			return
		case errors.Is(err, feedback.ErrIdempotencyConflict):
			http.Error(w, "idempotency key conflict", http.StatusConflict)
			return
		case errors.Is(err, pgx.ErrNoRows):
			http.NotFound(w, r)
			return
		case err != nil:
			http.Error(w, "feedback unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, accepted)
	}
}

func rankingDiff(service FeedbackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		version, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "version")), 10, 64)
		if err != nil || version < 1 {
			http.Error(w, "invalid ranking version", http.StatusBadRequest)
			return
		}
		var jobID uuid.UUID
		if raw := strings.TrimSpace(r.URL.Query().Get("jobId")); raw != "" {
			jobID, err = uuid.Parse(raw)
			if err != nil {
				http.Error(w, "invalid rebuild job ID", http.StatusBadRequest)
				return
			}
		}
		diff, err := service.Diff(r.Context(), session.UserID, version, jobID)
		if errors.Is(err, feedback.ErrInvalidCommand) {
			http.Error(w, "invalid ranking version", http.StatusBadRequest)
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "ranking diff unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, diff)
	}
}
