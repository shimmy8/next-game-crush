package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidState    = errors.New("invalid or expired login state")
	ErrReplay          = errors.New("replayed OpenID assertion")
	ErrUserCancelled   = errors.New("OpenID authentication cancelled")
	ErrUnauthenticated = errors.New("authentication required")
	steamIDPattern     = regexp.MustCompile(`^[0-9]{17}$`)
)

type Config struct {
	AppBaseURL     string
	AllowedSteamID string
	SessionSecret  string
	ProviderURL    string
	FixtureAuth    bool
}

type LoginState struct {
	Token      string
	ReturnPath string
	ExpiresAt  time.Time
}

type Session struct {
	ID         uuid.UUID
	Token      string
	UserID     uuid.UUID
	SteamID    string
	CSRFToken  string
	ExpiresAt  time.Time
	ReturnPath string
}

type Repository interface {
	SaveLoginState(context.Context, LoginState) error
	GetLoginState(context.Context, string, time.Time) (LoginState, error)
	FinishLogin(context.Context, string, string, time.Time, string, Session, *uuid.UUID) (LoginState, Session, error)
	GetSession(context.Context, uuid.UUID) (Session, error)
	DeleteSession(context.Context, uuid.UUID) error
}

type Service struct {
	baseURL        *url.URL
	allowedSteamID string
	secret         []byte
	providerURL    string
	repository     Repository
	provider       Provider
	now            func() time.Time
	fixtureAuth    bool
}

type rotationContextKey struct{}

func New(cfg Config, repository Repository, provider Provider) (*Service, error) {
	baseURL, err := url.Parse(cfg.AppBaseURL)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" || baseURL.User != nil || baseURL.Path != "" && baseURL.Path != "/" || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("APP_BASE_URL must be an origin without a path, query, or fragment")
	}
	if cfg.AllowedSteamID == "" || !steamIDPattern.MatchString(cfg.AllowedSteamID) {
		return nil, errors.New("ALLOWED_STEAM_ID must be a 17-digit SteamID")
	}
	if cfg.SessionSecret == "" {
		return nil, errors.New("SESSION_SECRET is required")
	}
	if repository == nil {
		return nil, errors.New("auth repository is required")
	}
	if cfg.ProviderURL == "" {
		cfg.ProviderURL = defaultProviderURL
	}
	providerParsed, err := url.Parse(cfg.ProviderURL)
	if err != nil || providerParsed.Scheme != "https" || providerParsed.Host == "" {
		return nil, errors.New("OpenID provider must be an HTTPS URL")
	}
	if provider == nil {
		provider = NewHTTPProvider(nil, cfg.ProviderURL)
	}
	baseURL.Path = ""
	return &Service{baseURL: baseURL, allowedSteamID: cfg.AllowedSteamID, secret: []byte(cfg.SessionSecret), providerURL: cfg.ProviderURL, repository: repository, provider: provider, now: time.Now, fixtureAuth: cfg.FixtureAuth}, nil
}

func (s *Service) Begin(returnPath string) (string, error) {
	if returnPath == "" {
		returnPath = "/"
	}
	parsed, err := url.Parse(returnPath)
	if err != nil || !strings.HasPrefix(returnPath, "/") || strings.HasPrefix(returnPath, "//") || strings.HasPrefix(parsed.Path, "//") || strings.Contains(returnPath, "\\") || strings.ContainsAny(returnPath, "\r\n\x00") || parsed.IsAbs() || parsed.Host != "" {
		return "", failure(errors.New("return path must be local"), "begin_request", "invalid_return_path", false)
	}
	state, err := randomToken(32)
	if err != nil {
		return "", failure(err, "login_state", "entropy_failure", true)
	}
	now := s.now()
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.repository.SaveLoginState(saveCtx, LoginState{Token: state, ReturnPath: returnPath, ExpiresAt: now.Add(10 * time.Minute)}); err != nil {
		return "", failure(fmt.Errorf("save login state: %w", err), "login_state", "storage_failure", true)
	}
	returnTo := s.callbackURL(state)
	values := url.Values{
		"openid.ns": {openIDNamespace}, "openid.mode": {"checkid_setup"},
		"openid.return_to": {returnTo}, "openid.realm": {s.realm()},
		"openid.identity": {identifierSelect}, "openid.claimed_id": {identifierSelect},
	}
	return s.providerURL + "?" + values.Encode(), nil
}

