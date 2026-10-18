package jobs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"next-game-crush/internal/observability"
)

type Handler func(context.Context, Lease) error

type safeReasoner interface {
	SafeReason() string
}

type retryDelayProvider interface {
	RetryDelay() time.Duration
}

type workerQueue interface {
	Claim(context.Context, Lane, string, time.Duration) (*Lease, error)
	Renew(context.Context, Lease, time.Duration) error
	Complete(context.Context, Lease) error
	Fail(context.Context, Lease, error, time.Time) (FailureOutcome, error)
}

type Worker struct {
	lane            Lane
	queue           workerQueue
	workerID        string
	concurrency     int
	pollInterval    time.Duration
	leaseDuration   time.Duration
	retryBase       time.Duration
	retrySchedule   []time.Duration
	retryAfterCap   time.Duration
	retryJitter     RetryJitter
	shutdownTimeout time.Duration
	heartbeat       time.Duration
	logger          *observability.Logger

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

// WithRetrySchedule selects delays by post-failure retry number instead of the
// interactive worker's exponential backoff. The last delay is the upper bound.
func WithRetrySchedule(delays ...time.Duration) WorkerOption {
	schedule := append([]time.Duration(nil), delays...)
	return func(worker *Worker) { worker.retrySchedule = schedule }
}

func WithRetryAfterCap(duration time.Duration) WorkerOption {
	return func(worker *Worker) { worker.retryAfterCap = duration }
}

func WithRetryJitter(jitter RetryJitter) WorkerOption {
	return func(worker *Worker) { worker.retryJitter = jitter }
}

func WithShutdownTimeout(duration time.Duration) WorkerOption {
	return func(worker *Worker) { worker.shutdownTimeout = duration }
}

func WithHeartbeatInterval(interval time.Duration) WorkerOption {
	return func(worker *Worker) { worker.heartbeat = interval }
}

func WithLogger(logger *observability.Logger) WorkerOption {
	return func(worker *Worker) { worker.logger = logger }
}

func WithConcurrency(concurrency int) WorkerOption {
	return func(worker *Worker) { worker.concurrency = concurrency }
}

func NewWorker(queue *Queue, lane Lane, workerID string, options ...WorkerOption) *Worker {
	if queue == nil {
		return newWorker(nil, lane, workerID, options...)
	}
	return newWorker(queue, lane, workerID, options...)
}

func newWorker(queue workerQueue, lane Lane, workerID string, options ...WorkerOption) *Worker {
	worker := &Worker{
		lane: lane, queue: queue, workerID: workerID, concurrency: 1, pollInterval: 250 * time.Millisecond,
		leaseDuration: 30 * time.Second, retryBase: time.Second, handlers: make(map[string]Handler),
		shutdownTimeout: 10 * time.Second, heartbeat: 15 * time.Second, logger: observability.New(nil),
		retryJitter: jitter,
	}
	for _, option := range options {
		option(worker)
	}
	if worker.logger == nil {
		worker.logger = observability.New(nil)
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
	if !worker.lane.valid() {
		return errors.New("worker lane must be interactive or catalog")
	}
	if worker.queue == nil {
		return errors.New("job queue is required")
	}
	if worker.workerID == "" {
		return errors.New("worker ID is required")
	}
	if worker.concurrency < 1 || worker.concurrency > 64 {
		return errors.New("worker concurrency must be between 1 and 64")
	}
	if worker.pollInterval <= 0 || worker.leaseDuration <= 0 || worker.retryBase <= 0 || worker.shutdownTimeout <= 0 || worker.heartbeat <= 0 {
		return errors.New("worker durations must be positive")
	}
	if worker.retryAfterCap < 0 || worker.retryJitter == nil {
		return errors.New("worker retry policy is invalid")
	}
	for _, delay := range worker.retrySchedule {
		if delay <= 0 {
			return errors.New("worker retry schedule durations must be positive")
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, worker.concurrency)
	for lane := 1; lane <= worker.concurrency; lane++ {
		laneID := fmt.Sprintf("%s:lane-%02d", worker.workerID, lane)
		go func() { results <- worker.runLane(runCtx, laneID) }()
	}
	var firstErr error
	for range worker.concurrency {
		if err := <-results; err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}

func (worker *Worker) runLane(ctx context.Context, workerID string) error {
	worker.logger.Worker(ctx, observability.WorkerEvent{Lane: string(worker.lane), Stage: observability.WorkerStarted})
	defer worker.logger.Worker(context.WithoutCancel(ctx), observability.WorkerEvent{Lane: string(worker.lane), Stage: observability.WorkerStopped})
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		lease, err := worker.queue.Claim(ctx, worker.lane, workerID, worker.leaseDuration)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			worker.logger.Worker(ctx, observability.WorkerEvent{Lane: string(worker.lane), Stage: observability.WorkerClaimFailed, Reason: observability.ReasonPersistenceFailed})
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
			if errors.Is(err, ErrLeaseLost) {
				continue
			}
			return err
		}
	}
}

func (worker *Worker) process(ctx context.Context, lease Lease) error {
	correlation := observability.Correlation{WorkflowID: lease.WorkflowID, JobID: lease.ID, ParentJobID: lease.ParentJobID}
	ctx = observability.WithCorrelation(ctx, correlation)
	started := time.Now()
	worker.logAttempt(ctx, lease, observability.JobStarted, 0, 0, "")
	worker.mu.RLock()
	handler := worker.handlers[lease.Kind]
	worker.mu.RUnlock()
	if handler == nil {
		return worker.failWithTimeout(ctx, lease, errors.New("no handler registered"), started)
	}

	handlerCtx, cancelHandler := context.WithCancel(ctx)
	defer cancelHandler()
	result := make(chan error, 1)
	go func() { result <- handler(handlerCtx, lease) }()
	renewEvery := worker.leaseDuration / 2
	if renewEvery <= 0 {
		renewEvery = time.Millisecond
	}
	ticker := time.NewTicker(renewEvery)
	defer ticker.Stop()
	heartbeat := time.NewTicker(worker.heartbeat)
	defer heartbeat.Stop()
	for {
		if ctx.Err() != nil {
			return worker.waitForCancellation(ctx, lease, result, started)
		}
		select {
		case err := <-result:
			if ctx.Err() != nil {
				worker.logAttempt(context.WithoutCancel(ctx), lease, observability.JobCancelled, time.Since(started), 0, "")
				return nil
			}
			if err != nil {
				if errors.Is(err, ErrDeferred) {
					worker.logAttempt(ctx, lease, observability.JobDeferred, time.Since(started), 0, "")
					return nil
				}
				return worker.failWithTimeout(ctx, lease, err, started)
			}
			operationCtx, cancel := detachedTimeout(ctx)
			completeErr := worker.queue.Complete(operationCtx, lease)
			if completeErr != nil {
				reason := persistenceReason(completeErr)
				worker.logger.Worker(operationCtx, observability.WorkerEvent{Lane: string(worker.lane), Stage: observability.WorkerCompletionFailed, Correlation: correlation, Reason: reason})
				if errors.Is(completeErr, ErrLeaseLost) {
					worker.logAttempt(operationCtx, lease, observability.JobLeaseLost, time.Since(started), 0, reason)
				}
				cancel()
				return completeErr
			}
			worker.logAttempt(operationCtx, lease, observability.JobSucceeded, time.Since(started), 0, "")
			cancel()
			return nil
		case <-ticker.C:
			if err := worker.queue.Renew(ctx, lease, worker.leaseDuration); err != nil {
				if ctx.Err() != nil {
					return worker.waitForCancellation(ctx, lease, result, started)
				}
				reason := persistenceReason(err)
				worker.logger.Worker(ctx, observability.WorkerEvent{Lane: string(worker.lane), Stage: observability.WorkerRenewalFailed, Correlation: correlation, Reason: reason})
				if errors.Is(err, ErrLeaseLost) {
					worker.logAttempt(ctx, lease, observability.JobLeaseLost, time.Since(started), 0, reason)
					cancelHandler()
					worker.waitForHandler(result)
				}
				return err
			}
		case <-heartbeat.C:
			if ctx.Err() != nil {
				return worker.waitForCancellation(ctx, lease, result, started)
			}
			worker.logAttempt(ctx, lease, observability.JobHeartbeat, 0, 0, "")
		case <-ctx.Done():
			return worker.waitForCancellation(ctx, lease, result, started)
		}
	}
}

func (worker *Worker) waitForCancellation(ctx context.Context, lease Lease, result <-chan error, started time.Time) error {
	worker.waitForHandler(result)
	worker.logAttempt(context.WithoutCancel(ctx), lease, observability.JobCancelled, time.Since(started), 0, "")
	return nil
}

func (worker *Worker) waitForHandler(result <-chan error) {
	timer := time.NewTimer(worker.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-result:
	case <-timer.C:
	}
}

func (worker *Worker) failWithTimeout(ctx context.Context, lease Lease, cause error, started time.Time) error {
	operationCtx, cancel := detachedTimeout(ctx)
	defer cancel()
	retryDelay := worker.retryDelay(lease.Attempts, cause)
	outcome, err := worker.queue.Fail(operationCtx, lease, cause, time.Now().Add(retryDelay))
	if err != nil {
		reason := persistenceReason(err)
		worker.logger.Worker(operationCtx, observability.WorkerEvent{Lane: string(worker.lane), Stage: observability.WorkerFailureRecordFailed, Correlation: observability.Correlation{WorkflowID: lease.WorkflowID, JobID: lease.ID, ParentJobID: lease.ParentJobID}, Reason: reason})
		if errors.Is(err, ErrLeaseLost) {
			worker.logAttempt(operationCtx, lease, observability.JobLeaseLost, time.Since(started), 0, reason)
		}
		return err
	}
	stage := observability.JobRetryScheduled
	delay := retryDelay
	if outcome == FailureDead {
		stage = observability.JobDead
		delay = 0
	}
	worker.logAttempt(operationCtx, lease, stage, time.Since(started), delay, failureReason(cause))
	return nil
}

func (worker *Worker) logAttempt(ctx context.Context, lease Lease, stage observability.JobStage, duration, retryDelay time.Duration, reason observability.Reason) {
	worker.logger.JobAttempt(ctx, observability.JobAttemptEvent{
		Stage: stage, Correlation: observability.Correlation{WorkflowID: lease.WorkflowID, JobID: lease.ID, ParentJobID: lease.ParentJobID},
		Lane: string(lease.Lane), Kind: lease.Kind, LogicalStage: lease.Stage, Attempt: lease.Attempts, MaxAttempts: lease.MaxAttempts,
		Required: lease.Required, Duration: duration, RetryDelay: retryDelay, Reason: reason,
	})
}

func failureReason(err error) observability.Reason {
	if errors.Is(err, context.DeadlineExceeded) {
		return observability.ReasonTimeout
	}
	if errors.Is(err, ErrLeaseLost) {
		return observability.ReasonLeaseLost
	}
	var classified safeReasoner
	if errors.As(err, &classified) {
		switch classified.SafeReason() {
		case string(observability.ReasonInvalidEvidence):
			return observability.ReasonInvalidEvidence
		case string(observability.ReasonInvalidOutput):
			return observability.ReasonInvalidOutput
		}
	}
	return observability.ReasonInternalError
}

func persistenceReason(err error) observability.Reason {
	if errors.Is(err, ErrLeaseLost) {
		return observability.ReasonLeaseLost
	}
	return observability.ReasonPersistenceFailed
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

func (worker *Worker) retryDelay(attempts int, cause error) time.Duration {
	if len(worker.retrySchedule) > 0 {
		var hint RetryHint
		if errors.As(cause, &hint) {
			if delay, valid := hint.RetryAfter(); valid && delay > 0 {
				if worker.retryAfterCap > 0 && delay > worker.retryAfterCap {
					delay = worker.retryAfterCap
				}
				return delay
			}
		}
		index := max(0, min(attempts-1, len(worker.retrySchedule)-1))
		return worker.retryJitter(worker.retrySchedule[index])
	}
	delay := worker.retryJitter(worker.backoff(attempts))
	var provider retryDelayProvider
	if errors.As(cause, &provider) && provider.RetryDelay() > delay {
		delay = provider.RetryDelay()
	}
	return delay
}

func jitter(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

func detachedTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
