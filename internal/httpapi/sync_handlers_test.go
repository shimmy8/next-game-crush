package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/observability"
	syncservice "next-game-crush/internal/sync"
)

func TestSyncRequestLogAcceptedUsesGeneratedCorrelation(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	jobID := uuid.New()
	var logs bytes.Buffer
	router := NewRouter(Dependencies{Auth: authService, Sync: &fakeSyncService{jobID: jobID}, Observability: observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))})
	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	req.Header.Set("X-Request-ID", "hostile-request-id-canary")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	records := decodeLogRecords(t, logs.String())
	if len(records) != 1 {
		t.Fatalf("records=%#v", records)
	}
	record := records[0]
	if record["event"] != "sync_request" || record["outcome"] != "accepted" || record["workflow_id"] != jobID.String() || record["http_status"] != float64(http.StatusAccepted) {
		t.Fatalf("record=%#v", record)
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "event": true, "outcome": true, "request_id": true, "workflow_id": true, "http_status": true, "duration_ms": true}
	for key := range record {
		if !allowed[key] {
			t.Fatalf("unexpected sync_request field %q in %#v", key, record)
		}
	}
	requestID, _ := record["request_id"].(string)
	if requestID == "" || requestID != recorder.Header().Get("X-Request-ID") || requestID == "hostile-request-id-canary" {
		t.Fatalf("request ID=%q response=%q", requestID, recorder.Header().Get("X-Request-ID"))
	}
	if strings.Contains(logs.String(), "hostile-request-id-canary") || strings.Contains(logs.String(), session.UserID.String()) {
		t.Fatalf("sensitive value leaked: %s", logs.String())
	}
}

func TestSyncRequestLogFailuresAreBounded(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{name: "blocked", err: fmt.Errorf("%w: private-canary", syncservice.ErrPreflightBlocked), status: http.StatusConflict, reason: "upstream_unavailable"},
		{name: "upstream", err: fmt.Errorf("%w: upstream-canary", syncservice.ErrPreflightFailed), status: http.StatusBadGateway, reason: "upstream_unavailable"},
		{name: "internal", err: errors.New("database-canary"), status: http.StatusInternalServerError, reason: "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService, repo := handlerService(t, "http://localhost:8080")
			session := repo.addSession()
			var logs bytes.Buffer
			router := NewRouter(Dependencies{Auth: authService, Sync: &fakeSyncService{startErr: test.err}, Observability: observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))})
			req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != test.status {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			records := decodeLogRecords(t, logs.String())
			if len(records) != 1 || records[0]["outcome"] != "failed" || records[0]["http_status"] != float64(test.status) || records[0]["reason"] != test.reason {
				t.Fatalf("records=%#v", records)
			}
			if _, ok := records[0]["workflow_id"]; ok {
				t.Fatalf("failed event has workflow ID: %#v", records[0])
			}
			if strings.Contains(logs.String(), "canary") {
				t.Fatalf("raw error leaked: %s", logs.String())
			}
		})
	}
}

func TestSyncRequestLogNotEmittedBeforeAuthAndCSRF(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	var logs bytes.Buffer
	router := NewRouter(Dependencies{Auth: authService, Sync: &fakeSyncService{jobID: uuid.New()}, Observability: observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))})
	requests := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/sync", nil),
		httptest.NewRequest(http.MethodPost, "/api/sync", nil),
	}
	requests[1].AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	for _, req := range requests {
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	if strings.Contains(logs.String(), `"event":"sync_request"`) {
		t.Fatalf("pre-handler rejection emitted sync event: %s", logs.String())
	}
}

func TestSyncEndpointsRequireAuthenticationAndCSRF(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	fake := &fakeSyncService{jobID: uuid.New()}
	router := NewRouter(Dependencies{Auth: authService, Sync: fake})

	unauthenticated := httptest.NewRecorder()
	router.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticated.Code)
	}

	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	missingCSRF.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missingCSRF)
	if missingRecorder.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", missingRecorder.Code)
	}

	authorized := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	authorized.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	authorized.Header.Set("X-CSRF-Token", session.CSRFToken)
	authorizedRecorder := httptest.NewRecorder()
	router.ServeHTTP(authorizedRecorder, authorized)
	if authorizedRecorder.Code != http.StatusAccepted {
		t.Fatalf("authorized status = %d, body = %s", authorizedRecorder.Code, authorizedRecorder.Body)
	}
	var body struct {
		JobID uuid.UUID `json:"jobId"`
	}
	if err := json.NewDecoder(authorizedRecorder.Body).Decode(&body); err != nil || body.JobID != fake.jobID {
		t.Fatalf("response = %#v, %v", body, err)
	}
}