func (s *Service) Complete(ctx context.Context, callback url.Values) (Session, error) {
	if len(callback["openid.mode"]) == 1 && callback.Get("openid.mode") == "cancel" {
		return Session{}, failure(ErrUserCancelled, "assertion_shape", "user_cancelled", false)
	}
	for _, field := range []string{"state", "openid.ns", "openid.mode", "openid.op_endpoint", "openid.return_to", "openid.response_nonce", "openid.claimed_id", "openid.identity", "openid.assoc_handle", "openid.signed", "openid.sig"} {
		if len(callback[field]) != 1 {
			return Session{}, failure(errors.New("OpenID assertion has missing or duplicate fields"), "assertion_shape", "missing_or_duplicate_fields", false)
		}
	}
	stateToken := callback.Get("state")
	state, err := s.repository.GetLoginState(ctx, stateToken, s.now())
	if err != nil {
		if errors.Is(err, ErrInvalidState) {
			return Session{}, failure(ErrInvalidState, "login_state", "invalid_or_expired", false)
		}
		return Session{}, failure(err, "login_state", "storage_failure", true)
	}
	expectedReturnTo := s.callbackURL(stateToken)
	if callback.Get("openid.ns") != openIDNamespace || callback.Get("openid.mode") != "id_res" || callback.Get("openid.op_endpoint") != s.providerURL || callback.Get("openid.return_to") != expectedReturnTo {
		return Session{}, failure(errors.New("invalid OpenID assertion metadata"), "metadata", "invalid", false)
	}
	signed := map[string]bool{}
	for _, field := range strings.Split(callback.Get("openid.signed"), ",") {
		signed[strings.TrimSpace(field)] = true
	}
	for _, field := range []string{"op_endpoint", "claimed_id", "identity", "return_to", "response_nonce", "assoc_handle"} {
		if !signed[field] {
			return Session{}, failure(errors.New("OpenID assertion omitted required signed fields"), "signed_fields", "required_fields_missing", false)
		}
	}
	claimed := callback.Get("openid.claimed_id")
	if callback.Get("openid.identity") != claimed {
		return Session{}, failure(errors.New("OpenID claimed identity mismatch"), "identity", "mismatch", false)
	}
	steamID, ok := parseSteamClaimedID(claimed)
	if !ok {
		return Session{}, failure(errors.New("invalid Steam claimed ID"), "identity", "invalid_claim_format", false)
	}
	if subtle.ConstantTimeCompare([]byte(steamID), []byte(s.allowedSteamID)) != 1 {
		return Session{}, failure(errors.New("SteamID is not allowlisted"), "identity", "not_allowlisted", false)
	}
	nonce := callback.Get("openid.response_nonce")
	z := strings.IndexByte(nonce, 'Z')
	if z < 0 {
		return Session{}, failure(errors.New("invalid OpenID response nonce"), "nonce", "invalid_format", false)
	}
	nonceTime, err := time.Parse(time.RFC3339Nano, nonce[:z+1])
	if err != nil || nonceTime.Before(s.now().Add(-10*time.Minute)) || nonceTime.After(s.now().Add(2*time.Minute)) {
		return Session{}, failure(errors.New("expired OpenID response nonce"), "nonce", "outside_time_window", false)
	}
	if err := s.provider.Verify(ctx, callback); err != nil {
		diagnostic := Diagnose(err)
		if diagnostic.Stage != "internal" {
			return Session{}, err
		}
		if errors.Is(err, ErrProviderRejected) {
			return Session{}, failure(err, "provider_verification", "rejected", false)
		}
		return Session{}, failure(err, "provider_verification", "transport_failure", true)
	}
	csrf, err := randomToken(32)
	if err != nil {
		return Session{}, failure(err, "session_finalize", "entropy_failure", true)
	}
	cookieToken, err := randomToken(32)
	if err != nil {
		return Session{}, failure(err, "session_finalize", "entropy_failure", true)
	}
	session := Session{ID: s.sessionID(cookieToken), Token: cookieToken, SteamID: steamID, CSRFToken: csrf, ExpiresAt: s.now().Add(7 * 24 * time.Hour), ReturnPath: state.ReturnPath}
	var oldSessionID *uuid.UUID
	if old, ok := ctx.Value(rotationContextKey{}).(uuid.UUID); ok {
		oldSessionID = &old
	}
	_, session, err = s.repository.FinishLogin(ctx, stateToken, nonce, s.now().Add(24*time.Hour), steamID, session, oldSessionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidState):
			return Session{}, failure(err, "session_finalize", "state_expired", false)
		case errors.Is(err, ErrReplay):
			return Session{}, failure(err, "session_finalize", "replay", false)
		case errors.Is(err, ErrUnauthenticated):
			return Session{}, failure(err, "session_finalize", "session_rotation_invalid", false)
		default:
			return Session{}, failure(err, "session_finalize", "storage_failure", true)
		}
	}
	return session, nil
}

