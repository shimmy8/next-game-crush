package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	openIDNamespace    = "http://specs.openid.net/auth/2.0"
	identifierSelect   = "http://specs.openid.net/auth/2.0/identifier_select"
	defaultProviderURL = "https://steamcommunity.com/openid/login"
)

var ErrProviderRejected = errors.New("OpenID provider rejected the assertion")

type Provider interface {
	Verify(context.Context, url.Values) error
}

type HTTPProvider struct {
	client *http.Client
	url    string
}

func NewHTTPProvider(client *http.Client, providerURL string) *HTTPProvider {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &HTTPProvider{client: client, url: providerURL}
}

func (p *HTTPProvider) Verify(ctx context.Context, assertion url.Values) error {
	values := make(url.Values)
	for key, value := range assertion {
		if strings.HasPrefix(key, "openid.") {
			values[key] = append([]string(nil), value...)
		}
	}
	values.Set("openid.mode", "check_authentication")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, strings.NewReader(values.Encode()))
	if err != nil {
		return failure(fmt.Errorf("build OpenID verification request: %w", err), "provider_verification", "transport_failure", true)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.client.Do(req)
	if err != nil {
		reason := "transport_failure"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "timeout"
		} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
			reason = "timeout"
		}
		return failure(fmt.Errorf("verify OpenID assertion: %w", err), "provider_verification", reason, true)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return failure(ErrProviderRejected, "provider_verification", "rejected", false)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<10)+1))
	if err != nil {
		return failure(fmt.Errorf("read OpenID verification response: %w", err), "provider_verification", "transport_failure", true)
	}
	if len(body) > 8<<10 {
		return failure(ErrProviderRejected, "provider_verification", "invalid_response", false)
	}
	validValues := 0
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && key == "is_valid" {
			validValues++
			if value != "true" {
				return failure(ErrProviderRejected, "provider_verification", "rejected", false)
			}
		}
	}
	if validValues == 1 {
		return nil
	}
	return failure(ErrProviderRejected, "provider_verification", "invalid_response", false)
}