func TestSyncStatusRequiresOwnerSession(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	jobID := uuid.New()
	fake := &fakeSyncService{jobID: jobID, status: syncservice.Status{ID: jobID, Stage: "catalog", Status: "running", Progress: 50}}
	router := NewRouter(Dependencies{Auth: authService, Sync: fake})
	req := httptest.NewRequest(http.MethodGet, "/api/sync/"+jobID.String(), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || fake.statusUser != session.UserID {
		t.Fatalf("status = %d, queried user = %s", recorder.Code, fake.statusUser)
	}
}

func TestCurrentSyncContract(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	jobID := uuid.New()
	fake := &fakeSyncService{currentStatus: syncservice.Status{ID: jobID, Stage: "catalog", Status: "running", Progress: 40}, currentFound: true}
	router := NewRouter(Dependencies{Auth: authService, Sync: fake})

	t.Run("authenticated active sync", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/sync/current", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var got syncservice.Status
		if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.ID != jobID || got.Progress != 40 || fake.currentUser != session.UserID {
			t.Fatalf("status=%#v current user=%s", got, fake.currentUser)
		}
	})

	t.Run("no active sync", func(t *testing.T) {
		fake.currentFound = false
		req := httptest.NewRequest(http.MethodGet, "/api/sync/current", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
			t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("authentication required", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/sync/current", nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestSyncPreflightContract(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	displayName, avatarURL := "Player", "https://avatars.cloudflare.steamstatic.com/avatar.jpg"
	fake := &fakeSyncService{preflight: syncservice.PreflightResult{
		CanSync: true, ProfileVisibility: "public", GameDetailsVisibility: "public", DisplayName: &displayName, AvatarURL: &avatarURL,
	}}
	router := NewRouter(Dependencies{Auth: authService, Sync: fake})

	for _, test := range []struct {
		name       string
		withCookie bool
		withCSRF   bool
		status     int
	}{
		{name: "unauthenticated", status: http.StatusUnauthorized},
		{name: "missing csrf", withCookie: true, status: http.StatusForbidden},
		{name: "success", withCookie: true, withCSRF: true, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/sync/preflight", nil)
			if test.withCookie {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			}
			if test.withCSRF {
				req.Header.Set("X-CSRF-Token", session.CSRFToken)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if test.status == http.StatusOK {
				var got syncservice.PreflightResult
				if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil || !got.CanSync || got.ProfileVisibility != "public" || got.GameDetailsVisibility != "public" || got.Reason != nil || got.DisplayName == nil || *got.DisplayName != displayName {
					t.Fatalf("response = %#v, err = %v", got, err)
				}
			}
		})
	}

	for _, test := range []struct {
		name        string
		result      syncservice.PreflightResult
		wantProfile string
		wantGames   string
		wantReason  string
	}{
		{name: "private profile", result: syncservice.PreflightResult{ProfileVisibility: "private", GameDetailsVisibility: "unknown", Reason: stringTestPointer("profile_private")}, wantProfile: "private", wantGames: "unknown", wantReason: "profile_private"},
		{name: "ambiguous game details", result: syncservice.PreflightResult{ProfileVisibility: "public", GameDetailsVisibility: "unknown", Reason: stringTestPointer("game_details_unavailable")}, wantProfile: "public", wantGames: "unknown", wantReason: "game_details_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake.preflight = test.result
			req := httptest.NewRequest(http.MethodPost, "/api/sync/preflight", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			var got syncservice.PreflightResult
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil || got.CanSync || string(got.ProfileVisibility) != test.wantProfile || string(got.GameDetailsVisibility) != test.wantGames || got.Reason == nil || *got.Reason != test.wantReason {
				t.Fatalf("response = %#v, err = %v", got, err)
			}
		})
	}
}

func stringTestPointer(value string) *string { return &value }

func TestSyncPreflightFailuresAreBounded(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{name: "upstream", err: fmt.Errorf("%w: canary-private-id payload", syncservice.ErrPreflightFailed), status: http.StatusBadGateway, reason: "check_failed"},
		{name: "persistence", err: errors.New("canary-database-error"), status: http.StatusInternalServerError, reason: "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService, repo := handlerService(t, "http://localhost:8080")
			session := repo.addSession()
			var logs bytes.Buffer
			fake := &fakeSyncService{preflightErr: test.err}
			router := NewRouter(Dependencies{Auth: authService, Sync: fake, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			req := httptest.NewRequest(http.MethodPost, "/api/sync/preflight", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Reason string `json:"reason"`
			}
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil || body.Reason != test.reason {
				t.Fatalf("body = %#v, err = %v", body, err)
			}
			if strings.Contains(recorder.Body.String(), "canary") || strings.Contains(logs.String(), "canary") {
				t.Fatalf("response/logs leaked canary: %s / %s", recorder.Body.String(), logs.String())
			}
		})
	}
}

func TestDirectSyncStartMapsBlockedPreflight(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	fake := &fakeSyncService{startErr: syncservice.ErrPreflightBlocked}
	router := NewRouter(Dependencies{Auth: authService, Sync: fake})
	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestSyncRefreshContract(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	jobID := uuid.New()
	lastSuccessfulAt := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)

	for _, test := range []struct {
		name       string
		decision   syncservice.RefreshDecision
		wantStatus int
		wantBody   string
	}{
		{
			name: "fresh",
			decision: syncservice.RefreshDecision{
				Reason: syncservice.RefreshReasonFresh, LastSuccessfulAt: &lastSuccessfulAt,
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"enqueued":false,"reason":"fresh","lastSuccessfulAt":"2026-10-18T12:30:00Z"}`,
		},
		{
			name: "created stale",
			decision: syncservice.RefreshDecision{
				Enqueued: true, Reason: syncservice.RefreshReasonStale, JobID: &jobID, LastSuccessfulAt: &lastSuccessfulAt,
			},
			wantStatus: http.StatusAccepted,
			wantBody:   `{"enqueued":true,"reason":"stale","jobId":"` + jobID.String() + `","lastSuccessfulAt":"2026-10-18T12:30:00Z"}`,
		},
		{
			name:       "active",
			decision:   syncservice.RefreshDecision{Reason: syncservice.RefreshReasonActive, JobID: &jobID},
			wantStatus: http.StatusAccepted,
			wantBody:   `{"enqueued":false,"reason":"active","jobId":"` + jobID.String() + `"}`,
		},
		{
			name:       "created missing",
			decision:   syncservice.RefreshDecision{Enqueued: true, Reason: syncservice.RefreshReasonMissing, JobID: &jobID},
			wantStatus: http.StatusAccepted,
			wantBody:   `{"enqueued":true,"reason":"missing","jobId":"` + jobID.String() + `"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeSyncService{refreshDecision: test.decision}
			var logs bytes.Buffer
			router := NewRouter(Dependencies{Auth: authService, Sync: fake, Observability: observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))})
			req := httptest.NewRequest(http.MethodPost, "/api/sync/refresh-if-stale", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != test.wantStatus || strings.TrimSpace(recorder.Body.String()) != test.wantBody {
				t.Fatalf("status=%d body=%q", recorder.Code, strings.TrimSpace(recorder.Body.String()))
			}
			if fake.refreshUser != session.UserID {
				t.Fatalf("refresh user=%s want=%s", fake.refreshUser, session.UserID)
			}
			records := decodeLogRecords(t, logs.String())
			if len(records) != 1 || records[0]["event"] != "sync_request" || records[0]["outcome"] != "accepted" || records[0]["http_status"] != float64(test.wantStatus) || records[0]["reason"] != string(test.decision.Reason) {
				t.Fatalf("refresh log=%#v", records)
			}
			if test.decision.JobID != nil && records[0]["workflow_id"] != test.decision.JobID.String() {
				t.Fatalf("refresh workflow log=%#v", records[0])
			}
			if strings.Contains(logs.String(), session.UserID.String()) {
				t.Fatalf("refresh log leaked user: %s", logs.String())
			}
		})
	}
}

func TestSyncRefreshRequiresAuthenticationOwnerAndCSRF(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	owner := repo.addSession()
	fake := &fakeSyncService{refreshDecision: syncservice.RefreshDecision{Reason: syncservice.RefreshReasonFresh}}
	router := NewRouter(Dependencies{Auth: authService, Sync: fake})

	unauthenticated := httptest.NewRecorder()
	router.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/sync/refresh-if-stale", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauthenticated.Code)
	}

	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/sync/refresh-if-stale", nil)
	missingCSRF.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: owner.Token})
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missingCSRF)
	if missingRecorder.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", missingRecorder.Code)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("refresh called before middleware authorization: %d", fake.refreshCalls)
	}

	demo := repo.addSession()
	demo.AccessMode = auth.AccessModeDemo
	repo.sessions[demo.ID] = demo
	demoRequest := httptest.NewRequest(http.MethodPost, "/api/sync/refresh-if-stale", nil)
	demoRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: demo.Token})
	demoRequest.Header.Set("X-CSRF-Token", demo.CSRFToken)
	demoRecorder := httptest.NewRecorder()
	router.ServeHTTP(demoRecorder, demoRequest)
	if demoRecorder.Code != http.StatusForbidden || fake.refreshCalls != 0 {
		t.Fatalf("demo status=%d refresh calls=%d", demoRecorder.Code, fake.refreshCalls)
	}
}

