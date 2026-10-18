package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"next-game-crush/internal/auth"
)

func TestSteamAuthLogsBeginAndCallbackSuccessWithoutSensitiveValues(t *testing.T) {
	service, repo := handlerService(t, "http://localhost:8080")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := NewRouter(Dependencies{Auth: service, Logger: logger})

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodGet, "/auth/steam?return=/calendar-return-canary", nil)
	loginRequest.Header.Set("X-Request-ID", "client-request-id-canary")
	router.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("begin status = %d", login.Code)
	}
	serverRequestID := login.Header().Get("X-Request-ID")
	if serverRequestID == "" || serverRequestID == "client-request-id-canary" {
		t.Fatalf("server request ID = %q", serverRequestID)
	}

	providerRedirect, _ := url.Parse(login.Header().Get("Location"))
	returnTo, _ := url.Parse(providerRedirect.Query().Get("openid.return_to"))
	callback := returnTo.Query()
	callback.Set("openid.ns", "http://specs.openid.net/auth/2.0")
	callback.Set("openid.mode", "id_res")
	callback.Set("openid.op_endpoint", service.ProviderURL())
	callback.Set("openid.return_to", returnTo.String())
	callback.Set("openid.response_nonce", time.Now().UTC().Format(time.RFC3339)+"nonce-canary")
	callback.Set("openid.assoc_handle", "assoc-handle-canary")
	callback.Set("openid.signed", "op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle")
	callback.Set("openid.sig", "signature-canary-secret")
	callback.Set("openid.claimed_id", "http://steamcommunity.com/openid/id/76561197960287930")
	callback.Set("openid.identity", callback.Get("openid.claimed_id"))
	callback.Set("api_key", "api-key-canary-secret")
	callbackRequest := httptest.NewRequest(http.MethodGet, "/auth/steam/callback?"+callback.Encode(), nil)
	oldSession := repo.addSession()
	callbackRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: oldSession.Token})
	callbackRequest.AddCookie(login.Result().Cookies()[0])
	callbackRecorder := httptest.NewRecorder()
	router.ServeHTTP(callbackRecorder, callbackRequest)
	if callbackRecorder.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body = %s", callbackRecorder.Code, callbackRecorder.Body.String())
	}

	records := decodeLogRecords(t, logs.String())
	assertLogRecord(t, records, "begin", "redirected", "", "", "INFO")
	assertLogRecord(t, records, "callback", "authenticated", "", "", "INFO")
	assertNoSensitiveLogValues(t, logs.String(), []string{
		"client-request-id-canary",
		"calendar-return-canary",
		callback.Get("state"),
		oldSession.Token,
		"api-key-canary-secret",
		"signature-canary-secret",
		"assoc-handle-canary",
		"nonce-canary",
		"76561197960287930",
		callback.Get("openid.claimed_id"),
		returnTo.String(),
		providerRedirect.String(),
	})
}

func TestSteamCallbackLogsCookieFailuresWithoutSecrets(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cookie string
		reason string
	}{
		{name: "missing", reason: "missing"},
		{name: "mismatch", cookie: "cookie-state-canary", reason: "state_mismatch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service, _ := handlerService(t, "http://localhost:8080")
			var logs bytes.Buffer
			router := NewRouter(Dependencies{Auth: service, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			request := httptest.NewRequest(http.MethodGet, "/auth/steam/callback?state=query-state-canary&openid.sig=signature-canary", nil)
			if tt.cookie != "" {
				request.AddCookie(&http.Cookie{Name: loginStateCookieName, Value: tt.cookie})
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", recorder.Code)
			}
			records := decodeLogRecords(t, logs.String())
			assertLogRecord(t, records, "callback", "rejected", "callback_cookie", tt.reason, "WARN")
			assertNoSensitiveLogValues(t, logs.String(), []string{"query-state-canary", "cookie-state-canary", "signature-canary"})
		})
	}
}