func parseSteamClaimedID(raw string) (string, bool) {
	claimed, err := url.Parse(raw)
	if err != nil || claimed.Opaque != "" || claimed.User != nil {
		return "", false
	}
	if !strings.EqualFold(claimed.Scheme, "http") && !strings.EqualFold(claimed.Scheme, "https") {
		return "", false
	}
	// Comparing Host rather than only Hostname rejects explicit ports, trailing
	// dots, and other authority variations while retaining DNS case folding.
	if !strings.EqualFold(claimed.Host, "steamcommunity.com") {
		return "", false
	}
	if claimed.RawQuery != "" || claimed.ForceQuery || claimed.Fragment != "" || claimed.RawFragment != "" || strings.Contains(raw, "#") {
		return "", false
	}
	// Steam's identity path is ASCII and canonical. Reject all alternate
	// percent-encoded spellings before inspecting the decoded path.
	if claimed.RawPath != "" {
		return "", false
	}
	const prefix = "/openid/id/"
	if !strings.HasPrefix(claimed.Path, prefix) {
		return "", false
	}
	steamID := strings.TrimPrefix(claimed.Path, prefix)
	if !steamIDPattern.MatchString(steamID) {
		return "", false
	}
	return steamID, true
}

func (s *Service) Authenticate(ctx context.Context, cookie string) (Session, error) {
	id := s.sessionID(cookie)
	session, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return Session{}, ErrUnauthenticated
	}
	if !session.ExpiresAt.After(s.now()) {
		_ = s.repository.DeleteSession(ctx, id)
		return Session{}, ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare([]byte(session.SteamID), []byte(s.allowedSteamID)) != 1 {
		_ = s.repository.DeleteSession(ctx, id)
		return Session{}, ErrUnauthenticated
	}
	return session, nil
}

func (s *Service) WithSessionRotation(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, rotationContextKey{}, id)
}

func (s *Service) Logout(ctx context.Context, id uuid.UUID) error {
	return s.repository.DeleteSession(ctx, id)
}
func (s *Service) BeginFixture(string) (string, error) {
	if !s.fixtureAuth {
		return "", errors.New("fixture authentication is not available")
	}
	return "/auth/fixture", nil
}

type fixtureRepository interface {
	CreateFixtureSession(context.Context, string, Session) (Session, error)
}

func (s *Service) CreateFixtureSession(ctx context.Context) (Session, error) {
	if !s.fixtureAuth {
		return Session{}, errors.New("fixture authentication is not available")
	}
	repo, ok := s.repository.(fixtureRepository)
	if !ok {
		return Session{}, errors.New("fixture authentication is not supported")
	}
	csrf, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	token, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	session := Session{ID: s.sessionID(token), Token: token, SteamID: s.allowedSteamID, CSRFToken: csrf, ExpiresAt: s.now().Add(2 * time.Hour), ReturnPath: "/"}
	return repo.CreateFixtureSession(ctx, s.allowedSteamID, session)
}
func (s *Service) FixtureEnabled() bool { return s.fixtureAuth }
func (s *Service) ProviderURL() string  { return s.providerURL }
func (s *Service) Origin() string       { return strings.TrimRight(s.baseURL.String(), "/") }
func (s *Service) SecureCookies() bool  { return s.baseURL.Scheme == "https" }
func (s *Service) realm() string        { return strings.TrimRight(s.baseURL.String(), "/") + "/" }
func (s *Service) callbackURL(state string) string {
	return strings.TrimRight(s.baseURL.String(), "/") + "/auth/steam/callback?state=" + url.QueryEscape(state)
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate secure token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (s *Service) sessionID(token string) uuid.UUID {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(token))
	digest := mac.Sum(nil)
	var id uuid.UUID
	copy(id[:], digest[:16])
	return id
}
