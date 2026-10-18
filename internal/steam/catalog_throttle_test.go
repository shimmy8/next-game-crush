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

type testSlotThrottle struct {
	testHostThrottle
	acquired, released atomic.Int32
	acquire            func(context.Context) error
}

func (h *testSlotThrottle) AcquireSlot(ctx context.Context, _ string) (string, error) {
	h.acquired.Add(1)
	if h.acquire != nil {
		if err := h.acquire(ctx); err != nil {
			return "", err
		}
	}
	return "token", nil
}
func (h *testSlotThrottle) ReleaseSlot(string, string) { h.released.Add(1) }

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
				if host != "store.steampowered.com" {
					t.Fatalf("host = %q", host)
				}
				return nil
			}, observe: func(host string, got int, hint *time.Duration, err error) {
				observed++
				if host != "store.steampowered.com" || got != status {
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
			if before != 2 || observed != 1 {
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

func TestCatalogThrottleDeferralSkipsTransportRetries(t *testing.T) {
	eligible := time.Now().UTC().Add(time.Hour)
	var before, requests atomic.Int32
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		serveFixture(t, w, "store_app.json")
	}), HTTPConfig{HostThrottle: testHostThrottle{limit: 4, before: func(context.Context, string) error {
		before.Add(1)
		return &ThrottleDeferred{At: eligible}
	}}})
	_, err := client.GameDetails(context.Background(), 620)
	var deferred *ThrottleDeferred
	if !errors.As(err, &deferred) || !deferred.NextEligibleAt().Equal(eligible) {
		t.Fatalf("deferred=%v", err)
	}
	if before.Load() != 1 || requests.Load() != 0 {
		t.Fatalf("before=%d requests=%d", before.Load(), requests.Load())
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

func TestCatalogGlobalSlotReleasedOnCancellation(t *testing.T) {
	throttle := &testSlotThrottle{testHostThrottle: testHostThrottle{limit: 1}}
	started := make(chan struct{})
	client := fixtureClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }), HTTPConfig{MaxRetries: -1, RequestTimeout: time.Minute, HostThrottle: throttle})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.GameDetails(ctx, 620); done <- err }()
	<-started
	cancel()
	<-done
	if throttle.acquired.Load() != 1 || throttle.released.Load() != 1 {
		t.Fatalf("slots acquired=%d released=%d", throttle.acquired.Load(), throttle.released.Load())
	}
}

func TestCatalogTakesLocalPermitBeforeGlobalSlot(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	throttle := &testSlotThrottle{testHostThrottle: testHostThrottle{limit: 1}, acquire: func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveFixture(t, w, "store_app.json") }), HTTPConfig{MaxRetries: -1, MinRequestInterval: -1, HostThrottle: throttle})
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := client.GameDetails(context.Background(), 620); done <- err }()
	}
	<-entered
	select {
	case <-entered:
		t.Fatal("second request hoarded a global slot")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if throttle.acquired.Load() != 2 || throttle.released.Load() != 2 {
		t.Fatalf("slots=%d/%d", throttle.acquired.Load(), throttle.released.Load())
	}
}

func TestCatalogRechecksCooldownAfterWaitingForLocalPermit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	beforeCalls := make(chan struct{}, 4)
	var cooled atomic.Bool
	var requests atomic.Int32
	throttle := &testSlotThrottle{testHostThrottle: testHostThrottle{
		limit: 1,
		before: func(context.Context, string) error {
			beforeCalls <- struct{}{}
			if cooled.Load() {
				return &ThrottleDeferred{At: time.Now().UTC().Add(time.Minute)}
			}
			return nil
		},
		observe: func(_ string, status int, _ *time.Duration, _ error) {
			if status == http.StatusTooManyRequests {
				cooled.Store(true)
			}
		},
	}}
	client := fixtureClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		close(entered)
		<-release
		w.WriteHeader(http.StatusTooManyRequests)
	}), HTTPConfig{MaxRetries: -1, MinRequestInterval: -1, HostThrottle: throttle})
	done := make(chan error, 2)
	go func() { _, err := client.GameDetails(context.Background(), 620); done <- err }()
	<-entered
	go func() { _, err := client.GameDetails(context.Background(), 620); done <- err }()
	for range 3 {
		select {
		case <-beforeCalls:
		case <-time.After(time.Second):
			t.Fatal("second request did not reach local permit wait")
		}
	}
	close(release)
	for range 2 {
		<-done
	}
	if requests.Load() != 1 || throttle.acquired.Load() != 1 || throttle.released.Load() != 1 {
		t.Fatalf("requests=%d slots=%d/%d", requests.Load(), throttle.acquired.Load(), throttle.released.Load())
	}
}