func TestSteamCallbackLogsInvalidClaimClassificationWithoutClaimValue(t *testing.T) {
	service, _ := handlerService(t, "http://localhost:8080")
	var logs bytes.Buffer
	router := NewRouter(Dependencies{Auth: service, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})

	login := httptest.NewRecorder()
	router.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/auth/steam", nil))
	providerRedirect, _ := url.Parse(login.Header().Get("Location"))
	returnTo, _ := url.Parse(providerRedirect.Query().Get("openid.return_to"))
	callback := returnTo.Query()
	callback.Set("openid.ns", "http://specs.openid.net/auth/2.0")
	callback.Set("openid.mode", "id_res")
	callback.Set("openid.op_endpoint", service.ProviderURL())
	callback.Set("openid.return_to", returnTo.String())
	callback.Set("openid.response_nonce", time.Now().UTC().Format(time.RFC3339)+"invalid-claim-nonce-canary")
	callback.Set("openid.assoc_handle", "invalid-claim-handle-canary")
	callback.Set("openid.signed", "op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle")
	callback.Set("openid.sig", "invalid-claim-signature-canary")
	invalidClaim := "https://userinfo-canary@steamcommunity.com/openid/id/76561197960287930"
	callback.Set("openid.claimed_id", invalidClaim)
	callback.Set("openid.identity", invalidClaim)

	request := httptest.NewRequest(http.MethodGet, "/auth/steam/callback?"+callback.Encode(), nil)
	request.AddCookie(login.Result().Cookies()[0])
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
	records := decodeLogRecords(t, logs.String())
	assertLogRecord(t, records, "callback", "rejected", "identity", "invalid_claim_format", "WARN")
	assertNoSensitiveLogValues(t, logs.String(), []string{
		invalidClaim,
		callback.Get("state"),
		"invalid-claim-nonce-canary",
		"invalid-claim-handle-canary",
		"invalid-claim-signature-canary",
		"76561197960287930",
	})
}

func TestAuthFailureLogLevel(t *testing.T) {
	for _, tt := range []struct {
		diagnostic auth.Diagnostic
		want       slog.Level
	}{
		{auth.Diagnostic{Reason: "user_cancelled"}, slog.LevelInfo},
		{auth.Diagnostic{Reason: "rejected"}, slog.LevelWarn},
		{auth.Diagnostic{Reason: "invalid_response"}, slog.LevelWarn},
		{auth.Diagnostic{Reason: "timeout", Infrastructure: true}, slog.LevelError},
		{auth.Diagnostic{Reason: "transport_failure", Infrastructure: true}, slog.LevelError},
		{auth.Diagnostic{Reason: "storage_failure", Infrastructure: true}, slog.LevelError},
	} {
		if got := authFailureLogLevel(tt.diagnostic); got != tt.want {
			t.Errorf("authFailureLogLevel(%#v) = %v, want %v", tt.diagnostic, got, tt.want)
		}
	}
}

func TestAuthLogsUseOnlyAllowlistedFields(t *testing.T) {
	service, _ := handlerService(t, "http://localhost:8080")
	var logs bytes.Buffer
	router := NewRouter(Dependencies{Auth: service, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth/steam/callback?state=state-canary", nil))
	allowed := map[string]bool{
		"time": true, "level": true, "msg": true,
		"event": true, "phase": true, "outcome": true, "stage": true,
		"reason": true, "request_id": true, "http_status": true,
	}
	for _, record := range decodeLogRecords(t, logs.String()) {
		for key := range record {
			if !allowed[key] {
				t.Errorf("unexpected log field %q in %#v", key, record)
			}
		}
	}
}

func decodeLogRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var records []map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func assertLogRecord(t *testing.T, records []map[string]any, phase, outcome, stage, reason, level string) {
	t.Helper()
	for _, record := range records {
		if record["event"] == "steam_auth" && record["phase"] == phase && record["outcome"] == outcome && record["stage"] == stage && record["reason"] == reason && record["level"] == level {
			if requestID, ok := record["request_id"].(string); !ok || requestID == "" {
				t.Fatalf("record has no request_id: %#v", record)
			}
			return
		}
	}
	t.Fatalf("missing log record phase=%s outcome=%s stage=%s reason=%s level=%s in %#v", phase, outcome, stage, reason, level, records)
}

func assertNoSensitiveLogValues(t *testing.T, raw string, sensitive []string) {
	t.Helper()
	for _, value := range sensitive {
		if value != "" && strings.Contains(raw, value) {
			t.Errorf("sensitive canary was logged: %q", value)
		}
	}
}
