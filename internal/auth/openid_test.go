package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPProviderPerformsStatelessVerification(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "steamcommunity.com" || r.URL.Path != "/openid/login" {
			t.Errorf("verification request = %s %s", r.Method, r.URL.String())
		}
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		if values.Get("openid.mode") != "check_authentication" {
			t.Errorf("mode = %q", values.Get("openid.mode"))
		}
		if values.Get("state") != "" {
			t.Error("application state was sent to provider")
		}
		return testResponse("ns:" + openIDNamespace + "\nis_valid:true\n"), nil
	})}
	provider := NewHTTPProvider(client, defaultProviderURL)
	if err := provider.Verify(context.Background(), url.Values{"openid.mode": {"id_res"}, "openid.sig": {"signature"}, "state": {"private-state"}}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPProviderClassifiesSafeFailures(t *testing.T) {
	tests := []struct {
		name   string
		client *http.Client
		stage  string
		reason string
	}{
		{
			name: "rejected",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("secret body")), Header: make(http.Header)}, nil
			})},
			stage: "provider_verification", reason: "rejected",
		},
		{
			name: "invalid response",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return testResponse("not-an-openid-response"), nil
			})},
			stage: "provider_verification", reason: "invalid_response",
		},
		{
			name: "timeout",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, timeoutError{}
			})},
			stage: "provider_verification", reason: "timeout",
		},
		{
			name: "transport",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("transport-canary-secret")
			})},
			stage: "provider_verification", reason: "transport_failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewHTTPProvider(tt.client, defaultProviderURL).Verify(context.Background(), url.Values{"openid.sig": {"signature-canary"}})
			diagnostic := Diagnose(err)
			if diagnostic.Stage != tt.stage || diagnostic.Reason != tt.reason {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout-canary-secret" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ interface{ Timeout() bool } = timeoutError{}

func TestProviderRejectionPreservesSentinel(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testResponse("is_valid:false\n"), nil
	})}
	err := NewHTTPProvider(client, defaultProviderURL).Verify(context.Background(), url.Values{"openid.mode": {"id_res"}})
	if !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("errors.Is(%v, ErrProviderRejected) = false", err)
	}
	if Diagnose(err).Reason != "rejected" {
		t.Fatalf("diagnostic = %#v", Diagnose(err))
	}
}

func TestHTTPProviderRejectsOversizedOrInvalidResponse(t *testing.T) {
	for _, body := range []string{"is_valid:false\n", strings.Repeat("x", (8<<10)+1) + "\nis_valid:true\n"} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return testResponse(body), nil })}
		provider := NewHTTPProvider(client, defaultProviderURL)
		if err := provider.Verify(context.Background(), url.Values{"openid.mode": {"id_res"}}); err == nil {
			t.Error("expected provider response rejection")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
func testResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
