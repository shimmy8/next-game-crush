package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/observability"
)

type fakeWorkerQueue struct {
	claim    func(context.Context, Lane, string, time.Duration) (*Lease, error)
	renew    func(context.Context, Lease, time.Duration) error
	complete func(context.Context, Lease) error
	fail     func(context.Context, Lease, error, time.Time) (FailureOutcome, error)
}

type testSafeReasonError struct {
	reason string
}

func (err testSafeReasonError) Error() string      { return "handler-error-canary" }
func (err testSafeReasonError) SafeReason() string { return err.reason }

type testRetryError struct {
	retryable bool
	delay     time.Duration
}

func (err testRetryError) Error() string             { return "retry-error-canary" }
func (err testRetryError) Retryable() bool           { return err.retryable }
func (err testRetryError) RetryDelay() time.Duration { return err.delay }

func (q *fakeWorkerQueue) Claim(ctx context.Context, lane Lane, worker string, duration time.Duration) (*Lease, error) {
	if q.claim != nil {
		return q.claim(ctx, lane, worker, duration)
	}
	return nil, nil
}

func (q *fakeWorkerQueue) Renew(ctx context.Context, lease Lease, duration time.Duration) error {
	if q.renew != nil {
		return q.renew(ctx, lease, duration)
	}
	return nil
}

func (q *fakeWorkerQueue) Complete(ctx context.Context, lease Lease) error {
	if q.complete != nil {
		return q.complete(ctx, lease)
	}
	return nil
}

func (q *fakeWorkerQueue) Fail(ctx context.Context, lease Lease, cause error, retryAt time.Time) (FailureOutcome, error) {
	if q.fail != nil {
		return q.fail(ctx, lease, cause, retryAt)
	}
	return FailureRetryScheduled, nil
}

func TestWorkerLifecycleLogsSuccessAndInjectsCorrelation(t *testing.T) {
	var output bytes.Buffer
	lease := testLease()
	worker := newWorker(&fakeWorkerQueue{}, LaneInteractive, "worker-canary", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&output, nil)))))
	if err := worker.Register(lease.Kind, func(ctx context.Context, _ Lease) error {
		if got := observability.CorrelationFromContext(ctx); got.WorkflowID != lease.WorkflowID || got.JobID != lease.ID || got.ParentJobID != lease.ParentJobID {
			t.Fatalf("handler correlation=%#v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := worker.process(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	stages := workerLogStages(t, output.String(), "job_attempt")
	if strings.Join(stages, ",") != "started,succeeded" {
		t.Fatalf("stages=%v logs=%s", stages, output.String())
	}
}

func TestWorkerHeartbeatStartsAfterFullInterval(t *testing.T) {
	var shortOutput bytes.Buffer
	shortLease := testLease()
	shortWorker := newWorker(&fakeWorkerQueue{}, LaneInteractive, "worker", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&shortOutput, nil)))), WithHeartbeatInterval(30*time.Millisecond), WithLeaseDuration(time.Second))
	if err := shortWorker.Register(shortLease.Kind, func(context.Context, Lease) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := shortWorker.process(context.Background(), shortLease); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shortOutput.String(), `"stage":"heartbeat"`) {
		t.Fatal("short attempt emitted heartbeat")
	}

	var output bytes.Buffer
	lease := testLease()
	release := make(chan struct{})
	worker := newWorker(&fakeWorkerQueue{}, LaneInteractive, "worker", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&output, nil)))), WithHeartbeatInterval(15*time.Millisecond), WithLeaseDuration(time.Second))
	if err := worker.Register(lease.Kind, func(context.Context, Lease) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.process(context.Background(), lease) }()
	time.Sleep(25 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stages := workerLogStages(t, output.String(), "job_attempt")
	heartbeats := 0
	for _, stage := range stages {
		if stage == "heartbeat" {
			heartbeats++
		}
	}
	if heartbeats == 0 || heartbeats > 2 {
		t.Fatalf("heartbeat count=%d stages=%v", heartbeats, stages)
	}
}

