package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

var ErrLeaseLost = errors.New("job lease is no longer owned")
var ErrDeferred = errors.New("job deferred")

type Spec struct {
	Kind           string
	IdempotencyKey string
	Payload        json.RawMessage
	MaxAttempts    int
	Priority       int
	AvailableAt    time.Time
	ParentJobID    uuid.UUID
	Required       bool
	Optional       bool
	Stage          string
	UserID         uuid.UUID
}

type Lease struct {
	ID             uuid.UUID
	ParentJobID    uuid.UUID
	WorkflowID     uuid.UUID
	Kind           string
	Required       bool
	Stage          string
	UserID         uuid.UUID
	IdempotencyKey string
	Payload        json.RawMessage
	Result         json.RawMessage
	Attempts       int
	MaxAttempts    int
	Priority       int
	WorkerID       string
	FencingToken   uuid.UUID
	LeaseUntil     time.Time
}

type FailureOutcome string

type retryDisposition interface {
	Retryable() bool
}

const (
	FailureRetryScheduled FailureOutcome = "retry_scheduled"
	FailureDead           FailureOutcome = "dead"
)

type Queue struct {
	store *store.Store
}

func NewQueue(db *store.Store) *Queue {
	return &Queue{store: db}
}

// RecordResult stores version metadata produced by a running handler. The
// fencing predicate prevents a stale worker from overwriting a reclaimed job.
func (q *Queue) RecordResult(ctx context.Context, lease Lease, result any) error {
	if q == nil || q.store == nil {
		return errors.New("queue store is required")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode job result: %w", err)
	}
	command, err := q.store.Pool.Exec(ctx, `UPDATE sync_jobs SET result=$1,updated_at=now()
		WHERE id=$2 AND status='running' AND worker_id=$3 AND fencing_token=$4 AND lease_until>now()`, encoded, lease.ID, lease.WorkerID, lease.FencingToken)
	if err != nil {
		return fmt.Errorf("record job result: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (q *Queue) Enqueue(ctx context.Context, spec Spec) (uuid.UUID, error) {
	if q == nil || q.store == nil {
		return uuid.Nil, errors.New("queue store is required")
	}
	return EnqueueInTx(ctx, q.store.Queries, spec)
}

// EnqueueInTx enqueues using the caller's queries, so a transaction-bound
// instance publishes the job atomically with the caller's other writes.
func EnqueueInTx(ctx context.Context, queries *dbgen.Queries, spec Spec) (uuid.UUID, error) {
	if queries == nil {
		return uuid.Nil, errors.New("queue queries are required")
	}
	if strings.TrimSpace(spec.Kind) == "" {
		return uuid.Nil, errors.New("job kind is required")
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" {
		return uuid.Nil, errors.New("idempotency key is required")
	}
	if len(spec.Payload) == 0 {
		spec.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(spec.Payload) {
		return uuid.Nil, errors.New("job payload must be valid JSON")
	}
	if spec.MaxAttempts == 0 {
		spec.MaxAttempts = 3
	}
	if spec.MaxAttempts < 1 {
		return uuid.Nil, errors.New("max attempts must be positive")
	}
	if spec.Priority == 0 {
		spec.Priority = 50
	}
	if spec.Priority < 1 || spec.Priority > 100 {
		return uuid.Nil, errors.New("priority must be between 1 and 100")
	}
	if spec.ParentJobID == uuid.Nil && !spec.Required {
		// Root jobs are required by definition; zero-value Specs remain backwards compatible.
		spec.Required = true
	}
	availableAt := pgtype.Timestamptz{}
	if !spec.AvailableAt.IsZero() {
		availableAt = timeToPG(spec.AvailableAt)
	}
	id, err := queries.EnqueueJob(ctx, dbgen.EnqueueJobParams{
		ID: uuidToPG(uuid.New()), Kind: spec.Kind, IdempotencyKey: spec.IdempotencyKey,
		Payload: spec.Payload, MaxAttempts: int32(spec.MaxAttempts), AvailableAt: availableAt,
		ParentJobID: nullableUUID(spec.ParentJobID), Required: spec.Required, Stage: spec.Stage,
		UserID: nullableUUID(spec.UserID), Priority: int16(spec.Priority),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("enqueue job: %w", err)
	}
	return uuidFromPG(id)
}

// AwaitChildren atomically enqueues idempotent child stages and releases the
// parent lease. The parent is re-queued once every child is terminal and no
// required child failed. The caller must return ErrDeferred when ready is false.
func (q *Queue) AwaitChildren(ctx context.Context, lease Lease, specs []Spec) (ready bool, err error) {
	if q == nil || q.store == nil {
		return false, errors.New("queue store is required")
	}
	tx, err := q.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin child scheduling: %w", err)
	}
	defer tx.Rollback(ctx)
	var owned uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3 AND lease_until>now() FOR UPDATE`, lease.ID, lease.WorkerID, lease.FencingToken).Scan(&owned); errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLeaseLost
	} else if err != nil {
		return false, fmt.Errorf("verify parent lease: %w", err)
	}
	queries := q.store.Queries.WithTx(tx)
	for _, spec := range specs {
		spec.ParentJobID = lease.ID
		if !spec.Required && !spec.Optional {
			spec.Required = true
		}
		if _, err := EnqueueInTx(ctx, queries, spec); err != nil {
			return false, fmt.Errorf("enqueue child stage: %w", err)
		}
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE parent_job_id=$1 AND status NOT IN ('succeeded','dead')`, lease.ID).Scan(&pending); err != nil {
		return false, fmt.Errorf("inspect child stages: %w", err)
	}
	if pending == 0 {
		return true, tx.Commit(ctx)
	}
	command, err := tx.Exec(ctx, `UPDATE sync_jobs SET status='waiting', attempts=GREATEST(attempts-1,0), worker_id=NULL, fencing_token=NULL, lease_until=NULL, updated_at=now() WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3`, lease.ID, lease.WorkerID, lease.FencingToken)
	if err != nil {
		return false, fmt.Errorf("defer parent: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE job_attempts SET status='waiting', finished_at=now() WHERE job_id=$1 AND worker_id=$2 AND fencing_token=$3 AND status='running'`, lease.ID, lease.WorkerID, lease.FencingToken); err != nil {
		return false, fmt.Errorf("finish deferred attempt: %w", err)
	}
	return false, tx.Commit(ctx)
}

// DeferUntil releases an owned lease for a future poll without spending a
// failure attempt. Handlers must return ErrDeferred after this succeeds.
func (q *Queue) DeferUntil(ctx context.Context, lease Lease, availableAt time.Time) error {
	if q == nil || q.store == nil {
		return errors.New("queue store is required")
	}
	if !availableAt.After(time.Now()) {
		return errors.New("deferred availability must be in the future")
	}
	tx, err := q.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin deferred polling: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := q.store.Queries.WithTx(tx)
	rows, err := queries.DeferJobUntil(ctx, dbgen.DeferJobUntilParams{
		AvailableAt: timeToPG(availableAt), ID: uuidToPG(lease.ID),
		WorkerID: textToPG(lease.WorkerID), FencingToken: uuidToPG(lease.FencingToken),
	})
	if err != nil {
		return fmt.Errorf("defer polling job: %w", err)
	}
	if err := requireOwnership(rows); err != nil {
		return err
	}
	if err := queries.FinishDeferredAttempt(ctx, dbgen.FinishDeferredAttemptParams{
		JobID: uuidToPG(lease.ID), WorkerID: lease.WorkerID, FencingToken: uuidToPG(lease.FencingToken),
	}); err != nil {
		return fmt.Errorf("finish deferred attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit deferred polling: %w", err)
	}
	return nil
}

func (q *Queue) Claim(ctx context.Context, workerID string, lease time.Duration) (*Lease, error) {
	if q == nil || q.store == nil {
		return nil, errors.New("queue store is required")
	}
	if strings.TrimSpace(workerID) == "" {
		return nil, errors.New("worker ID is required")
	}
	if lease <= 0 {
		return nil, errors.New("lease duration must be positive")
	}
	tx, err := q.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := q.store.Queries.WithTx(tx)
	if err := queries.FinishExpiredAttempts(ctx); err != nil {
		return nil, fmt.Errorf("finish expired attempts: %w", err)
	}
	if err := queries.DeadLetterExhaustedLeases(ctx); err != nil {
		return nil, fmt.Errorf("dead-letter exhausted leases: %w", err)
	}
	if err := queries.FailParentsWithDeadRequiredChildren(ctx); err != nil {
		return nil, fmt.Errorf("fail parent with dead required child: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_jobs parent SET status='queued',available_at=now(),updated_at=now()
		WHERE parent.status='waiting'
		AND NOT EXISTS(SELECT 1 FROM sync_jobs child WHERE child.parent_job_id=parent.id AND child.status NOT IN ('succeeded','dead'))
		AND NOT EXISTS(SELECT 1 FROM sync_jobs child WHERE child.parent_job_id=parent.id AND child.required AND child.status='dead')`); err != nil {
		return nil, fmt.Errorf("promote workflows after lease cleanup: %w", err)
	}
	token := uuid.New()
	row, err := queries.ClaimJob(ctx, dbgen.ClaimJobParams{
		WorkerID: textToPG(workerID), FencingToken: uuidToPG(token), LeaseDuration: durationToPG(lease),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit lease cleanup: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	jobID, err := uuidFromPG(row.ID)
	if err != nil {
		return nil, err
	}
	workflowIDValue, err := queries.GetJobWorkflowID(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve job workflow: %w", err)
	}
	workflowID, err := uuidFromPG(workflowIDValue)
	if err != nil {
		return nil, fmt.Errorf("resolve job workflow: %w", err)
	}
	parentJobID := uuid.Nil
	if row.ParentJobID.Valid {
		parentJobID = uuid.UUID(row.ParentJobID.Bytes)
	}
	userID := uuid.Nil
	if row.UserID.Valid {
		userID = uuid.UUID(row.UserID.Bytes)
	}
	if err := queries.InsertJobAttempt(ctx, dbgen.InsertJobAttemptParams{
		ID: uuidToPG(uuid.New()), ID_2: row.ID, WorkerID: textToPG(workerID), FencingToken: uuidToPG(token),
	}); err != nil {
		return nil, fmt.Errorf("record job attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &Lease{
		ID: jobID, ParentJobID: parentJobID, WorkflowID: workflowID,
		Kind: row.Kind, Required: row.Required, Stage: row.Stage, UserID: userID, IdempotencyKey: row.IdempotencyKey,
		Payload: row.Payload, Result: row.Result, Attempts: int(row.Attempts), MaxAttempts: int(row.MaxAttempts),
		Priority: int(row.Priority),
		WorkerID: workerID, FencingToken: token, LeaseUntil: row.LeaseUntil.Time,
	}, nil
}

func (q *Queue) Renew(ctx context.Context, lease Lease, duration time.Duration) error {
	if duration <= 0 {
		return errors.New("lease duration must be positive")
	}
	rows, err := q.store.Queries.RenewJob(ctx, dbgen.RenewJobParams{
		LeaseDuration: durationToPG(duration), ID: uuidToPG(lease.ID),
		WorkerID: textToPG(lease.WorkerID), FencingToken: uuidToPG(lease.FencingToken),
	})
	if err != nil {
		return fmt.Errorf("renew job lease: %w", err)
	}
	return requireOwnership(rows)
}

func (q *Queue) Complete(ctx context.Context, lease Lease) error {
	tx, err := q.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin completion: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := q.store.Queries.WithTx(tx)
	rows, err := queries.CompleteJob(ctx, dbgen.CompleteJobParams{
		ID: uuidToPG(lease.ID), WorkerID: textToPG(lease.WorkerID), FencingToken: uuidToPG(lease.FencingToken),
	})
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if err := requireOwnership(rows); err != nil {
		return err
	}
	if err := queries.FinishSuccessfulAttempt(ctx, dbgen.FinishSuccessfulAttemptParams{
		JobID: uuidToPG(lease.ID), WorkerID: lease.WorkerID, FencingToken: uuidToPG(lease.FencingToken),
	}); err != nil {
		return fmt.Errorf("finish successful attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_jobs parent SET status='queued', available_at=now(), updated_at=now()
		WHERE parent.id=(SELECT parent_job_id FROM sync_jobs WHERE id=$1) AND parent.status='waiting'
		AND NOT EXISTS (SELECT 1 FROM sync_jobs child WHERE child.parent_job_id=parent.id AND child.status NOT IN ('succeeded','dead'))
		AND NOT EXISTS (SELECT 1 FROM sync_jobs child WHERE child.parent_job_id=parent.id AND child.required AND child.status='dead')`, lease.ID); err != nil {
		return fmt.Errorf("promote completed parent: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}
	return nil
}

func (q *Queue) Fail(ctx context.Context, lease Lease, cause error, retryAt time.Time) (FailureOutcome, error) {
	if cause == nil {
		return "", errors.New("failure cause is required")
	}
	if retryAt.IsZero() {
		retryAt = time.Now()
	}
	tx, err := q.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin failure: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := q.store.Queries.WithTx(tx)
	retryable := true
	var disposition retryDisposition
	if errors.As(cause, &disposition) {
		retryable = disposition.Retryable()
	}
	rows, err := queries.FailJob(ctx, dbgen.FailJobParams{
		RetryAt: timeToPG(retryAt), Cause: textToPG(cause.Error()), ID: uuidToPG(lease.ID),
		WorkerID: textToPG(lease.WorkerID), FencingToken: uuidToPG(lease.FencingToken), Retryable: retryable,
	})
	if err != nil {
		return "", fmt.Errorf("fail job: %w", err)
	}
	if err := requireOwnership(rows); err != nil {
		return "", err
	}
	status := "retryable"
	if !retryable || lease.Attempts >= lease.MaxAttempts {
		status = "dead"
	}
	if err := queries.FinishFailedAttempt(ctx, dbgen.FinishFailedAttemptParams{
		JobID: uuidToPG(lease.ID), WorkerID: lease.WorkerID, FencingToken: uuidToPG(lease.FencingToken),
		Status: status, Error: textToPG(cause.Error()),
	}); err != nil {
		return "", fmt.Errorf("finish failed attempt: %w", err)
	}
	if status == "dead" {
		if _, err := tx.Exec(ctx, `UPDATE sync_jobs parent SET status='dead', last_error='required child stage failed', updated_at=now()
			WHERE parent.id=(SELECT parent_job_id FROM sync_jobs WHERE id=$1) AND parent.status='waiting' AND (SELECT required FROM sync_jobs WHERE id=$1)`, lease.ID); err != nil {
			return "", fmt.Errorf("fail parent workflow: %w", err)
		}
		// A required child may kill an optional parent (for example an embedding
		// child of an optional AI feature stage). Wake that parent's waiting
		// parent once every sibling is terminal; the optional failure must not
		// strand the outer workflow.
		if _, err := tx.Exec(ctx, `UPDATE sync_jobs grandparent SET status='queued',available_at=now(),updated_at=now()
			WHERE grandparent.id=(SELECT parent.parent_job_id FROM sync_jobs child JOIN sync_jobs parent ON parent.id=child.parent_job_id WHERE child.id=$1 AND NOT parent.required)
			AND grandparent.status='waiting'
			AND NOT EXISTS(SELECT 1 FROM sync_jobs sibling WHERE sibling.parent_job_id=grandparent.id AND sibling.status NOT IN ('succeeded','dead'))
			AND NOT EXISTS(SELECT 1 FROM sync_jobs sibling WHERE sibling.parent_job_id=grandparent.id AND sibling.required AND sibling.status='dead')`, lease.ID); err != nil {
			return "", fmt.Errorf("promote grandparent after nested optional failure: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE sync_jobs parent SET status='queued',available_at=now(),updated_at=now()
			WHERE parent.id=(SELECT parent_job_id FROM sync_jobs WHERE id=$1) AND parent.status='waiting'
			AND NOT EXISTS(SELECT 1 FROM sync_jobs child WHERE child.parent_job_id=parent.id AND child.status NOT IN ('succeeded','dead'))
			AND NOT EXISTS(SELECT 1 FROM sync_jobs child WHERE child.parent_job_id=parent.id AND child.required AND child.status='dead')`, lease.ID); err != nil {
			return "", fmt.Errorf("promote parent after optional failure: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit failure: %w", err)
	}
	if status == "dead" {
		return FailureDead, nil
	}
	return FailureRetryScheduled, nil
}

func requireOwnership(rows int64) error {
	if rows != 1 {
		return ErrLeaseLost
	}
	return nil
}

func uuidToPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }
func nullableUUID(value uuid.UUID) pgtype.UUID {
	if value == uuid.Nil {
		return pgtype.UUID{}
	}
	return uuidToPG(value)
}

func uuidFromPG(value pgtype.UUID) (uuid.UUID, error) {
	if !value.Valid {
		return uuid.Nil, errors.New("unexpected null UUID")
	}
	return uuid.UUID(value.Bytes), nil
}

func textToPG(value string) pgtype.Text { return pgtype.Text{String: value, Valid: true} }
func timeToPG(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
func durationToPG(value time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: value.Microseconds(), Valid: true}
}
