package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/feedback"
)

func TestFeedbackAndReminderRequireAuthenticationAndCSRF(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	service := &fakeFeedbackService{}
	recommendationID := uuid.New()
	router := NewRouter(Dependencies{Auth: authService, Feedback: service})

	for _, path := range []string{"/api/recommendations/" + recommendationID.String() + "/feedback", "/api/recommendations/" + recommendationID.String() + "/reminder"} {
		body := bytes.NewBufferString(`{"action":"like","idempotencyKey":"event-1"}`)
		unauthenticated := httptest.NewRecorder()
		router.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, path, body))
		if unauthenticated.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated status=%d", path, unauthenticated.Code)
		}

		wrongCSRF := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"action":"like","idempotencyKey":"event-1"}`))
		wrongCSRF.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		wrongCSRF.Header.Set("X-CSRF-Token", "wrong")
		denied := httptest.NewRecorder()
		router.ServeHTTP(denied, wrongCSRF)
		if denied.Code != http.StatusForbidden {
			t.Fatalf("%s CSRF status=%d", path, denied.Code)
		}
	}
}

func TestFeedbackHandlerReturnsAcceptedWorkflowMetadata(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	eventID, jobID, recommendationID := uuid.New(), uuid.New(), uuid.New()
	service := &fakeFeedbackService{accepted: feedback.Accepted{EventID: eventID, RebuildJobID: jobID, PreviousRankingVersion: 4}}
	router := NewRouter(Dependencies{Auth: authService, Feedback: service})
	req := httptest.NewRequest(http.MethodPost, "/api/recommendations/"+recommendationID.String()+"/feedback", bytes.NewBufferString(`{"action":"dislike","reason":"difficulty","freeText":"Too punishing","idempotencyKey":"event-2"}`))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got feedback.Accepted
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got != service.accepted || service.command.UserID != session.UserID || service.command.RecommendationID != recommendationID || service.command.Reason != "difficulty" {
		t.Fatalf("got=%#v command=%#v", got, service.command)
	}
}

func TestRankingDiffUsesAuthenticatedOwner(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	service := &fakeFeedbackService{diff: feedback.RankingDiff{RebuildStatus: "succeeded", PreviousVersion: 2, CurrentVersion: 3, Added: []uuid.UUID{uuid.New()}, Reordered: []uuid.UUID{uuid.New()}}}
	router := NewRouter(Dependencies{Auth: authService, Feedback: service})
	jobID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/rankings/2/diff?jobId="+jobID.String(), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK || service.diffUser != session.UserID || service.diffVersion != 2 || service.diffJob != jobID {
		t.Fatalf("status=%d user=%s version=%d job=%s", response.Code, service.diffUser, service.diffVersion, service.diffJob)
	}
}

func TestRankingDiffReturnsNotFoundForUnknownWorkflow(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	router := NewRouter(Dependencies{Auth: authService, Feedback: &fakeFeedbackService{diffErr: pgx.ErrNoRows}})
	req := httptest.NewRequest(http.MethodGet, "/api/rankings/2/diff?jobId="+uuid.NewString(), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

type fakeFeedbackService struct {
	accepted    feedback.Accepted
	command     feedback.Command
	diff        feedback.RankingDiff
	diffUser    uuid.UUID
	diffVersion int64
	diffJob     uuid.UUID
	diffErr     error
}

func (f *fakeFeedbackService) Record(_ context.Context, command feedback.Command) (feedback.Accepted, error) {
	f.command = command
	return f.accepted, nil
}
func (f *fakeFeedbackService) Diff(_ context.Context, userID uuid.UUID, version int64, jobID uuid.UUID) (feedback.RankingDiff, error) {
	f.diffUser, f.diffVersion, f.diffJob = userID, version, jobID
	return f.diff, f.diffErr
}