func TestSyncRefreshFailuresAreSanitized(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{name: "blocked", err: fmt.Errorf("%w: private-canary", syncservice.ErrPreflightBlocked), wantStatus: http.StatusConflict, wantBody: "synchronization blocked\n"},
		{name: "failed", err: fmt.Errorf("%w: upstream-canary", syncservice.ErrPreflightFailed), wantStatus: http.StatusBadGateway, wantBody: "synchronization unavailable\n"},
		{name: "internal", err: errors.New("database-canary"), wantStatus: http.StatusInternalServerError, wantBody: "synchronization unavailable\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService, repo := handlerService(t, "http://localhost:8080")
			session := repo.addSession()
			var logs bytes.Buffer
			fake := &fakeSyncService{refreshErr: test.err}
			router := NewRouter(Dependencies{Auth: authService, Sync: fake, Observability: observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))})
			req := httptest.NewRequest(http.MethodPost, "/api/sync/refresh-if-stale", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != test.wantStatus || recorder.Body.String() != test.wantBody {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "canary") || strings.Contains(logs.String(), "canary") {
				t.Fatalf("raw error leaked: body=%q logs=%q", recorder.Body.String(), logs.String())
			}
		})
	}
}

type fakeSyncService struct {
	jobID           uuid.UUID
	status          syncservice.Status
	statusUser      uuid.UUID
	currentStatus   syncservice.Status
	currentFound    bool
	currentErr      error
	currentUser     uuid.UUID
	preflight       syncservice.PreflightResult
	preflightErr    error
	startErr        error
	refreshDecision syncservice.RefreshDecision
	refreshErr      error
	refreshUser     uuid.UUID
	refreshCalls    int
}

func (f *fakeSyncService) Start(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	return f.jobID, f.startErr
}
func (f *fakeSyncService) Preflight(_ context.Context, _ uuid.UUID) (syncservice.PreflightResult, error) {
	return f.preflight, f.preflightErr
}
func (f *fakeSyncService) RefreshIfStale(_ context.Context, userID uuid.UUID) (syncservice.RefreshDecision, error) {
	f.refreshUser = userID
	f.refreshCalls++
	return f.refreshDecision, f.refreshErr
}
func (f *fakeSyncService) GetStatus(_ context.Context, userID, _ uuid.UUID) (syncservice.Status, error) {
	f.statusUser = userID
	return f.status, nil
}
func (f *fakeSyncService) GetCurrentStatus(_ context.Context, userID uuid.UUID) (syncservice.Status, bool, error) {
	f.currentUser = userID
	return f.currentStatus, f.currentFound, f.currentErr
}
