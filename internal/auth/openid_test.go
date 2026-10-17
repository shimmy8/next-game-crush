package auth

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPProviderPerformsStatelessVerification(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
