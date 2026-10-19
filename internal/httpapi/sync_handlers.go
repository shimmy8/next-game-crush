package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/observability"
	syncservice "next-game-crush/internal/sync"
)

type SyncService interface {
	Preflight(context.Context, uuid.UUID) (syncservice.PreflightResult, error)
	Start(context.Context, uuid.UUID) (uuid.UUID, error)
	RefreshIfStale(context.Context, uuid.UUID) (syncservice.RefreshDecision, error)
	GetStatus(context.Context, uuid.UUID, uuid.UUID) (syncservice.Status, error)
	GetCurrentStatus(context.Context, uuid.UUID) (syncservice.Status, bool, error)
}

func refreshSyncIfStale(service SyncService, logger *observability.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		started := time.Now()
		decision, err := service.RefreshIfStale(r.Context(), session.UserID)
		if err != nil {
			status, reason, message := syncRequestFailure(err)
			logger.SyncRequest(r.Context(), observability.SyncRequestEvent{Outcome: observability.RequestFailed, RequestID: requestIDFromContext(r.Context()), HTTPStatus: status, Duration: time.Since(started), Reason: reason})
			http.Error(w, message, status)
			return
		}
		status := http.StatusOK
		workflowID := uuid.Nil
		if decision.JobID != nil {
			workflowID = *decision.JobID
			status = http.StatusAccepted
		}
		logger.SyncRequest(r.Context(), observability.SyncRequestEvent{Outcome: observability.RequestAccepted, RequestID: requestIDFromContext(r.Context()), WorkflowID: workflowID, HTTPStatus: status, Duration: time.Since(started), Reason: refreshReason(decision.Reason)})
		writeJSONStatus(w, status, decision)
	}
}

func refreshReason(reason syncservice.RefreshReason) observability.Reason {
	switch reason {
	case syncservice.RefreshReasonMissing:
		return observability.ReasonRefreshMissing
	case syncservice.RefreshReasonStale:
		return observability.ReasonRefreshStale
	case syncservice.RefreshReasonFresh:
		return observability.ReasonRefreshFresh
	case syncservice.RefreshReasonActive:
		return observability.ReasonRefreshActive
	default:
		return observability.ReasonInternalError
	}
}

func syncPreflight(service SyncService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		result, err := service.Preflight(r.Context(), session.UserID)
		if errors.Is(err, syncservice.ErrPreflightFailed) {
			writeJSONStatus(w, http.StatusBadGateway, map[string]string{"reason": "check_failed"})
			return
		}
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"reason": "internal_error"})
			return
		}
		writeJSON(w, result)
	}
}

func startSync(service SyncService, logger *observability.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		started := time.Now()
		jobID, err := service.Start(r.Context(), session.UserID)
		if err != nil {
			status, reason, message := syncRequestFailure(err)
			logger.SyncRequest(r.Context(), observability.SyncRequestEvent{Outcome: observability.RequestFailed, RequestID: requestIDFromContext(r.Context()), HTTPStatus: status, Duration: time.Since(started), Reason: reason})
			http.Error(w, message, status)
			return
		}
		logger.SyncRequest(r.Context(), observability.SyncRequestEvent{Outcome: observability.RequestAccepted, RequestID: requestIDFromContext(r.Context()), WorkflowID: jobID, HTTPStatus: http.StatusAccepted, Duration: time.Since(started)})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"jobId": jobID})
	}
}

func syncRequestFailure(err error) (int, observability.Reason, string) {
	switch {
	case errors.Is(err, syncservice.ErrPreflightBlocked):
		return http.StatusConflict, observability.ReasonUpstreamUnavailable, "synchronization blocked"
	case errors.Is(err, syncservice.ErrPreflightFailed):
		return http.StatusBadGateway, observability.ReasonUpstreamUnavailable, "synchronization unavailable"
	default:
		return http.StatusInternalServerError, observability.ReasonInternalError, "synchronization unavailable"
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func syncStatus(service SyncService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		jobID, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "invalid synchronization ID", http.StatusBadRequest)
			return
		}
		status, err := service.GetStatus(r.Context(), session.UserID, jobID)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "synchronization status unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	}
}

func currentSyncStatus(service SyncService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		status, found, err := service.GetCurrentStatus(r.Context(), session.UserID)
		if err != nil {
			http.Error(w, "synchronization status unavailable", http.StatusInternalServerError)
			return
		}
		if !found {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, status)
	}
}
