package auth

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

const allowedSteamID = "76561197960287930"

func TestBeginTargetsSteamLoginEndpoint(t *testing.T) {
	service, err := New(Config{
		AppBaseURL: "http://localhost:8080", AllowedSteamID: allowedSteamID,
		SessionSecret: "a-test-secret-that-is-long-enough",
	}, newMemoryRepository(), &fakeProvider{valid: true})
	if err != nil {
		t.Fatal(err)
	}

	redirect, err := service.Begin("/calendar")
	if err != nil {
		t.Fatal(err)
	}
	providerURL, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	if providerURL.Scheme != "https" || providerURL.Host != "steamcommunity.com" || providerURL.Path != "/openid/login" {
		t.Fatalf("provider URL = %q", providerURL.String())
	}
	values := providerURL.Query()
	if values.Get("openid.ns") != openIDNamespace || values.Get("openid.mode") != "checkid_setup" {
		t.Fatalf("OpenID metadata = %#v", values)
	}
	if values.Get("openid.identity") != identifierSelect || values.Get("openid.claimed_id") != identifierSelect {
		t.Fatalf("OpenID identity selection = %#v", values)
	}
	returnTo, err := url.Parse(values.Get("openid.return_to"))
	if err != nil {
		t.Fatal(err)
	}
	if returnTo.Scheme != "http" || returnTo.Host != "localhost:8080" || returnTo.Path != "/auth/steam/callback" || returnTo.Query().Get("state") == "" {
		t.Fatalf("return_to = %q", returnTo.String())
	}
	if values.Get("openid.realm") != "http://localhost:8080/" {
		t.Fatalf("realm = %q", values.Get("openid.realm"))
	}
}

func TestCreateDemoSessionRequiresOptInAndExistingOwner(t *testing.T) {
	repo := newMemoryRepository()
	service, err := New(Config{AppBaseURL: "http://localhost:8080", AllowedSteamID: allowedSteamID, SessionSecret: "a-test-secret-that-is-long-enough"}, repo, &fakeProvider{valid: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateDemoSession(context.Background()); err == nil {
		t.Fatal("disabled demo mode created a session")
	}
	service, err = New(Config{AppBaseURL: "http://localhost:8080", AllowedSteamID: allowedSteamID, SessionSecret: "a-test-secret-that-is-long-enough", DemoMode: true}, repo, &fakeProvider{valid: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateDemoSession(context.Background()); !errors.Is(err, ErrDemoUnavailable) {
		t.Fatalf("missing owner error = %v", err)
	}
	repo.accounts[allowedSteamID] = uuid.New()
	session, err := service.CreateDemoSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.AccessMode != AccessModeDemo || session.UserID != repo.accounts[allowedSteamID] || session.SteamID != allowedSteamID || session.Token == "" || session.CSRFToken == "" {
		t.Fatalf("demo session = %#v", session)
	}
	authenticated, err := service.Authenticate(context.Background(), session.Token)
	if err != nil || authenticated.AccessMode != AccessModeDemo {
		t.Fatalf("Authenticate() = %#v, %v", authenticated, err)
	}
}

func TestCompleteDistinguishesExpiredStateFromStorageFailure(t *testing.T) {
	service, _, _ := testService(t)
	callback := beginCallback(t, service)
	callback.Set("state", "expired-state-canary")
	_, err := service.Complete(context.Background(), callback)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("errors.Is(%v, ErrInvalidState) = false", err)
	}
	if got := Diagnose(err); got.Stage != "login_state" || got.Reason != "invalid_or_expired" || got.Infrastructure {
		t.Fatalf("expired state diagnostic = %#v", got)
	}

	repo := &failingGetRepository{memoryRepository: newMemoryRepository(), err: errors.New("database-canary-secret")}
	storageService, newErr := New(Config{
		AppBaseURL: "http://localhost:8080", AllowedSteamID: allowedSteamID,
		SessionSecret: "a-test-secret-that-is-long-enough", ProviderURL: defaultProviderURL,
	}, repo, &fakeProvider{valid: true})
	if newErr != nil {
		t.Fatal(newErr)
	}
	_, err = storageService.Complete(context.Background(), callback)
	if errors.Is(err, ErrInvalidState) {
		t.Fatalf("storage error was collapsed into ErrInvalidState: %v", err)
	}
	if got := Diagnose(err); got.Stage != "login_state" || got.Reason != "storage_failure" || !got.Infrastructure {
		t.Fatalf("storage diagnostic = %#v", got)
	}
}

func TestCompleteReturnsSafeFailureClassifications(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(url.Values)
		stage  string
		reason string
	}{
		{"missing fields", func(v url.Values) { v.Del("openid.sig") }, "assertion_shape", "missing_or_duplicate_fields"},
		{"metadata", func(v url.Values) { v.Set("openid.op_endpoint", "https://attacker.invalid/canary") }, "metadata", "invalid"},
		{"signed fields", func(v url.Values) { v.Set("openid.signed", "op_endpoint") }, "signed_fields", "required_fields_missing"},
		{"identity mismatch", func(v url.Values) { v.Set("openid.identity", "identity-canary") }, "identity", "mismatch"},
		{"claim format", func(v url.Values) {
			v.Set("openid.claimed_id", "claim-canary")
			v.Set("openid.identity", "claim-canary")
		}, "identity", "invalid_claim_format"},
		{"allowlist", func(v url.Values) {
			v.Set("openid.claimed_id", claimedID("76561198000000000"))
			v.Set("openid.identity", claimedID("76561198000000000"))
		}, "identity", "not_allowlisted"},
		{"nonce format", func(v url.Values) { v.Set("openid.response_nonce", "nonce-canary") }, "nonce", "invalid_format"},
		{"nonce window", func(v url.Values) { v.Set("openid.response_nonce", "2020-01-01T00:00:00Znonce-canary") }, "nonce", "outside_time_window"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, _ := testService(t)
			callback := beginCallback(t, service)
			tt.mutate(callback)
			_, err := service.Complete(context.Background(), callback)
			if got := Diagnose(err); got.Stage != tt.stage || got.Reason != tt.reason {
				t.Fatalf("diagnostic = %#v", got)
			}
		})
	}
}

func TestCompletePreservesReplaySentinel(t *testing.T) {
	repo := &failingFinishRepository{memoryRepository: newMemoryRepository(), err: ErrReplay}
	service, err := New(Config{
		AppBaseURL: "http://localhost:8080", AllowedSteamID: allowedSteamID,
		SessionSecret: "a-test-secret-that-is-long-enough", ProviderURL: defaultProviderURL,
	}, repo, &fakeProvider{valid: true})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	_, err = service.Complete(context.Background(), beginCallback(t, service))
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("errors.Is(%v, ErrReplay) = false", err)
	}
	if got := Diagnose(err); got.Stage != "session_finalize" || got.Reason != "replay" {
		t.Fatalf("replay diagnostic = %#v", got)
	}
}

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