func TestWorkerFailureLogsDispositionSeverityAndNoCanaries(t *testing.T) {
	for _, test := range []struct {
		name     string
		required bool
		outcome  FailureOutcome
		stage    string
		level    string
	}{
		{name: "retry", required: true, outcome: FailureRetryScheduled, stage: "retry_scheduled", level: "WARN"},
		{name: "required dead", required: true, outcome: FailureDead, stage: "dead", level: "ERROR"},
		{name: "optional dead", required: false, outcome: FailureDead, stage: "dead", level: "WARN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			lease := testLease()
			lease.Required = test.required
			lease.Payload = json.RawMessage(`{"secret":"payload-canary"}`)
			lease.IdempotencyKey = "idempotency-canary"
			lease.WorkerID = "worker-canary"
			queue := &fakeWorkerQueue{fail: func(context.Context, Lease, error, time.Time) (FailureOutcome, error) { return test.outcome, nil }}
			worker := newWorker(queue, LaneInteractive, "worker-id-canary", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&output, nil)))))
			if err := worker.Register(lease.Kind, func(context.Context, Lease) error { return errors.New("handler-error-canary") }); err != nil {
				t.Fatal(err)
			}
			if err := worker.process(context.Background(), lease); err != nil {
				t.Fatal(err)
			}
			events := workerLogEvents(t, output.String())
			last := events[len(events)-1]
			if last["stage"] != test.stage || last["level"] != test.level {
				t.Fatalf("last=%#v", last)
			}
			for _, canary := range []string{"payload-canary", "idempotency-canary", "worker-canary", "worker-id-canary", "handler-error-canary", lease.FencingToken.String()} {
				if strings.Contains(output.String(), canary) {
					t.Fatalf("canary %q leaked in %s", canary, output.String())
				}
			}
		})
	}
}

func TestWorkerFailureHonorsRetryDispositionAndProviderDelay(t *testing.T) {
	lease := testLease()
	lease.Attempts = 1
	lease.MaxAttempts = 3
	started := time.Now()
	queue := &fakeWorkerQueue{fail: func(_ context.Context, got Lease, _ error, retryAt time.Time) (FailureOutcome, error) {
		if got.Attempts != lease.Attempts {
			t.Fatalf("attempts = %d, want %d", got.Attempts, lease.Attempts)
		}
		if retryAt.Sub(started) < 7*time.Second {
			t.Fatalf("retry delay = %s, want at least 7s", retryAt.Sub(started))
		}
		return FailureRetryScheduled, nil
	}}
	worker := newWorker(queue, LaneInteractive, "worker", WithRetryBase(time.Second))
	if err := worker.failWithTimeout(context.Background(), lease, testRetryError{retryable: true, delay: 7 * time.Second}, started); err != nil {
		t.Fatal(err)
	}
}

func TestFailureReasonUsesOnlyAllowlistedSafeReasons(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		reason observability.Reason
	}{
		{name: "invalid evidence", err: fmt.Errorf("wrapped: %w", testSafeReasonError{reason: "invalid_evidence"}), reason: "invalid_evidence"},
		{name: "invalid output", err: fmt.Errorf("wrapped: %w", testSafeReasonError{reason: "invalid_output"}), reason: "invalid_output"},
		{name: "hostile reason", err: testSafeReasonError{reason: "payload-canary"}, reason: observability.ReasonInternalError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := failureReason(test.err); got != test.reason {
				t.Fatalf("failureReason() = %q, want %q", got, test.reason)
			}
		})
	}
}

func TestWorkerLifecycleLogsDeferredAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name  string
		run   func(*Worker, Lease) error
		stage string
	}{
		{name: "deferred", stage: "deferred", run: func(worker *Worker, lease Lease) error {
			if err := worker.Register(lease.Kind, func(context.Context, Lease) error { return ErrDeferred }); err != nil {
				return err
			}
			return worker.process(context.Background(), lease)
		}},
		{name: "cancelled", stage: "cancelled", run: func(worker *Worker, lease Lease) error {
			ctx, cancel := context.WithCancel(context.Background())
			if err := worker.Register(lease.Kind, func(ctx context.Context, _ Lease) error { <-ctx.Done(); return ctx.Err() }); err != nil {
				return err
			}
			cancel()
			return worker.process(ctx, lease)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			worker := newWorker(&fakeWorkerQueue{}, LaneInteractive, "worker", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&output, nil)))))
			if err := test.run(worker, testLease()); err != nil {
				t.Fatal(err)
			}
			stages := workerLogStages(t, output.String(), "job_attempt")
			if stages[len(stages)-1] != test.stage {
				t.Fatalf("stages=%v", stages)
			}
		})
	}
}

func TestWorkerInfrastructureAndLeaseLossLogsAreBounded(t *testing.T) {
	tests := []struct {
		name        string
		queue       *fakeWorkerQueue
		handler     Handler
		workerStage string
		jobStage    string
	}{
		{name: "completion", queue: &fakeWorkerQueue{complete: func(context.Context, Lease) error { return errors.New("completion-canary") }}, handler: func(context.Context, Lease) error { return nil }, workerStage: "completion_failed"},
		{name: "failure record", queue: &fakeWorkerQueue{fail: func(context.Context, Lease, error, time.Time) (FailureOutcome, error) {
			return "", errors.New("failure-record-canary")
		}}, handler: func(context.Context, Lease) error { return errors.New("handler-canary") }, workerStage: "failure_record_failed"},
		{name: "renewal lease loss", queue: &fakeWorkerQueue{renew: func(context.Context, Lease, time.Duration) error { return ErrLeaseLost }}, handler: func(ctx context.Context, _ Lease) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
				return nil
			}
		}, workerStage: "renewal_failed", jobStage: "lease_lost"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			worker := newWorker(test.queue, LaneInteractive, "worker-canary", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&output, nil)))), WithLeaseDuration(10*time.Millisecond), WithHeartbeatInterval(time.Second))
			lease := testLease()
			if err := worker.Register(lease.Kind, test.handler); err != nil {
				t.Fatal(err)
			}
			if err := worker.process(context.Background(), lease); err == nil {
				t.Fatal("infrastructure failure returned nil")
			}
			stages := workerLogStages(t, output.String(), "worker")
			if len(stages) == 0 || stages[len(stages)-1] != test.workerStage {
				t.Fatalf("worker stages=%v logs=%s", stages, output.String())
			}
			if test.jobStage != "" {
				jobStages := workerLogStages(t, output.String(), "job_attempt")
				if jobStages[len(jobStages)-1] != test.jobStage {
					t.Fatalf("job stages=%v", jobStages)
				}
			}
			if strings.Contains(output.String(), "canary") {
				t.Fatalf("canary leaked: %s", output.String())
			}
		})
	}
}

func TestWorkerRunLogsStartClaimFailureAndStop(t *testing.T) {
	var output bytes.Buffer
	queue := &fakeWorkerQueue{claim: func(context.Context, Lane, string, time.Duration) (*Lease, error) {
		return nil, errors.New("claim-canary")
	}}
	worker := newWorker(queue, LaneInteractive, "worker-canary", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&output, nil)))))
	if err := worker.Run(context.Background()); err == nil {
		t.Fatal("Run returned nil")
	}
	stages := workerLogStages(t, output.String(), "worker")
	if strings.Join(stages, ",") != "started,claim_failed,stopped" {
		t.Fatalf("stages=%v logs=%s", stages, output.String())
	}
	if strings.Contains(output.String(), "canary") {
		t.Fatalf("canary leaked: %s", output.String())
	}
}

