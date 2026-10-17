package auth

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

const allowedSteamID = "76561197960287930"

func TestCompleteAcceptsVerifiedAllowlistedSteamID(t *testing.T) {
	service, provider, repo := testService(t)
	callback := beginCallback(t, service)
	provider.valid = true
	session, err := service.Complete(context.Background(), callback)
	if err != nil {
		t.Fatal(err)
	}
	if session.SteamID != allowedSteamID || session.ID == uuid.Nil || session.CSRFToken == "" {
		t.Fatalf("session = %#v", session)
	}
	if repo.accounts[allowedSteamID] == uuid.Nil {
		t.Fatal("Steam account was not persisted")
	}
}

func TestCompleteRejectsDifferentSteamID(t *testing.T) {
	service, _, _ := testService(t)
	callback := beginCallback(t, service)
	callback.Set("openid.claimed_id", claimedID("76561198000000000"))
	callback.Set("openid.identity", claimedID("76561198000000000"))
	if _, err := service.Complete(context.Background(), callback); err == nil {
		t.Fatal("expected allowlist rejection")
	}
}

func TestCompleteRejectsInvalidReturnTo(t *testing.T) {
	service, _, _ := testService(t)
	for _, unsafe := range []string{"https://evil.example/", "//evil.example/", "/\\evil.example/", "/%2f%2fevil.example/", "/ok\r\nLocation: https://evil.example"} {
		if _, err := service.Begin(unsafe); err == nil {
			t.Errorf("expected return path %q to be rejected", unsafe)
		}
	}
	callback := beginCallback(t, service)
	callback.Set("openid.return_to", "http://localhost:8080/auth/steam/callback?state=wrong")
	if _, err := service.Complete(context.Background(), callback); err == nil {
		t.Fatal("expected return_to rejection")
	}
}

func TestCompleteRejectsDuplicateSecurityFields(t *testing.T) {
	service, _, _ := testService(t)
	callback := beginCallback(t, service)
	callback["openid.claimed_id"] = append(callback["openid.claimed_id"], callback.Get("openid.claimed_id"))
	if _, err := service.Complete(context.Background(), callback); err == nil {
		t.Fatal("expected duplicate field rejection")
	}
}

func TestCompleteRejectsUnsolicitedOrReplayedState(t *testing.T) {
	service, _, _ := testService(t)
	callback := beginCallback(t, service)
	callback.Set("state", "unsolicited")
	if _, err := service.Complete(context.Background(), callback); err == nil {
		t.Fatal("expected unsolicited state rejection")
	}
	callback = beginCallback(t, service)
	if _, err := service.Complete(context.Background(), callback); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), callback); err == nil {
		t.Fatal("expected replayed state rejection")
	}
}

func TestCompleteRejectsClaimedIDProviderMismatch(t *testing.T) {
	service, _, _ := testService(t)
	callback := beginCallback(t, service)
	callback.Set("openid.op_endpoint", "https://evil.example/openid")
	if _, err := service.Complete(context.Background(), callback); err == nil {
		t.Fatal("expected provider mismatch rejection")
	}

	callback = beginCallback(t, service)
	callback.Set("openid.claimed_id", "https://evil.example/openid/id/"+allowedSteamID)
	callback.Set("openid.identity", "https://evil.example/openid/id/"+allowedSteamID)
	if _, err := service.Complete(context.Background(), callback); err == nil {
		t.Fatal("expected claimed ID mismatch rejection")
	}
}

func TestSessionExpires(t *testing.T) {
	service, _, _ := testService(t)
	session, err := service.Complete(context.Background(), beginCallback(t, service))
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return session.ExpiresAt.Add(time.Second) }
	if _, err := service.Authenticate(context.Background(), session.Token); err == nil {
		t.Fatal("expected expired session rejection")
	}
}

func TestSessionRechecksCurrentAllowlist(t *testing.T) {
	service, _, repo := testService(t)
	session := Session{ID: uuid.New(), Token: "old-account-token", UserID: uuid.New(), SteamID: "76561198000000000", CSRFToken: "csrf", ExpiresAt: service.now().Add(time.Hour)}
	session.ID = service.sessionID(session.Token)
	repo.sessions[session.ID] = session
	if _, err := service.Authenticate(context.Background(), session.Token); err == nil {
		t.Fatal("expected old account session rejection")
	}
	if _, exists := repo.sessions[session.ID]; exists {
		t.Fatal("non-allowlisted session was not invalidated")
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	service, _, _ := testService(t)
	session, err := service.Complete(context.Background(), beginCallback(t, service))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), session.Token); err == nil {
		t.Fatal("expected logged-out session rejection")
	}
}

