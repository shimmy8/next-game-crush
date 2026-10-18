package steam

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type testHostThrottle struct {
	before  func(context.Context, string) error
	observe func(string, int, *time.Duration, error)
	limit   int
}

func (h testHostThrottle) Before(ctx context.Context, host string) error {
	if h.before != nil {
		return h.before(ctx, host)
	}
	return nil
}
func (h testHostThrottle) Observe(host string, status int, hint *time.Duration, err error) {
	if h.observe != nil {
		h.observe(host, status, hint, err)
	}
}
func (h testHostThrottle) Limit(string) int { return h.limit }

func TestCatalogThrottleObservesEveryHTTPAttempt(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var before, observed int
			throttle := testHostThrottle{limit: 4, before: func(_ context.Context, host string) error {
				before++
				if host != "127.0.0.1" {
					t.Fatalf("host = %q", host)
				}
				return nil
			}, observe: func(host string, got int, hint *time.Duration, err error) {
				observed++
				if host != "127.0.0.1" || got != status {
					t.Fatalf("observation = %q,%d", host, got)
				}
				if status != http.StatusOK {
					if err == nil || hint == nil || *hint != 7*time.Second {
						t.Fatalf("failure hint=%v err=%v", hint, err)
					}
				} else if err != nil || hint != nil {
					t.Fatal("success reported failure")
				}
			}}
			client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if status != http.StatusOK {
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(status)
					return
				}
				serveFixture(t, w, "store_app.json")
			}), HTTPConfig{MaxRetries: -1, HostThrottle: throttle})
			_, _ = client.GameDetails(context.Background(), 620)
			if before != 1 || observed != 1 {
				t.Fatalf("before=%d observed=%d", before, observed)
			}
		})
	}
}

func TestCatalogThrottleDoesNotBlockInteractiveSteamClient(t *testing.T) {
	entered := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); serveFixture(t, w, "store_app.json") }))
	defer server.Close()
	cfg := HTTPConfig{APIKey: testAPIKey, StoreBaseURL: server.URL, HTTPClient: server.Client(), MaxRetries: -1, MinRequestInterval: -1}
	interactive, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HostThrottle = testHostThrottle{limit: 1, before: func(ctx context.Context, _ string) error { close(entered); <-ctx.Done(); return ctx.Err() }}
	catalog, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := catalog.GameDetails(ctx, 620); done <- err }()
	<-entered
	interactiveCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := interactive.GameDetails(interactiveCtx, 620); err != nil {
		t.Fatalf("interactive blocked: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("catalog cancellation = %v", err)
	}
}

func TestCatalogThrottleEnforcesHostConcurrency(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		serveFixture(t, w, "store_app.json")
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	client, err := NewClient(HTTPConfig{APIKey: testAPIKey, StoreBaseURL: server.URL, HTTPClient: server.Client(), MaxRetries: -1, MinRequestInterval: -1, HostThrottle: testHostThrottle{limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 4)
	for range 4 {
		go func() { _, err := client.GameDetails(context.Background(), 620); done <- err }()
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	select {
	case <-entered:
		t.Fatal("catalog host concurrency exceeded")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogThrottleObservesTimeout(t *testing.T) {
	var observed atomic.Int32
	client := fixtureClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), HTTPConfig{MaxRetries: -1, RequestTimeout: 20 * time.Millisecond, HostThrottle: testHostThrottle{limit: 4, observe: func(_ string, status int, hint *time.Duration, err error) {
		if status != 0 || hint != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout observation status=%d hint=%v err=%v", status, hint, err)
		}
		observed.Add(1)
	}}})
	if _, err := client.GameDetails(context.Background(), 620); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
	if observed.Load() != 1 {
		t.Fatalf("observations = %d", observed.Load())
	}
}