func TestCompleteAcceptsHTTPSClaimedIdentityFromSteam(t *testing.T) {
	for _, host := range []string{"steamcommunity.com", "STEAMCOMMUNITY.COM"} {
		t.Run(host, func(t *testing.T) {
			service, _, _ := testService(t)
			callback := beginCallback(t, service)
			claim := "https://" + host + "/openid/id/" + allowedSteamID
			callback.Set("openid.claimed_id", claim)
			callback.Set("openid.identity", claim)
			session, err := service.Complete(context.Background(), callback)
			if err != nil {
				t.Fatal(err)
			}
			if session.SteamID != allowedSteamID {
				t.Fatalf("unexpected authenticated account")
			}
		})
	}
}

func TestCompleteStrictlyRejectsMalformedClaimedIdentityURLs(t *testing.T) {
	tests := []struct {
		name  string
		claim string
	}{
		{"look-alike host", "https://steamcommunlty.com/openid/id/" + allowedSteamID},
		{"subdomain", "https://login.steamcommunity.com/openid/id/" + allowedSteamID},
		{"host suffix", "https://steamcommunity.com.evil.example/openid/id/" + allowedSteamID},
		{"userinfo", "https://user@steamcommunity.com/openid/id/" + allowedSteamID},
		{"explicit port", "https://steamcommunity.com:443/openid/id/" + allowedSteamID},
		{"query", "https://steamcommunity.com/openid/id/" + allowedSteamID + "?source=canary"},
		{"empty query", "https://steamcommunity.com/openid/id/" + allowedSteamID + "?"},
		{"fragment", "https://steamcommunity.com/openid/id/" + allowedSteamID + "#canary"},
		{"empty fragment", "https://steamcommunity.com/openid/id/" + allowedSteamID + "#"},
		{"wrong path", "https://steamcommunity.com/openid/user/" + allowedSteamID},
		{"encoded slash", "https://steamcommunity.com/openid%2fid/" + allowedSteamID},
		{"encoded backslash", "https://steamcommunity.com/openid%5cid/" + allowedSteamID},
		{"encoded dot", "https://steamcommunity.com/openid/%2e/id/" + allowedSteamID},
		{"dot segment", "https://steamcommunity.com/openid/../id/" + allowedSteamID},
		{"raw backslash", "https://steamcommunity.com/openid\\id/" + allowedSteamID},
		{"short ID", "https://steamcommunity.com/openid/id/7656119796028793"},
		{"long ID", "https://steamcommunity.com/openid/id/765611979602879300"},
		{"non-digit ID", "https://steamcommunity.com/openid/id/7656119796028793x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, _ := testService(t)
			callback := beginCallback(t, service)
			callback.Set("openid.claimed_id", tt.claim)
			callback.Set("openid.identity", tt.claim)
			_, err := service.Complete(context.Background(), callback)
			if err == nil {
				t.Fatal("expected claimed identity rejection")
			}
			if got := Diagnose(err); got.Stage != "identity" || got.Reason != "invalid_claim_format" || got.Infrastructure {
				t.Fatalf("diagnostic = %#v", got)
			}
		})
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

type failingGetRepository struct {
	*memoryRepository
	err error
}

func (r *failingGetRepository) GetLoginState(context.Context, string, time.Time) (LoginState, error) {
	return LoginState{}, r.err
}

type failingFinishRepository struct {
	*memoryRepository
	err error
}

func (r *failingFinishRepository) FinishLogin(context.Context, string, string, time.Time, string, Session, *uuid.UUID) (LoginState, Session, error) {
	return LoginState{}, Session{}, r.err
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
func (r *memoryRepository) CreateDemoSession(_ context.Context, steamID string, session Session) (Session, error) {
	id := r.accounts[steamID]
	if id == uuid.Nil {
		return Session{}, ErrDemoUnavailable
	}
	session.UserID = id
	session.SteamID = steamID
	r.sessions[session.ID] = session
	return session, nil
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
