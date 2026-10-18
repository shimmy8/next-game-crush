package catalog

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"next-game-crush/internal/steam"
)

func TestSharedThrottleHonorsRetryAfterAndRecoversGradually(t *testing.T) {
	db := openCatalogTestStore(t)
	workers := make([]*PersistentHostThrottle, 4)
	for i := range workers {
		var err error
		workers[i], err = NewPersistentHostThrottle(db, 4)
		if err != nil {
			t.Fatal(err)
		}
	}
	a, b := workers[0], workers[1]
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, worker := range workers {
		worker.now = func() time.Time { return now }
	}
	hint := 7 * time.Second
	a.Observe("store.steampowered.com", http.StatusTooManyRequests, &hint, errors.New("rate limited"))
	for i, worker := range workers {
		if got := worker.Limit("store.steampowered.com"); got != 2 {
			t.Fatalf("worker %d shared limit=%d", i, got)
		}
	}
	// A success already in flight when the 429 arrived cannot erase the shared
	// provider cooldown or start recovery early.
	b.Observe("store.steampowered.com", http.StatusOK, nil, nil)
	err := b.Before(context.Background(), "store.steampowered.com")
	var deferred *steam.ThrottleDeferred
	if !errors.As(err, &deferred) || !deferred.NextEligibleAt().Equal(now.Add(hint)) {
		t.Fatalf("before=%v", err)
	}
	if _, err := workers[2].AcquireSlot(context.Background(), "store.steampowered.com"); !errors.As(err, &deferred) || !deferred.NextEligibleAt().Equal(now.Add(hint)) {
		t.Fatalf("atomic slot cooldown=%v", err)
	}
	now = now.Add(hint)
	if err := b.Before(context.Background(), "store.steampowered.com"); err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers[:3] {
		worker.Observe("store.steampowered.com", http.StatusOK, nil, nil)
	}
	if got := a.Limit("store.steampowered.com"); got != 2 {
		t.Fatalf("recovered early=%d", got)
	}
	workers[3].Observe("store.steampowered.com", http.StatusOK, nil, nil)
	if got := a.Limit("store.steampowered.com"); got != 3 {
		t.Fatalf("gradual recovery=%d", got)
	}
	a.Observe("store.steampowered.com", http.StatusServiceUnavailable, nil, errors.New("upstream"))
	if got := b.Limit("store.steampowered.com"); got != 2 {
		t.Fatalf("5xx limit=%d", got)
	}
	now = now.Add(time.Second)
	b.Observe("store.steampowered.com", 0, nil, context.DeadlineExceeded)
	if got := a.Limit("store.steampowered.com"); got != 1 {
		t.Fatalf("timeout limit=%d", got)
	}
}

func TestPersistentThrottleGlobalSlotsAcrossInstances(t *testing.T) {
	db := openCatalogTestStore(t)
	a, _ := NewPersistentHostThrottle(db, 2)
	b, _ := NewPersistentHostThrottle(db, 2)
	tokenA, err := a.AcquireSlot(context.Background(), "store.steampowered.com")
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := b.AcquireSlot(context.Background(), "store.steampowered.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, throttle := range []*PersistentHostThrottle{a, b} {
		if _, err := throttle.AcquireSlot(context.Background(), "store.steampowered.com"); err == nil {
			t.Fatal("global concurrency exceeded")
		} else {
			var deferred *steam.ThrottleDeferred
			if !errors.As(err, &deferred) {
				t.Fatalf("slot wait=%v", err)
			}
		}
	}
	a.ReleaseSlot("store.steampowered.com", tokenA)
	if token, err := b.AcquireSlot(context.Background(), "store.steampowered.com"); err != nil {
		t.Fatal(err)
	} else {
		b.ReleaseSlot("store.steampowered.com", token)
	}
	b.ReleaseSlot("store.steampowered.com", tokenB)
}

func TestPersistentThrottleIgnoresPermanentFailures(t *testing.T) {
	db := openCatalogTestStore(t)
	throttle, _ := NewPersistentHostThrottle(db, 4)
	for _, tc := range []struct {
		status int
		err    error
	}{{http.StatusBadRequest, &steam.RequestError{StatusCode: http.StatusBadRequest}}, {http.StatusFound, &steam.RequestError{StatusCode: http.StatusFound}}, {0, &steam.RequestError{Temporary: false}}, {http.StatusOK, errors.New("parse failure")}} {
		throttle.Observe("store.steampowered.com", tc.status, nil, tc.err)
	}
	throttle.Observe("store.steampowered.com", 0, nil, context.Canceled)
	if got := throttle.Limit("store.steampowered.com"); got != 4 {
		t.Fatalf("permanent failure reduced limit=%d", got)
	}
	throttle.Observe("store.steampowered.com", 0, nil, &steam.RequestError{Temporary: true})
	if got := throttle.Limit("store.steampowered.com"); got != 3 {
		t.Fatalf("temporary transport limit=%d", got)
	}
}
