package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/profile"
)

func TestProfileReturnsPublishedActivityDocumentWithoutAdjustments(t *testing.T) {
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	publishedAt := time.Date(2026, 10, 18, 9, 0, 8, 0, time.UTC)
	createdAt := publishedAt.Add(-8 * time.Second)
	reader := &fakeProfileReader{value: profile.Profile{
		ID: uuid.New(), Version: 8, Summary: "Любит вдумчивые кооперативные игры.",
		SummaryStatus: profile.SummaryReady, SummarySource: profile.SummarySourceOpenAI,
		PublicationState: profile.PublicationPublished, PublishedAt: &publishedAt, Updating: true,
		Confidence: .72, EvidenceCount: 14, CreatedAt: createdAt,
	}}
	router := NewRouter(Dependencies{Auth: authService, Profile: reader, Preferences: &fakePreferenceService{err: errPrivateCanary}})
	req := httptest.NewRequest(http.MethodGet, "/api/profile", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "version", "summary", "summaryStatus", "summarySource", "confidence", "evidenceCount", "createdAt", "publishedAt", "updating"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("missing %s in %s", field, res.Body.String())
		}
	}
	for _, forbidden := range []string{"adjustments", "publicationState", "positive", "negative", "signalCounts"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("unexpected %s in %s", forbidden, res.Body.String())
		}
	}
	if string(body["summaryStatus"]) != `"ready"` || string(body["summarySource"]) != `"openai"` || string(body["updating"]) != "true" {
		t.Fatalf("publication fields=%s", res.Body.String())
	}
	if reader.userID != session.UserID {
		t.Fatal("profile read was not scoped to session owner")
	}
}

func TestProfileReturnsNotFoundWithoutPublishedDocument(t *testing.T) {
	for _, test := range []struct {
		name   string
		reader *fakeProfileReader
	}{
		{name: "none", reader: &fakeProfileReader{err: pgx.ErrNoRows}},
		{name: "insufficient", reader: &fakeProfileReader{err: pgx.ErrNoRows}},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService, sessions := handlerService(t, "http://localhost:8080")
			session := sessions.addSession()
			router := NewRouter(Dependencies{Auth: authService, Profile: test.reader})
			req := httptest.NewRequest(http.MethodGet, "/api/profile", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestProfileRejectsMalformedPublishedDocument(t *testing.T) {
	publishedAt := time.Now().UTC()
	for _, value := range []profile.Profile{
		{ID: uuid.New(), Version: 1, Summary: " ", SummarySource: profile.SummarySourceOpenAI, PublishedAt: &publishedAt},
		{ID: uuid.New(), Version: 1, Summary: "summary", SummarySource: "provider-private-value", PublishedAt: &publishedAt},
		{ID: uuid.New(), Version: 1, Summary: "summary", SummarySource: profile.SummarySourceOpenAI},
	} {
		authService, sessions := handlerService(t, "http://localhost:8080")
		session := sessions.addSession()
		router := NewRouter(Dependencies{Auth: authService, Profile: &fakeProfileReader{value: value}})
		req := httptest.NewRequest(http.MethodGet, "/api/profile", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "provider-private-value") {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
	}
}

var errPrivateCanary = &profileContractError{}

type profileContractError struct{}

func (*profileContractError) Error() string { return "private-profile-contract-canary" }