func TestWorkerRunUsesConfiguredConcurrentUniqueLanes(t *testing.T) {
	const concurrency = 10
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	claimedBy := make(map[string]struct{}, concurrency)
	allLanes := make(chan struct{})
	queue := &fakeWorkerQueue{claim: func(ctx context.Context, _ Lane, workerID string, _ time.Duration) (*Lease, error) {
		mu.Lock()
		claimedBy[workerID] = struct{}{}
		if len(claimedBy) == concurrency {
			select {
			case <-allLanes:
			default:
				close(allLanes)
			}
		}
		mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	worker := newWorker(queue, LaneInteractive, "worker", WithConcurrency(concurrency))
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case <-allLanes:
	case <-time.After(time.Second):
		t.Fatal("worker pool did not start every lane")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for lane := 1; lane <= concurrency; lane++ {
		want := fmt.Sprintf("worker:lane-%02d", lane)
		if _, ok := claimedBy[want]; !ok {
			t.Errorf("missing lane ID %q in %#v", want, claimedBy)
		}
	}
}

func TestWorkerRunCancelsSiblingLanesAfterInfrastructureFailure(t *testing.T) {
	want := errors.New("claim failed")
	var mu sync.Mutex
	entered := 0
	allEntered := make(chan struct{})
	siblingsStopped := make(chan struct{}, 2)
	queue := &fakeWorkerQueue{claim: func(ctx context.Context, _ Lane, workerID string, _ time.Duration) (*Lease, error) {
		mu.Lock()
		entered++
		if entered == 3 {
			close(allEntered)
		}
		mu.Unlock()
		<-allEntered
		if strings.HasSuffix(workerID, "lane-01") {
			return nil, want
		}
		<-ctx.Done()
		siblingsStopped <- struct{}{}
		return nil, ctx.Err()
	}}
	worker := newWorker(queue, LaneInteractive, "worker", WithConcurrency(3))
	if err := worker.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
	for range 2 {
		select {
		case <-siblingsStopped:
		case <-time.After(time.Second):
			t.Fatal("sibling lane was not cancelled")
		}
	}
}

func TestWorkerRunContinuesAfterLeaseLossAndStopsHandlerBeforeNextClaim(t *testing.T) {
	lease := testLease()
	handlerStarted := make(chan struct{})
	handlerStopped := make(chan struct{})
	cleanup := make(chan struct{})
	workerDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(cleanup)

	claims := 0
	queue := &fakeWorkerQueue{
		claim: func(context.Context, Lane, string, time.Duration) (*Lease, error) {
			claims++
			switch claims {
			case 1:
				return &lease, nil
			case 2:
				select {
				case <-handlerStopped:
				case <-time.After(100 * time.Millisecond):
					return nil, errors.New("worker claimed again before the lease-lost handler stopped")
				}
				cancel()
				return nil, nil
			default:
				return nil, errors.New("unexpected extra claim")
			}
		},
		renew: func(context.Context, Lease, time.Duration) error {
			return ErrLeaseLost
		},
		complete: func(context.Context, Lease) error {
			return errors.New("lease-lost handler completed unexpectedly")
		},
		fail: func(context.Context, Lease, error, time.Time) (FailureOutcome, error) {
			return "", errors.New("lease-lost handler failed unexpectedly")
		},
	}
	worker := newWorker(queue, LaneInteractive, "worker", WithLeaseDuration(10*time.Millisecond), WithPollInterval(time.Millisecond), WithHeartbeatInterval(time.Second))
	if err := worker.Register(lease.Kind, func(ctx context.Context, _ Lease) error {
		close(handlerStarted)
		defer close(handlerStopped)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cleanup:
			return errors.New("test cleanup released an uncancelled handler")
		}
	}); err != nil {
		t.Fatal(err)
	}

	go func() { workerDone <- worker.Run(ctx) }()
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("Run returned after recoverable lease loss: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not continue after lease loss")
	}
	if claims != 2 {
		t.Fatalf("claims=%d, want 2", claims)
	}
}

