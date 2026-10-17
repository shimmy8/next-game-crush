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

type Spec struct {
	Kind           string
	IdempotencyKey string
	Payload        json.RawMessage
	MaxAttempts    int
	AvailableAt    time.Time
}

type Lease struct {
	ID             uuid.UUID
	Kind           string
	IdempotencyKey string
	Payload        json.RawMessage
	Attempts       int
	MaxAttempts    int
	WorkerID       string
	FencingToken   uuid.UUID
	LeaseUntil     time.Time
}

type Queue struct {
	store *store.Store
}

func NewQueue(db *store.Store) *Queue {
	return &Queue{store: db}
}

func (q *Queue) Enqueue(ctx context.Context, spec Spec) (uuid.UUID, error) {
	if q == nil || q.store == nil {
		return uuid.Nil, errors.New("queue store is required")
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
	availableAt := pgtype.Timestamptz{}
	if !spec.AvailableAt.IsZero() {
		availableAt = timeToPG(spec.AvailableAt)
	}
	id, err := q.store.Queries.EnqueueJob(ctx, dbgen.EnqueueJobParams{
		ID: uuidToPG(uuid.New()), Kind: spec.Kind, IdempotencyKey: spec.IdempotencyKey,
		Payload: spec.Payload, MaxAttempts: int32(spec.MaxAttempts), AvailableAt: availableAt,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("enqueue job: %w", err)
	}
	return uuidFromPG(id)
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
	if err := queries.InsertJobAttempt(ctx, dbgen.InsertJobAttemptParams{
		ID: uuidToPG(uuid.New()), ID_2: row.ID, WorkerID: textToPG(workerID), FencingToken: uuidToPG(token),
	}); err != nil {
		return nil, fmt.Errorf("record job attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &Lease{
		ID: jobID, Kind: row.Kind, IdempotencyKey: row.IdempotencyKey,
		Payload: row.Payload, Attempts: int(row.Attempts), MaxAttempts: int(row.MaxAttempts),
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
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}
	return nil
}

func (q *Queue) Fail(ctx context.Context, lease Lease, cause error, retryAt time.Time) error {
	if cause == nil {
		return errors.New("failure cause is required")
	}
	if retryAt.IsZero() {
		retryAt = time.Now()
	}
	tx, err := q.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin failure: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := q.store.Queries.WithTx(tx)
	rows, err := queries.FailJob(ctx, dbgen.FailJobParams{
		RetryAt: timeToPG(retryAt), Cause: textToPG(cause.Error()), ID: uuidToPG(lease.ID),
		WorkerID: textToPG(lease.WorkerID), FencingToken: uuidToPG(lease.FencingToken),
	})
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	if err := requireOwnership(rows); err != nil {
		return err
	}
	status := "retryable"
	if lease.Attempts >= lease.MaxAttempts {
		status = "dead"
	}
	if err := queries.FinishFailedAttempt(ctx, dbgen.FinishFailedAttemptParams{
		JobID: uuidToPG(lease.ID), WorkerID: lease.WorkerID, FencingToken: uuidToPG(lease.FencingToken),
		Status: status, Error: textToPG(cause.Error()),
	}); err != nil {
		return fmt.Errorf("finish failed attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit failure: %w", err)
	}
	return nil
}

func requireOwnership(rows int64) error {
	if rows != 1 {
		return ErrLeaseLost
	}
	return nil
}

func uuidToPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }

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
