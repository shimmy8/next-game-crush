package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type testRetryHint struct {
	delay time.Duration
	valid bool
}

func (e testRetryHint) Error() string                     { return "hint" }
func (e testRetryHint) RetryAfter() (time.Duration, bool) { return e.delay, e.valid }

func TestCatalogRetryScheduleAndRetryAfterCap(t *testing.T) {
	for _, test := range []struct {
		name    string
		attempt int
		hint    testRetryHint
		want    time.Duration
	}{
		{"first retry", 1, testRetryHint{}, time.Minute},
		{"second retry", 2, testRetryHint{}, 5 * time.Minute},
		{"third retry", 3, testRetryHint{}, 30 * time.Minute},
		{"exhausted schedule stays capped", 4, testRetryHint{}, 30 * time.Minute},
		{"short provider hint wins", 2, testRetryHint{time.Second, true}, time.Second},
		{"long provider hint", 1, testRetryHint{2 * time.Hour, true}, 2 * time.Hour},
		{"provider cap", 1, testRetryHint{48 * time.Hour, true}, 24 * time.Hour},
		{"zero hint", 2, testRetryHint{0, true}, 5 * time.Minute},
		{"negative hint", 3, testRetryHint{-time.Second, true}, 30 * time.Minute},
		{"malformed hint", 1, testRetryHint{time.Hour, false}, time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker := newWorker(nil, LaneCatalog, "worker", WithRetrySchedule(time.Minute, 5*time.Minute, 30*time.Minute), WithRetryAfterCap(24*time.Hour), WithRetryJitter(func(delay time.Duration) time.Duration { return delay }))
			if got := worker.retryDelay(test.attempt, fmt.Errorf("wrapped: %w", test.hint)); got != test.want {
				t.Fatalf("retry delay = %s, want %s", got, test.want)
			}
		})
	}
	t.Run("valid provider hint bypasses jitter", func(t *testing.T) {
		worker := newWorker(nil, LaneCatalog, "worker", WithRetrySchedule(time.Minute), WithRetryAfterCap(24*time.Hour), WithRetryJitter(func(time.Duration) time.Duration { t.Fatal("jitter applied to provider hint"); return 0 }))
		if got := worker.retryDelay(1, testRetryHint{time.Second, true}); got != time.Second {
			t.Fatal(got)
		}
	})
	t.Run("default jitter remains bounded", func(t *testing.T) {
		worker := newWorker(nil, LaneCatalog, "worker", WithRetrySchedule(time.Minute, 5*time.Minute, 30*time.Minute))
		for attempt, delay := range []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute} {
			for range 100 {
				if got := worker.retryDelay(attempt+1, errors.New("failure")); got < delay/2 || got > delay {
					t.Fatalf("jitter delay %s outside [%s,%s]", got, delay/2, delay)
				}
			}
		}
	})
}

func TestInteractiveWorkerRetryPolicyUnchanged(t *testing.T) {
	worker := newWorker(nil, LaneInteractive, "worker", WithRetryBase(2*time.Second), WithRetryJitter(func(delay time.Duration) time.Duration { return delay }))
	for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := worker.retryDelay(attempt+1, testRetryHint{time.Hour, true}); got != want {
			t.Fatalf("attempt %d = %s, want %s", attempt+1, got, want)
		}
	}
	if got := worker.retryDelay(1, testRetryError{delay: 7 * time.Second}); got != 7*time.Second {
		t.Fatalf("legacy delay = %s", got)
	}
	if got := worker.retryDelay(3, testRetryError{delay: time.Second}); got != 8*time.Second {
		t.Fatalf("legacy smaller provider delay = %s", got)
	}
}

func TestCatalogRetryErrorSanitizesAndPreservesCauseAndHint(t *testing.T) {
	cause := errors.New("sensitive-upstream-body")
	delay := 2 * time.Hour
	err := NewCatalogRetryError(cause, "rate_limited", &delay)
	delay = time.Nanosecond
	var hint RetryHint
	var reason safeReasoner
	var disposition retryDisposition
	if !errors.Is(err, cause) || !errors.As(err, &hint) || !errors.As(err, &reason) || !errors.As(err, &disposition) || !disposition.Retryable() {
		t.Fatal("lost retry error interfaces")
	}
	if got, valid := hint.RetryAfter(); !valid || got != 2*time.Hour {
		t.Fatalf("hint = %s,%v", got, valid)
	}
	if err.Error() != "rate_limited" || reason.SafeReason() != "rate_limited" || strings.Contains(fmt.Sprint(err), cause.Error()) {
		t.Fatalf("unsafe error %q", err)
	}
	for _, value := range []*time.Duration{nil, new(time.Duration)} {
		fallback := NewCatalogRetryError(cause, "sensitive-upstream-body", value)
		if fallback.Error() != "request_failed" {
			t.Fatalf("unsafe fallback %q", fallback)
		}
		if _, valid := fallback.(RetryHint).RetryAfter(); valid {
			t.Fatal("invalid hint accepted")
		}
	}
}

func TestCatalogWorkerRecordsConfiguredRetryDelay(t *testing.T) {
	lease := testLease()
	lease.Lane, lease.Attempts, lease.MaxAttempts = LaneCatalog, 2, 4
	started := time.Now()
	queue := &fakeWorkerQueue{fail: func(_ context.Context, _ Lease, _ error, retryAt time.Time) (FailureOutcome, error) {
		if got := retryAt.Sub(started); got < 5*time.Minute || got > 5*time.Minute+time.Second {
			t.Fatalf("recorded retry delay = %s", got)
		}
		return FailureRetryScheduled, nil
	}}
	worker := newWorker(queue, LaneCatalog, "worker", WithRetrySchedule(time.Minute, 5*time.Minute, 30*time.Minute), WithRetryJitter(func(delay time.Duration) time.Duration { return delay }))
	if err := worker.failWithTimeout(context.Background(), lease, NewCatalogRetryError(errors.New("upstream"), "request_failed", nil), started); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogFourthFailureIsTerminal(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	id, err := queue.Enqueue(ctx, Spec{Lane: LaneCatalog, Kind: "catalog-retry-test", IdempotencyKey: "catalog-four-attempts", MaxAttempts: 4, Optional: true})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(queue, LaneCatalog, "worker", WithRetrySchedule(time.Minute, 5*time.Minute, 30*time.Minute))
	for attempt := 1; attempt <= 4; attempt++ {
		lease, err := queue.Claim(ctx, LaneCatalog, "worker", time.Minute)
		if err != nil || lease == nil || lease.Attempts != attempt {
			t.Fatalf("attempt %d claim=%v err=%v", attempt, lease, err)
		}
		if err := worker.failWithTimeout(ctx, *lease, NewCatalogRetryError(errors.New("upstream-canary"), "request_failed", nil), time.Now()); err != nil {
			t.Fatal(err)
		}
		var status, safeError string
		if err := db.Pool.QueryRow(ctx, `SELECT status,last_error FROM sync_jobs WHERE id=$1`, id).Scan(&status, &safeError); err != nil {
			t.Fatal(err)
		}
		want := "retryable"
		if attempt == 4 {
			want = "dead"
		}
		if status != want || safeError != "request_failed" {
			t.Fatalf("attempt %d status=%s error=%q", attempt, status, safeError)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET available_at=now() WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if lease, err := queue.Claim(ctx, LaneCatalog, "worker", time.Minute); err != nil || lease != nil {
		t.Fatalf("terminal job reclaimed: %v,%v", lease, err)
	}
}
