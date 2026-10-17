package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerRegisterRejectsInvalidAndDuplicateKinds(t *testing.T) {
	queue, _ := openTestQueue(t)
	worker := NewWorker(queue, "test-worker", WithPollInterval(5*time.Millisecond))
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
	id, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "worker-complete"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(queue, "test-worker", WithPollInterval(5*time.Millisecond), WithLeaseDuration(time.Second))
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
	_, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "worker-shutdown"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(queue, "test-worker", WithPollInterval(time.Millisecond))
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
	_, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "worker-bounded-shutdown"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(queue, "test-worker", WithPollInterval(time.Millisecond), WithShutdownTimeout(30*time.Millisecond))
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
