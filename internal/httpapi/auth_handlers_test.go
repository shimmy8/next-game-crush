package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
	syncservice "next-game-crush/internal/sync"
)

func TestMeIncludesSteamIdentity(t *testing.T) {
	authService, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	displayName, avatarURL := "Player", "https://avatars.cloudflare.steamstatic.com/avatar.jpg"
	identity := &fakeIdentityReadService{identity: syncservice.SteamIdentity{DisplayName: &displayName, AvatarURL: &avatarURL}}
	router := NewRouter(Dependencies{Auth: authService, Identity: identity})
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Authenticated bool      `json:"authenticated"`
		SteamID       string    `json:"steamId"`
		CSRFToken     string    `json:"csrfToken"`
		ExpiresAt     time.Time `json:"expiresAt"`
		DisplayName   *string   `json:"displayName"`
		AvatarURL     *string   `json:"avatarUrl"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Authenticated || body.SteamID != session.SteamID || body.CSRFToken != session.CSRFToken || body.ExpiresAt.IsZero() || body.DisplayName == nil || *body.DisplayName != displayName || body.AvatarURL == nil || *body.AvatarURL != avatarURL {
		t.Fatalf("body = %#v", body)
	}
}

func TestMeIdentityNullableAndFailure(t *testing.T) {
	for _, test := range []struct {
		name   string
		reader *fakeIdentityReadService
		status int
	}{
		{name: "nullable", reader: &fakeIdentityReadService{}, status: http.StatusOK},
		{name: "failure", reader: &fakeIdentityReadService{err: errors.New("identity failed")}, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService, repo := handlerService(t, "http://localhost:8080")
			session := repo.addSession()
			req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
			recorder := httptest.NewRecorder()
			NewRouter(Dependencies{Auth: authService, Identity: test.reader}).ServeHTTP(recorder, req)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), `"authenticated":true`) || !strings.Contains(recorder.Body.String(), `"displayName":null`) || !strings.Contains(recorder.Body.String(), `"avatarUrl":null`) {
				t.Fatalf("body = %s", recorder.Body.String())
			}
		})
	}
}

type fakeIdentityReadService struct {
	identity syncservice.SteamIdentity
	err      error
}

func (f *fakeIdentityReadService) SteamIdentity(context.Context, uuid.UUID) (syncservice.SteamIdentity, error) {
	return f.identity, f.err
}

func TestSessionCookieIsHTTPOnlySameSite(t *testing.T) {
	service, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	recorder := httptest.NewRecorder()
	service.SetSessionCookie(recorder, session)
	cookie := recorder.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
		t.Fatalf("cookie = %#v", cookie)
	}
}

func TestHTTPSCookieIsSecure(t *testing.T) {
	service, repo := handlerService(t, "https://example.test")
	recorder := httptest.NewRecorder()
	service.SetSessionCookie(recorder, repo.addSession())
	if !recorder.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS cookie is not Secure")
	}
}

func TestSteamLoginRedirectTargetsSteamOPEndpoint(t *testing.T) {
	service, _ := handlerService(t, "http://localhost:8080")
	recorder := httptest.NewRecorder()
	NewRouter(Dependencies{Auth: service}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth/steam", nil))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusSeeOther)
	}
	providerURL, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if providerURL.Scheme != "https" || providerURL.Host != "steamcommunity.com" || providerURL.Path != "/openid/login" {
		t.Fatalf("Location = %q", providerURL.String())
	}
	returnTo, err := url.Parse(providerURL.Query().Get("openid.return_to"))
	if err != nil {
		t.Fatal(err)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "ngc_login_state" || cookies[0].Value == "" || cookies[0].Value != returnTo.Query().Get("state") {
		t.Fatalf("state cookie = %#v, return_to = %q", cookies, returnTo.String())
	}
}

func TestLoginRotatesSession(t *testing.T) {
	service, repo := handlerService(t, "http://localhost:8080")
	old := repo.addSession()
	login := httptest.NewRecorder()
	NewRouter(Dependencies{Auth: service}).ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/auth/steam", nil))
	providerRedirect, _ := url.Parse(login.Header().Get("Location"))
	loginStateCookie := login.Result().Cookies()[0]
	returnTo, _ := url.Parse(providerRedirect.Query().Get("openid.return_to"))
	callback := returnTo.Query()
	callback.Set("openid.ns", "http://specs.openid.net/auth/2.0")
	callback.Set("openid.mode", "id_res")
	callback.Set("openid.op_endpoint", service.ProviderURL())
	callback.Set("openid.return_to", returnTo.String())
	callback.Set("openid.response_nonce", time.Now().UTC().Format(time.RFC3339)+"handler")
	callback.Set("openid.assoc_handle", "test-handle")
	callback.Set("openid.signed", "op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle")
	callback.Set("openid.sig", "test-signature")
	callback.Set("openid.claimed_id", "http://steamcommunity.com/openid/id/76561197960287930")
	callback.Set("openid.identity", callback.Get("openid.claimed_id"))
	req := httptest.NewRequest(http.MethodGet, "/auth/steam/callback?"+callback.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: old.Token})
	req.AddCookie(loginStateCookie)
	recorder := httptest.NewRecorder()
	NewRouter(Dependencies{Auth: service}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	if _, ok := repo.sessions[old.ID]; ok {
		t.Fatal("old session was not invalidated")
	}
	if len(repo.sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(repo.sessions))
	}
}

func TestSteamCallbackRequiresInitiatingBrowserCookie(t *testing.T) {
	service, _ := handlerService(t, "http://localhost:8080")
	recorder := httptest.NewRecorder()
	NewRouter(Dependencies{Auth: service}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth/steam/callback?state=issued-elsewhere", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

func TestMutationRejectsMissingOrWrongCSRF(t *testing.T) {
	service, repo := handlerService(t, "http://localhost:8080")
	session := repo.addSession()
	handler := service.Middleware(service.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	for _, token := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/mutation", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.Token})
		req.Header.Set("X-CSRF-Token", token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("token %q status = %d", token, recorder.Code)
		}
	}
}

type handlerRepo struct{ sessions map[uuid.UUID]auth.Session }

func (r *handlerRepo) addSession() auth.Session {
	token := uuid.NewString() + uuid.NewString()
	mac := hmac.New(sha256.New, []byte(strings.Repeat("s", 32)))
	_, _ = mac.Write([]byte(token))
	sum := mac.Sum(nil)
	var id uuid.UUID
	copy(id[:], sum[:16])
	s := auth.Session{ID: id, Token: token, UserID: uuid.New(), SteamID: "76561197960287930", CSRFToken: "csrf", ExpiresAt: time.Now().Add(time.Hour)}
	r.sessions[s.ID] = s
	return s
}
func (*handlerRepo) SaveLoginState(context.Context, auth.LoginState) error { return nil }
func (*handlerRepo) GetLoginState(_ context.Context, token string, _ time.Time) (auth.LoginState, error) {
	return auth.LoginState{Token: token, ReturnPath: "/", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (r *handlerRepo) FinishLogin(_ context.Context, token, _ string, _ time.Time, steamID string, session auth.Session, oldSessionID *uuid.UUID) (auth.LoginState, auth.Session, error) {
	state := auth.LoginState{Token: token, ReturnPath: "/", ExpiresAt: time.Now().Add(time.Hour)}
	session.UserID = uuid.New()
	session.SteamID = steamID
	if oldSessionID != nil {
		delete(r.sessions, *oldSessionID)
	}
	r.sessions[session.ID] = session
	return state, session, nil
}
func (r *handlerRepo) GetSession(_ context.Context, id uuid.UUID) (auth.Session, error) {
	s, ok := r.sessions[id]
	if !ok {
		return auth.Session{}, auth.ErrUnauthenticated
	}
	return s, nil
}
func (r *handlerRepo) DeleteSession(_ context.Context, id uuid.UUID) error {
	delete(r.sessions, id)
	return nil
}

type acceptProvider struct{}

func (acceptProvider) Verify(context.Context, url.Values) error { return nil }

func handlerService(t *testing.T, baseURL string) (*auth.Service, *handlerRepo) {
	t.Helper()
	repo := &handlerRepo{sessions: map[uuid.UUID]auth.Session{}}
	service, err := auth.New(auth.Config{AppBaseURL: baseURL, AllowedSteamID: "76561197960287930", SessionSecret: strings.Repeat("s", 32)}, repo, acceptProvider{})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}
