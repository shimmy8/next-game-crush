package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/notifications"
)

type digestStub struct {
	deliveryID             uuid.UUID
	sendCalls, actionCalls int
	already                bool
	err                    error
}

func (s *digestStub) SendTestDigest(context.Context, uuid.UUID) (uuid.UUID, error) {
	s.sendCalls++
	return s.deliveryID, s.err
}
func (s *digestStub) ConsumeAction(context.Context, string) (bool, error) {
	s.actionCalls++
	return s.already, s.err
}

func TestEmailActionRecordsWithoutSessionAndReplayIsSafe(t *testing.T) {
	service := &digestStub{already: true}
	recorder := httptest.NewRecorder()
	emailFeedbackAction(service).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/actions/feedback/token", nil))
	if recorder.Code != http.StatusOK || service.actionCalls != 1 {
		t.Fatalf("status=%d calls=%d", recorder.Code, service.actionCalls)
	}
	if body := recorder.Body.String(); body == "" {
		t.Fatal("missing confirmation")
	}
}

func TestEmailActionRejectsExpiredOrAlteredToken(t *testing.T) {
	for name, err := range map[string]error{"expired": notifications.ErrExpired, "altered": notifications.ErrInvalidToken} {
		t.Run(name, func(t *testing.T) {
			service := &digestStub{err: err}
			recorder := httptest.NewRecorder()
			emailFeedbackAction(service).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/actions/feedback/bad", nil))
			expected := http.StatusBadRequest
			if errors.Is(err, notifications.ErrExpired) {
				expected = http.StatusGone
			}
			if recorder.Code != expected {
				t.Fatalf("status=%d want %d", recorder.Code, expected)
			}
		})
	}
}

func TestDigestEndpointRequiresAuthAndCSRF(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	digest := &digestStub{deliveryID: uuid.New()}
	router := NewRouter(Dependencies{Auth: authService, Digests: digest})
	tests := []struct {
		name  string
		setup func(*http.Request)
		want  int
	}{
		{"anonymous", func(*http.Request) {}, http.StatusUnauthorized},
		{"missing csrf", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "ngc_session", Value: session.Token}) }, http.StatusForbidden},
		{"valid", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "ngc_session", Value: session.Token})
			r.Header.Set("X-CSRF-Token", session.CSRFToken)
		}, http.StatusAccepted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/digests/test", nil)
			test.setup(request)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if digest.sendCalls != 1 {
		t.Fatalf("send calls=%d", digest.sendCalls)
	}
}
