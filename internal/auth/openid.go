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
	defaultProviderURL = "https://steamcommunity.com/openid/"
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
		return fmt.Errorf("build OpenID verification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("verify OpenID assertion: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrProviderRejected
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<10)+1))
	if err != nil {
		return fmt.Errorf("read OpenID verification response: %w", err)
	}
	if len(body) > 8<<10 {
		return ErrProviderRejected
	}
	validValues := 0
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && key == "is_valid" {
			validValues++
			if value != "true" {
				return ErrProviderRejected
			}
		}
	}
	if validValues == 1 {
		return nil
	}
	return ErrProviderRejected
}