func TestWorkerRunBoundsLeaseLossWaitWhenHandlerIgnoresCancellation(t *testing.T) {
	lease := testLease()
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	workerDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	claims := 0
	queue := &fakeWorkerQueue{
		claim: func(context.Context, Lane, string, time.Duration) (*Lease, error) {
			claims++
			if claims == 1 {
				return &lease, nil
			}
			cancel()
			return nil, nil
		},
		renew: func(context.Context, Lease, time.Duration) error { return ErrLeaseLost },
	}
	worker := newWorker(queue, LaneInteractive, "worker", WithLeaseDuration(10*time.Millisecond), WithPollInterval(time.Millisecond), WithHeartbeatInterval(time.Second), WithShutdownTimeout(20*time.Millisecond))
	if err := worker.Register(lease.Kind, func(context.Context, Lease) error {
		close(handlerStarted)
		<-releaseHandler
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	go func() { workerDone <- worker.Run(ctx) }()
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		close(releaseHandler)
		t.Fatal("handler did not start")
	}
	select {
	case err := <-workerDone:
		close(releaseHandler)
		if err != nil {
			t.Fatalf("Run returned after recoverable lease loss: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(releaseHandler)
		<-workerDone
		t.Fatal("worker remained blocked on a handler that ignored lease-loss cancellation")
	}
	if claims != 2 {
		t.Fatalf("claims=%d, want 2", claims)
	}
}

func TestWorkerRunReturnsNonLeaseProcessFailure(t *testing.T) {
	lease := testLease()
	want := errors.New("completion failed")
	claims := 0
	queue := &fakeWorkerQueue{
		claim: func(context.Context, Lane, string, time.Duration) (*Lease, error) {
			claims++
			return &lease, nil
		},
		complete: func(context.Context, Lease) error { return want },
	}
	worker := newWorker(queue, LaneInteractive, "worker")
	if err := worker.Register(lease.Kind, func(context.Context, Lease) error { return nil }); err != nil {
		t.Fatal(err)
	}

	if err := worker.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error=%v, want %v", err, want)
	}
	if claims != 1 {
		t.Fatalf("claims=%d, want 1", claims)
	}
}

func TestWorkerCancellationWinsOverReadySuccessAndRenewal(t *testing.T) {
	for _, test := range []struct {
		name  string
		queue *fakeWorkerQueue
	}{
		{name: "success", queue: &fakeWorkerQueue{}},
		{name: "renewal", queue: &fakeWorkerQueue{renew: func(ctx context.Context, _ Lease, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			lease := testLease()
			release := make(chan struct{})
			worker := newWorker(test.queue, LaneInteractive, "worker", WithLogger(observability.New(slog.New(slog.NewJSONHandler(&output, nil)))), WithLeaseDuration(2*time.Millisecond), WithHeartbeatInterval(2*time.Millisecond))
			if err := worker.Register(lease.Kind, func(context.Context, Lease) error { <-release; return nil }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- worker.process(ctx, lease) }()
			time.Sleep(5 * time.Millisecond)
			cancel()
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			stages := workerLogStages(t, output.String(), "job_attempt")
			if stages[len(stages)-1] != "cancelled" {
				t.Fatalf("terminal stages=%v logs=%s", stages, output.String())
			}
			if strings.Contains(output.String(), `"stage":"succeeded"`) || strings.Contains(output.String(), `"stage":"renewal_failed"`) {
				t.Fatalf("cancellation misclassified: %s", output.String())
			}
		})
	}
}

func testLease() Lease {
	return Lease{Lane: LaneInteractive, ID: uuid.New(), ParentJobID: uuid.New(), WorkflowID: uuid.New(), Kind: "steam.sync", Required: true, Stage: "steam", Attempts: 1, MaxAttempts: 3, WorkerID: "worker", FencingToken: uuid.New()}
}

func TestWorkerBindsEveryClaimToConfiguredLane(t *testing.T) {
	for _, lane := range []Lane{LaneInteractive, LaneCatalog} {
		t.Run(string(lane), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			queue := &fakeWorkerQueue{claim: func(ctx context.Context, got Lane, _ string, _ time.Duration) (*Lease, error) {
				if got != lane {
					t.Errorf("claim lane=%s, want %s", got, lane)
				}
				if calls.Add(1) == 4 {
					cancel()
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			if err := newWorker(queue, lane, "worker", WithConcurrency(4)).Run(ctx); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 4 {
				t.Fatalf("claims=%d, want 4", calls.Load())
			}
		})
	}
}

func TestWorkerRejectsInvalidLaneBeforeClaim(t *testing.T) {
	for _, lane := range []Lane{"", "invalid"} {
		queue := &fakeWorkerQueue{claim: func(context.Context, Lane, string, time.Duration) (*Lease, error) {
			t.Fatal("invalid worker reached queue")
			return nil, nil
		}}
		if err := newWorker(queue, lane, "worker").Run(context.Background()); err == nil {
			t.Fatal("invalid worker lane accepted")
		}
	}
}

func workerLogStages(t *testing.T, raw, eventName string) []string {
	t.Helper()
	events := workerLogEvents(t, raw)
	stages := make([]string, 0, len(events))
	for _, event := range events {
		if event["event"] == eventName {
			stages = append(stages, event["stage"].(string))
		}
	}
	return stages
}

func workerLogEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		result = append(result, event)
	}
	return result
}

func TestWorkerRegisterRejectsInvalidAndDuplicateKinds(t *testing.T) {
	queue, _ := openTestQueue(t)
	worker := NewWorker(queue, LaneInteractive, "test-worker", WithPollInterval(5*time.Millisecond))
	handler := func(context.Context, Lease) error { return nil }
	if err := worker.Register("", handler); err == nil {
		t.Fatal("empty kind registered")
	}
	if err := worker.Register("sync", nil); err == nil {
		t.Fatal("nil handler registered")
	}
	if err := worker.Register("sync", handler); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("sync", handler); err == nil {
		t.Fatal("duplicate kind registered")
	}
}

func TestWorkerRunsRegisteredHandlerAndCompletes(t *testing.T) {
	queue, db := openTestQueue(t)
	id, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "worker-complete"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(queue, LaneInteractive, "test-worker", WithPollInterval(5*time.Millisecond), WithLeaseDuration(time.Second))
	handled := make(chan struct{}, 1)
	if err := worker.Register("sync", func(context.Context, Lease) error {
		handled <- struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var status string
		err := db.Pool.QueryRow(context.Background(), "SELECT status FROM sync_jobs WHERE id=$1", id).Scan(&status)
		if err == nil && status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete: status=%q err=%v", status, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not shut down")
	}
}

func TestWorkerGracefulShutdownCancelsAndWaitsForHandler(t *testing.T) {
	queue, _ := openTestQueue(t)
	_, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "worker-shutdown"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(queue, LaneInteractive, "test-worker", WithPollInterval(time.Millisecond))
	started := make(chan struct{})
	stopped := make(chan struct{})
	if err := worker.Register("sync", func(ctx context.Context, _ Lease) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("handler was not canceled")
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not wait for handler shutdown")
	}
}

func TestWorkerShutdownIsBoundedWhenHandlerIgnoresCancellation(t *testing.T) {
	queue, _ := openTestQueue(t)
	_, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "worker-bounded-shutdown"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(queue, LaneInteractive, "test-worker", WithPollInterval(time.Millisecond), WithShutdownTimeout(30*time.Millisecond))
	started := make(chan struct{})
	release := make(chan struct{})
	if err := worker.Register("sync", func(context.Context, Lease) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("worker shutdown exceeded its deadline")
	}
	close(release)
}