func TestFixtureAuthCannotStartOutsideTestHarness(t *testing.T) {
	service, _, _ := testService(t)
	if _, err := service.BeginFixture(allowedSteamID); err == nil {
		t.Fatal("fixture auth must not exist in the production service")
	}
}

type fakeProvider struct{ valid bool }

func (p *fakeProvider) Verify(_ context.Context, values url.Values) error {
	if !p.valid {
		return ErrProviderRejected
	}
	if values.Get("openid.mode") != "id_res" {
		return ErrProviderRejected
	}
	return nil
}

type memoryRepository struct {
	states   map[string]LoginState
	nonces   map[string]bool
	sessions map[uuid.UUID]Session
	accounts map[string]uuid.UUID
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{map[string]LoginState{}, map[string]bool{}, map[uuid.UUID]Session{}, map[string]uuid.UUID{}}
}
func (r *memoryRepository) SaveLoginState(_ context.Context, state LoginState) error {
	r.states[state.Token] = state
	return nil
}
func (r *memoryRepository) GetLoginState(_ context.Context, token string, now time.Time) (LoginState, error) {
	s, ok := r.states[token]
	if !ok || !s.ExpiresAt.After(now) {
		return LoginState{}, ErrInvalidState
	}
	return s, nil
}
func (r *memoryRepository) FinishLogin(_ context.Context, stateToken, nonce string, _ time.Time, steamID string, session Session, oldSessionID *uuid.UUID) (LoginState, Session, error) {
	state, ok := r.states[stateToken]
	if !ok {
		return LoginState{}, Session{}, ErrInvalidState
	}
	if r.nonces[nonce] {
		return LoginState{}, Session{}, ErrReplay
	}
	delete(r.states, stateToken)
	r.nonces[nonce] = true
	id := r.accounts[steamID]
	if id == uuid.Nil {
		id = uuid.New()
		r.accounts[steamID] = id
	}
	session.UserID = id
	session.ReturnPath = state.ReturnPath
	if oldSessionID != nil {
		delete(r.sessions, *oldSessionID)
	}
	r.sessions[session.ID] = session
	return state, session, nil
}
func (r *memoryRepository) GetSession(_ context.Context, id uuid.UUID) (Session, error) {
	s, ok := r.sessions[id]
	if !ok {
		return Session{}, ErrUnauthenticated
	}
	return s, nil
}
func (r *memoryRepository) DeleteSession(_ context.Context, id uuid.UUID) error {
	delete(r.sessions, id)
	return nil
}

func testService(t *testing.T) (*Service, *fakeProvider, *memoryRepository) {
	t.Helper()
	provider := &fakeProvider{valid: true}
	repo := newMemoryRepository()
	service, err := New(Config{
		AppBaseURL: "http://localhost:8080", AllowedSteamID: allowedSteamID,
		SessionSecret: "a-test-secret-that-is-long-enough", ProviderURL: defaultProviderURL,
	}, repo, provider)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	return service, provider, repo
}

func beginCallback(t *testing.T, service *Service) url.Values {
	t.Helper()
	redirect, err := service.Begin("/calendar")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	returnTo := u.Query().Get("openid.return_to")
	callbackURL, _ := url.Parse(returnTo)
	return url.Values{
		"state":     {callbackURL.Query().Get("state")},
		"openid.ns": {openIDNamespace}, "openid.mode": {"id_res"},
		"openid.op_endpoint": {service.providerURL}, "openid.return_to": {returnTo},
		"openid.response_nonce": {service.now().UTC().Format(time.RFC3339) + "unique"},
		"openid.assoc_handle":   {"test-handle"},
		"openid.signed":         {"op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle"},
		"openid.sig":            {"test-signature"},
		"openid.claimed_id":     {claimedID(allowedSteamID)}, "openid.identity": {claimedID(allowedSteamID)},
	}
}

func claimedID(steamID string) string { return "http://steamcommunity.com/openid/id/" + steamID }
