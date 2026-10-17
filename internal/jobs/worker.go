package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Handler func(context.Context, Lease) error

type Worker struct {
	queue           *Queue
	workerID        string
	pollInterval    time.Duration
	leaseDuration   time.Duration
	retryBase       time.Duration
	shutdownTimeout time.Duration

	mu       sync.RWMutex
	handlers map[string]Handler
}

type WorkerOption func(*Worker)

func WithPollInterval(interval time.Duration) WorkerOption {
	return func(worker *Worker) { worker.pollInterval = interval }
}

func WithLeaseDuration(duration time.Duration) WorkerOption {
	return func(worker *Worker) { worker.leaseDuration = duration }
}

func WithRetryBase(duration time.Duration) WorkerOption {
	return func(worker *Worker) { worker.retryBase = duration }
}

func WithShutdownTimeout(duration time.Duration) WorkerOption {
	return func(worker *Worker) { worker.shutdownTimeout = duration }
}

func NewWorker(queue *Queue, workerID string, options ...WorkerOption) *Worker {
	worker := &Worker{
		queue: queue, workerID: workerID, pollInterval: 250 * time.Millisecond,
		leaseDuration: 30 * time.Second, retryBase: time.Second, handlers: make(map[string]Handler),
		shutdownTimeout: 10 * time.Second,
	}
	for _, option := range options {
		option(worker)
	}
	return worker
}

func (worker *Worker) Register(kind string, handler Handler) error {
	if kind == "" {
		return errors.New("job kind is required")
	}
	if handler == nil {
		return errors.New("job handler is required")
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if _, exists := worker.handlers[kind]; exists {
		return fmt.Errorf("handler already registered for %q", kind)
	}
	worker.handlers[kind] = handler
	return nil
}

func (worker *Worker) Run(ctx context.Context) error {
	if worker.queue == nil {
		return errors.New("job queue is required")
	}
	if worker.workerID == "" {
		return errors.New("worker ID is required")
	}
	if worker.pollInterval <= 0 || worker.leaseDuration <= 0 || worker.retryBase <= 0 || worker.shutdownTimeout <= 0 {
		return errors.New("worker durations must be positive")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		lease, err := worker.queue.Claim(ctx, worker.workerID, worker.leaseDuration)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			return err
		}
		if lease == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(worker.pollInterval):
			}
			continue
		}
		if err := worker.process(ctx, *lease); err != nil && ctx.Err() == nil {
			return err
		}
	}
}

func (worker *Worker) process(ctx context.Context, lease Lease) error {
	worker.mu.RLock()
	handler := worker.handlers[lease.Kind]
	worker.mu.RUnlock()
	if handler == nil {
		return worker.failWithTimeout(ctx, lease, fmt.Errorf("no handler registered for job kind %q", lease.Kind))
	}

	result := make(chan error, 1)
	go func() { result <- handler(ctx, lease) }()
	renewEvery := worker.leaseDuration / 2
	if renewEvery <= 0 {
		renewEvery = time.Millisecond
	}
	ticker := time.NewTicker(renewEvery)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return worker.failWithTimeout(ctx, lease, err)
			}
			operationCtx, cancel := detachedTimeout(ctx)
			defer cancel()
			return worker.queue.Complete(operationCtx, lease)
		case <-ticker.C:
			if err := worker.queue.Renew(ctx, lease, worker.leaseDuration); err != nil {
				return err
			}
		case <-ctx.Done():
			select {
			case <-result:
				return nil
			case <-time.After(worker.shutdownTimeout):
				return nil
			}
		}
	}
}

func (worker *Worker) failWithTimeout(ctx context.Context, lease Lease, cause error) error {
	operationCtx, cancel := detachedTimeout(ctx)
	defer cancel()
	return worker.queue.Fail(operationCtx, lease, cause, time.Now().Add(worker.backoff(lease.Attempts)))
}

func (worker *Worker) backoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 10 {
		attempts = 10
	}
	if attempts > 0 {
		attempts--
	}
	return worker.retryBase * time.Duration(1<<attempts)
}

func detachedTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
