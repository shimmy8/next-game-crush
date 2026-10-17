package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
	syncservice "next-game-crush/internal/sync"
)

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

type fakeSyncService struct {
	jobID      uuid.UUID
	status     syncservice.Status
	statusUser uuid.UUID
}

func (f *fakeSyncService) Start(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	return f.jobID, nil
}
func (f *fakeSyncService) GetStatus(_ context.Context, userID, _ uuid.UUID) (syncservice.Status, error) {
	f.statusUser = userID
	return f.status, nil
}
