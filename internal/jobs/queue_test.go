package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type permanentTestError struct{}

func (permanentTestError) Error() string   { return "permanent" }
func (permanentTestError) Retryable() bool { return false }

func TestEnqueueIsIdempotent(t *testing.T) {
	queue, _ := openTestQueue(t)
	spec := Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "sync:user:1", Payload: []byte(`{"user":"1"}`), MaxAttempts: 3}
	first, err := queue.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := queue.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("IDs differ: %s != %s", first, second)
	}
}

func TestClaimOrdersPriorityThenStableTime(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var ids []uuid.UUID
	for i, priority := range []int{10, 100, 50, 50, 50, 50} {
		id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "priority", IdempotencyKey: fmt.Sprintf("order:%d", i), Priority: priority, AvailableAt: base})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET created_at=$2 WHERE id=$1`, id, base); err != nil {
			t.Fatal(err)
		}
	}
	// Tie breakers are available_at, created_at, then UUID, regardless of insertion order.
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET available_at=$2 WHERE id=$1`, ids[2], base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET created_at=$2 WHERE id=$1`, ids[3], base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	lastA, lastB := ids[4], ids[5]
	if strings.Compare(lastA.String(), lastB.String()) > 0 {
		lastA, lastB = lastB, lastA
	}
	for i, want := range []uuid.UUID{ids[1], ids[2], ids[3], lastA, lastB, ids[0]} {
		lease, err := queue.Claim(ctx, LaneInteractive, "priority-test", time.Minute)
		if err != nil || lease == nil || lease.ID != want {
			t.Fatalf("claim %d=%#v err=%v, want %s", i, lease, err, want)
		}
		if err := queue.Complete(ctx, *lease); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReenqueueRaisesButNeverLowersPriority(t *testing.T) {
	for _, terminal := range []string{"running", "succeeded", "dead"} {
		t.Run(terminal, func(t *testing.T) {
			queue, db := openTestQueue(t)
			ctx := context.Background()
			id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "original", IdempotencyKey: "duplicate", Payload: []byte(`{"original":true}`), Priority: 10, MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := queue.Claim(ctx, LaneInteractive, "owner", time.Minute)
			if err != nil || lease == nil {
				t.Fatalf("claim=%#v err=%v", lease, err)
			}
			if terminal == "succeeded" {
				if err := queue.Complete(ctx, *lease); err != nil {
					t.Fatal(err)
				}
			}
			if terminal == "dead" {
				if _, err := queue.Fail(ctx, *lease, permanentTestError{}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			var before []byte
			if err := db.Pool.QueryRow(ctx, `SELECT to_jsonb(j)-'priority' FROM sync_jobs j WHERE id=$1`, id).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for _, priority := range []int{80, 50} {
				duplicate, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "replacement", IdempotencyKey: "duplicate", Payload: []byte(`{"replacement":true}`), MaxAttempts: 9, Priority: priority, AvailableAt: time.Now().Add(time.Hour)})
				if err != nil || duplicate != id {
					t.Fatalf("duplicate=%s err=%v", duplicate, err)
				}
				var after []byte
				var stored int
				if err := db.Pool.QueryRow(ctx, `SELECT to_jsonb(j)-'priority',priority FROM sync_jobs j WHERE id=$1`, id).Scan(&after, &stored); err != nil {
					t.Fatal(err)
				}
				if stored != 80 || string(before) != string(after) {
					t.Fatalf("duplicate changed job history or priority: stored=%d", stored)
				}
			}
		})
	}
}

func TestEnqueueInTxRollsBackAndSharesValidation(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	queries := db.Queries.WithTx(tx)
	for _, spec := range []Spec{
		{Lane: LaneInteractive, IdempotencyKey: "invalid"}, {Lane: LaneInteractive, Kind: "test"},
		{Lane: LaneInteractive, Kind: "test", IdempotencyKey: "invalid", Payload: []byte(`{`)},
		{Lane: LaneInteractive, Kind: "test", IdempotencyKey: "invalid", MaxAttempts: -1},
		{Lane: LaneInteractive, Kind: "test", IdempotencyKey: "invalid", Priority: -1},
		{Lane: LaneInteractive, Kind: "test", IdempotencyKey: "invalid", Priority: 101},
	} {
		if _, err := queue.Enqueue(ctx, spec); err == nil {
			t.Fatalf("Enqueue accepted invalid spec: %#v", spec)
		}
		if _, err := EnqueueInTx(ctx, queries, spec); err == nil {
			t.Fatalf("EnqueueInTx accepted invalid spec: %#v", spec)
		}
	}
	if _, err := EnqueueInTx(ctx, nil, Spec{Lane: LaneInteractive, Kind: "test", IdempotencyKey: "nil"}); err == nil {
		t.Fatal("nil queries accepted")
	}
	id, err := EnqueueInTx(ctx, queries, Spec{Lane: LaneInteractive, Kind: "test", IdempotencyKey: "rollback"})
	if err != nil {
		t.Fatal(err)
	}
	var priority, attempts int
	var required bool
	if err := tx.QueryRow(ctx, `SELECT priority,max_attempts,required FROM sync_jobs WHERE id=$1`, id).Scan(&priority, &attempts, &required); err != nil {
		t.Fatal(err)
	}
	if priority != 50 || attempts != 3 || !required {
		t.Fatalf("defaults=%d/%d/%v", priority, attempts, required)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE id=$1`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("rolled-back enqueue remained visible")
	}
	// The transaction path must keep the database's deletion fencing trigger.
	deleted := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO deleted_user_tombstones(user_id) VALUES($1)`, deleted); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		spec Spec
	}{
		{name: "direct user", spec: Spec{Lane: LaneInteractive, Kind: "test", IdempotencyKey: "deleted", UserID: deleted}},
		{name: "completion projection", spec: Spec{Lane: LaneInteractive, Kind: "completion.project", IdempotencyKey: "deleted-completion", UserID: deleted, Payload: []byte(`{"version":1}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := db.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := EnqueueInTx(ctx, dbgen.New(tx), test.spec); err == nil {
				t.Fatal("transaction enqueue bypassed deleted-user fencing")
			}
		})
	}
	tx, err = db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := EnqueueInTx(ctx, dbgen.New(tx), Spec{Lane: LaneInteractive, Kind: "achievement.classify", IdempotencyKey: "shared-classification", Payload: []byte(`{"version":1}`)}); err != nil {
		t.Fatalf("shared achievement classification was incorrectly fenced: %v", err)
	}
}

func TestAwaitChildrenPreservesPriorityAndAvailableAt(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	if _, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "parent", IdempotencyKey: "scheduled-parent"}); err != nil {
		t.Fatal(err)
	}
	parent, err := queue.Claim(ctx, LaneInteractive, "parent-worker", time.Minute)
	if err != nil || parent == nil {
		t.Fatalf("parent=%#v err=%v", parent, err)
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	ready, err := queue.AwaitChildren(ctx, *parent, []Spec{{Lane: LaneInteractive, Kind: "child", IdempotencyKey: "scheduled-child", Priority: 80, AvailableAt: future, Optional: true}})
	if err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	var priority int
	var availableAt time.Time
	var required bool
	if err := db.Pool.QueryRow(ctx, `SELECT priority,available_at,required FROM sync_jobs WHERE idempotency_key='scheduled-child'`).Scan(&priority, &availableAt, &required); err != nil {
		t.Fatal(err)
	}
	if priority != 80 || !availableAt.Equal(future) || required {
		t.Fatalf("child priority=%d available=%v required=%v", priority, availableAt, required)
	}
	if lease, err := queue.Claim(ctx, LaneInteractive, "child-worker", time.Minute); err != nil || lease != nil {
		t.Fatalf("claimed scheduled child: %#v err=%v", lease, err)
	}
}

func TestDeferUntilIsFenced(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "poll", IdempotencyKey: "poll", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(ctx, LaneInteractive, "poller", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim=%#v err=%v", first, err)
	}
	if _, err := queue.Fail(ctx, *first, errors.New("temporary"), time.Now()); err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(ctx, LaneInteractive, "poller", time.Minute)
	if err != nil || lease == nil || lease.Attempts != 2 {
		t.Fatalf("claim=%#v err=%v", lease, err)
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	stale := *lease
	stale.FencingToken = uuid.New()
	if err := queue.DeferUntil(ctx, stale, future); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale defer=%v", err)
	}
	for _, invalid := range []time.Time{{}, time.Now().Add(-time.Second)} {
		if err := queue.DeferUntil(ctx, *lease, invalid); err == nil {
			t.Fatal("accepted nonfuture defer")
		}
	}
	var attempts int
	var status string
	if err := db.Pool.QueryRow(ctx, `SELECT status,attempts FROM sync_jobs WHERE id=$1`, id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "running" || attempts != 2 {
		t.Fatalf("stale defer changed %s/%d", status, attempts)
	}
	if err := queue.DeferUntil(ctx, *lease, future); err != nil {
		t.Fatal(err)
	}
	var availableAt time.Time
	var leaseCleared bool
	if err := db.Pool.QueryRow(ctx, `SELECT status,attempts,available_at,worker_id IS NULL AND fencing_token IS NULL AND lease_until IS NULL FROM sync_jobs WHERE id=$1`, id).Scan(&status, &attempts, &availableAt, &leaseCleared); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempts != 1 || !availableAt.Equal(future) || !leaseCleared {
		t.Fatalf("deferred=%s/%d/%v/cleared=%v", status, attempts, availableAt, leaseCleared)
	}
	var history, waiting, failed int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='waiting' AND finished_at IS NOT NULL),count(*) FILTER(WHERE status='retryable') FROM job_attempts WHERE job_id=$1`, id).Scan(&history, &waiting, &failed); err != nil {
		t.Fatal(err)
	}
	if history != 2 || waiting != 1 || failed != 1 {
		t.Fatalf("history=%d waiting=%d failed=%d", history, waiting, failed)
	}
	if err := queue.DeferUntil(ctx, *lease, future); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("duplicate defer=%v", err)
	}
	if err := queue.Complete(ctx, *lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("deferred completion=%v", err)
	}
	if got, err := queue.Claim(ctx, LaneInteractive, "other", time.Minute); err != nil || got != nil {
		t.Fatalf("early claim=%#v err=%v", got, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET available_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	resumed, err := queue.Claim(ctx, LaneInteractive, "resumed", time.Minute)
	if err != nil || resumed == nil || resumed.Attempts != 2 || resumed.FencingToken == lease.FencingToken {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := queue.DeferUntil(ctx, *resumed, future); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired defer=%v", err)
	}
}

func TestClaimSkipsLeasedJob(t *testing.T) {
	queue, _ := openTestQueue(t)
	_, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), LaneInteractive, "worker-a", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	second, err := queue.Claim(context.Background(), LaneInteractive, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatalf("second worker claimed leased job: %#v", second)
	}
}

func TestFailureOutcomeDistinguishesRetryDeadAndLeaseLoss(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "steam.sync", IdempotencyKey: "failure-outcome", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim=%#v err=%v", first, err)
	}
	outcome, err := queue.Fail(ctx, *first, errors.New("retry canary"), time.Now())
	if err != nil || outcome != FailureRetryScheduled {
		t.Fatalf("first failure outcome=%q err=%v", outcome, err)
	}
	if _, err := db.Pool.Exec(ctx, "UPDATE sync_jobs SET available_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	second, err := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim=%#v err=%v", second, err)
	}
	stale := *second
	stale.FencingToken = uuid.New()
	outcome, err = queue.Fail(ctx, stale, errors.New("stale canary"), time.Now())
	if !errors.Is(err, ErrLeaseLost) || outcome != "" {
		t.Fatalf("stale failure outcome=%q err=%v", outcome, err)
	}
	outcome, err = queue.Fail(ctx, *second, errors.New("dead canary"), time.Now())
	if err != nil || outcome != FailureDead {
		t.Fatalf("final failure outcome=%q err=%v", outcome, err)
	}
}

func TestPermanentFailureDeadLettersOnFirstAttempt(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "ai.features", IdempotencyKey: "permanent-first-attempt", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim=%#v err=%v", lease, err)
	}
	outcome, err := queue.Fail(ctx, *lease, permanentTestError{}, time.Now().Add(time.Hour))
	if err != nil || outcome != FailureDead {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	var status string
	var attempts int
	if err := db.Pool.QueryRow(ctx, "SELECT status,attempts FROM sync_jobs WHERE id=$1", id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "dead" || attempts != 1 {
		t.Fatalf("status=%q attempts=%d, want dead/1", status, attempts)
	}
}

func TestDeadLetterSuccessorEnqueuesOnlyOnTerminalTransition(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	root, err := queue.Enqueue(ctx, Spec{Lane: LaneCatalog, Kind: "root", IdempotencyKey: "dead-successor:root", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	successor := Spec{Lane: LaneCatalog, Kind: "root", IdempotencyKey: "dead-successor:next", MaxAttempts: 2, AvailableAt: time.Now().Add(time.Hour)}
	first, err := queue.Claim(ctx, LaneCatalog, "dead-successor", time.Minute)
	if err != nil || first == nil || first.ID != root {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if outcome, err := queue.Fail(ctx, *first, WithDeadLetterSuccessor(errors.New("temporary"), successor), time.Now().Add(-time.Second)); err != nil || outcome != FailureRetryScheduled {
		t.Fatalf("first outcome=%q err=%v", outcome, err)
	}
	var successors int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE idempotency_key='dead-successor:next'`).Scan(&successors); err != nil || successors != 0 {
		t.Fatalf("successors before dead=%d err=%v", successors, err)
	}
	second, err := queue.Claim(ctx, LaneCatalog, "dead-successor", time.Minute)
	if err != nil || second == nil || second.ID != root {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if outcome, err := queue.Fail(ctx, *second, WithDeadLetterSuccessor(errors.New("terminal"), successor), time.Now()); err != nil || outcome != FailureDead {
		t.Fatalf("second outcome=%q err=%v", outcome, err)
	}
	var rootStatus, successorStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT
		(SELECT status FROM sync_jobs WHERE id=$1),(SELECT status FROM sync_jobs WHERE idempotency_key='dead-successor:next')`, root).
		Scan(&rootStatus, &successorStatus); err != nil {
		t.Fatal(err)
	}
	if rootStatus != "dead" || successorStatus != "queued" {
		t.Fatalf("root=%q successor=%q", rootStatus, successorStatus)
	}
}

func TestClaimClearsCurrentErrorAndPreservesAttemptHistory(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "steam.sync", IdempotencyKey: "claim-clears-error", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(ctx, LaneInteractive, "worker-a", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim=%#v err=%v", first, err)
	}
	if _, err := queue.Fail(ctx, *first, errors.New("first failure"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "UPDATE sync_jobs SET available_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	second, err := queue.Claim(ctx, LaneInteractive, "worker-b", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim=%#v err=%v", second, err)
	}
	var currentErrorCleared bool
	if err := db.Pool.QueryRow(ctx, "SELECT last_error IS NULL FROM sync_jobs WHERE id=$1", id).Scan(&currentErrorCleared); err != nil {
		t.Fatal(err)
	}
	if !currentErrorCleared {
		t.Fatal("claimed job retained the previous attempt error")
	}
	if _, err := queue.Fail(ctx, *second, errors.New("second failure"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var currentError string
	if err := db.Pool.QueryRow(ctx, "SELECT last_error FROM sync_jobs WHERE id=$1", id).Scan(&currentError); err != nil {
		t.Fatal(err)
	}
	if currentError != "second failure" {
		t.Fatalf("current error=%q, want second failure", currentError)
	}

	rows, err := db.Pool.Query(ctx, "SELECT attempt, error FROM job_attempts WHERE job_id=$1 ORDER BY attempt", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var attemptErrors []string
	for rows.Next() {
		var attempt int
		var attemptError string
		if err := rows.Scan(&attempt, &attemptError); err != nil {
			t.Fatal(err)
		}
		if attempt != len(attemptErrors)+1 {
			t.Fatalf("attempt number=%d, want %d", attempt, len(attemptErrors)+1)
		}
		attemptErrors = append(attemptErrors, attemptError)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(attemptErrors) != 2 || attemptErrors[0] != "first failure" || attemptErrors[1] != "second failure" {
		t.Fatalf("attempt errors=%q, want [first failure second failure]", attemptErrors)
	}
}

func TestLeaseCarriesRootWorkflowAndStoredMetadata(t *testing.T) {
	queue, _ := openTestQueue(t)
	ctx := context.Background()
	_, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "steam.sync", IdempotencyKey: "workflow-root", Stage: "steam"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if err != nil || root == nil {
		t.Fatalf("root=%#v err=%v", root, err)
	}
	if root.WorkflowID != root.ID || root.ParentJobID != uuid.Nil || !root.Required || root.Stage != "steam" {
		t.Fatalf("root metadata=%#v", root)
	}
	ready, err := queue.AwaitChildren(ctx, *root, []Spec{{Lane: LaneInteractive, Kind: "ai.features", IdempotencyKey: "workflow-child", Optional: true, Stage: "ai_features"}})
	if err != nil || ready {
		t.Fatalf("child scheduling ready=%v err=%v", ready, err)
	}
	child, err := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if err != nil || child == nil {
		t.Fatalf("child=%#v err=%v", child, err)
	}
	if child.WorkflowID != root.ID || child.ParentJobID != root.ID || child.Required || child.Stage != "ai_features" {
		t.Fatalf("child metadata=%#v", child)
	}
	ready, err = queue.AwaitChildren(ctx, *child, []Spec{{Lane: LaneInteractive, Kind: "ai.embedding", IdempotencyKey: "workflow-grandchild", Required: true, Stage: "ai_embedding"}})
	if err != nil || ready {
		t.Fatalf("grandchild scheduling ready=%v err=%v", ready, err)
	}
	grandchild, err := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if err != nil || grandchild == nil {
		t.Fatalf("grandchild=%#v err=%v", grandchild, err)
	}
	if grandchild.WorkflowID != root.ID || grandchild.ParentJobID != child.ID || !grandchild.Required || grandchild.Stage != "ai_embedding" {
		t.Fatalf("grandchild metadata=%#v", grandchild)
	}
}

func TestAwaitChildrenPublishesCompleteBatchAtomically(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	if _, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "steam.sync", IdempotencyKey: "atomic-batch-root", Stage: "steam"}); err != nil {
		t.Fatal(err)
	}
	root, err := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if err != nil || root == nil {
		t.Fatalf("root=%#v err=%v", root, err)
	}

	_, err = queue.AwaitChildren(ctx, *root, []Spec{
		{Lane: LaneInteractive, Kind: "ai.features", IdempotencyKey: "atomic-batch-valid", Stage: "ai_features"},
		{Lane: LaneInteractive, Kind: "ai.features", IdempotencyKey: "atomic-batch-invalid", Stage: "ai_features", MaxAttempts: -1},
	})
	if err == nil {
		t.Fatal("AwaitChildren accepted an invalid partial batch")
	}
	var childCount int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE parent_job_id=$1`, root.ID).Scan(&childCount); err != nil {
		t.Fatal(err)
	}
	if childCount != 0 {
		t.Fatalf("failed batch published %d children, want 0", childCount)
	}
	var rootStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, root.ID).Scan(&rootStatus); err != nil {
		t.Fatal(err)
	}
	if rootStatus != "running" {
		t.Fatalf("root status after rolled-back batch = %q, want running", rootStatus)
	}

	ready, err := queue.AwaitChildren(ctx, *root, []Spec{
		{Lane: LaneInteractive, Kind: "ai.features", IdempotencyKey: "atomic-batch-one", Stage: "ai_features"},
		{Lane: LaneInteractive, Kind: "ai.features", IdempotencyKey: "atomic-batch-two", Stage: "ai_features"},
	})
	if err != nil || ready {
		t.Fatalf("AwaitChildren ready=%v err=%v", ready, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE parent_job_id=$1 AND stage='ai_features'`, root.ID).Scan(&childCount); err != nil {
		t.Fatal(err)
	}
	if childCount != 2 {
		t.Fatalf("published AI batch has %d children, want 2", childCount)
	}
	first, err := queue.Claim(ctx, LaneInteractive, "ai-worker", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first AI claim=%#v err=%v", first, err)
	}
	if first.ParentJobID != root.ID || first.Stage != "ai_features" {
		t.Fatalf("first AI claim=%#v", first)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE parent_job_id=$1 AND stage='ai_features'`, root.ID).Scan(&childCount); err != nil {
		t.Fatal(err)
	}
	if childCount != 2 {
		t.Fatalf("first AI child ran before complete batch was visible: count=%d", childCount)
	}
}

func TestRecordResultRequiresCurrentFencingToken(t *testing.T) {
	queue, db := openTestQueue(t)
	id, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "profile.rebuild", IdempotencyKey: "profile:user:1"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(context.Background(), LaneInteractive, "worker-a", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim=%#v err=%v", lease, err)
	}
	stale := *lease
	stale.FencingToken = uuid.New()
	if err := queue.RecordResult(context.Background(), stale, map[string]any{"profileVersion": 1}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale result error=%v", err)
	}
	if err := queue.RecordResult(context.Background(), *lease, map[string]any{"profileVersion": 2}); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.Pool.QueryRow(context.Background(), `SELECT (result->>'profileVersion')::int FROM sync_jobs WHERE id=$1`, id).Scan(&version); err != nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestNestedRequiredFailurePromotesGrandparentWhenParentIsOptional(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	_, _ = queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "nested-root"})
	root, _ := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	ready, err := queue.AwaitChildren(ctx, *root, []Spec{{Lane: LaneInteractive, Kind: "feature", IdempotencyKey: "nested-feature", Optional: true, MaxAttempts: 1}})
	if err != nil || ready {
		t.Fatalf("feature wait ready=%v err=%v", ready, err)
	}
	feature, _ := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	ready, err = queue.AwaitChildren(ctx, *feature, []Spec{{Lane: LaneInteractive, Kind: "embedding", IdempotencyKey: "nested-embedding", Required: true, MaxAttempts: 1}})
	if err != nil || ready {
		t.Fatalf("embedding wait ready=%v err=%v", ready, err)
	}
	embedding, _ := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	if _, err := queue.Fail(ctx, *embedding, errors.New("embedding failed"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var rootStatus, featureStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT r.status,f.status FROM sync_jobs r JOIN sync_jobs f ON f.parent_job_id=r.id WHERE r.id=$1`, root.ID).Scan(&rootStatus, &featureStatus); err != nil {
		t.Fatal(err)
	}
	if rootStatus != "queued" || featureStatus != "dead" {
		t.Fatalf("root/feature=%s/%s, want queued/dead", rootStatus, featureStatus)
	}
}

func TestNestedExpiredRequiredLeasePromotesGrandparentWhenParentIsOptional(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	_, _ = queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "expired-root"})
	root, _ := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	_, _ = queue.AwaitChildren(ctx, *root, []Spec{{Lane: LaneInteractive, Kind: "feature", IdempotencyKey: "expired-feature", Optional: true, MaxAttempts: 1}})
	feature, _ := queue.Claim(ctx, LaneInteractive, "worker", time.Minute)
	_, _ = queue.AwaitChildren(ctx, *feature, []Spec{{Lane: LaneInteractive, Kind: "embedding", IdempotencyKey: "expired-embedding", Required: true, MaxAttempts: 1}})
	embedding, _ := queue.Claim(ctx, LaneInteractive, "worker", 15*time.Millisecond)
	if embedding == nil {
		t.Fatal("embedding not claimed")
	}
	time.Sleep(30 * time.Millisecond)
	reclaimed, err := queue.Claim(ctx, LaneInteractive, "next-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed == nil || reclaimed.ID != root.ID {
		t.Fatalf("reclaimed=%#v, want root %s", reclaimed, root.ID)
	}
	var featureStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, feature.ID).Scan(&featureStatus); err != nil || featureStatus != "dead" {
		t.Fatalf("feature=%s err=%v", featureStatus, err)
	}
}

func TestExpiredLeaseCanBeReclaimed(t *testing.T) {
	queue, _ := openTestQueue(t)
	id, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "reclaim"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), LaneInteractive, "worker-a", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	second, err := queue.Claim(context.Background(), LaneInteractive, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.ID != id {
		t.Fatalf("reclaim = %#v, want job %s", second, id)
	}
	if first.FencingToken == second.FencingToken {
		t.Fatal("reclaimed lease reused its fencing token")
	}
}

func TestExpiredLeasesExhaustAttempts(t *testing.T) {
	queue, db := openTestQueue(t)
	id, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "crash-loop", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), LaneInteractive, "worker-a", 15*time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	time.Sleep(30 * time.Millisecond)
	second, err := queue.Claim(context.Background(), LaneInteractive, "worker-b", 15*time.Millisecond)
	if err != nil || second == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	time.Sleep(30 * time.Millisecond)
	third, err := queue.Claim(context.Background(), LaneInteractive, "worker-c", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Fatalf("exhausted job reclaimed: %#v", third)
	}
	var status string
	var attempts int
	if err := db.Pool.QueryRow(context.Background(), "SELECT status, attempts FROM sync_jobs WHERE id=$1", id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "dead" || attempts != 2 {
		t.Fatalf("status, attempts = %q, %d; want dead, 2", status, attempts)
	}
	var unfinished int
	if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM job_attempts WHERE job_id=$1 AND finished_at IS NULL", id).Scan(&unfinished); err != nil {
		t.Fatal(err)
	}
	if unfinished != 0 {
		t.Fatalf("unfinished expired attempts = %d, want 0", unfinished)
	}
}

func TestStaleLeaseCannotCompleteOrFail(t *testing.T) {
	queue, _ := openTestQueue(t)
	_, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := queue.Claim(context.Background(), LaneInteractive, "worker-a", 15*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	current, err := queue.Claim(context.Background(), LaneInteractive, "worker-b", time.Minute)
	if err != nil || current == nil {
		t.Fatalf("reclaim = %#v, %v", current, err)
	}
	if err := queue.Complete(context.Background(), *stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Complete error = %v, want ErrLeaseLost", err)
	}
	if _, err := queue.Fail(context.Background(), *stale, errors.New("late"), time.Now()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Fail error = %v, want ErrLeaseLost", err)
	}
	if err := queue.Renew(context.Background(), *stale, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Renew error = %v, want ErrLeaseLost", err)
	}
}

func TestRetryBackoffAndAttemptExhaustion(t *testing.T) {
	queue, db := openTestQueue(t)
	id, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "retry", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().Add(2 * time.Second)
	if _, err := queue.Fail(context.Background(), *first, errors.New("temporary"), retryAt); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim != nil {
		t.Fatal("job was claimed before retry_at")
	}
	if _, err := db.Pool.Exec(context.Background(), "UPDATE sync_jobs SET available_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	second, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	if _, err := queue.Fail(context.Background(), *second, errors.New("permanent"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := db.Pool.QueryRow(context.Background(), "SELECT status, attempts FROM sync_jobs WHERE id=$1", id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "dead" || attempts != 2 {
		t.Fatalf("status, attempts = %q, %d; want dead, 2", status, attempts)
	}
	claim, err = queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil || claim != nil {
		t.Fatalf("exhausted claim = %#v, %v", claim, err)
	}
}

func TestParallelClaimsAreUnique(t *testing.T) {
	queue, _ := openTestQueue(t)
	const count = 16
	for i := 0; i < count; i++ {
		_, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan *Lease, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			lease, err := queue.Claim(context.Background(), LaneInteractive, uuid.NewString(), time.Minute)
			if err != nil {
				errs <- err
				return
			}
			results <- lease
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seenJobs := map[uuid.UUID]bool{}
	seenTokens := map[uuid.UUID]bool{}
	for lease := range results {
		if lease == nil {
			t.Fatal("parallel claim unexpectedly returned nil")
		}
		if seenJobs[lease.ID] || seenTokens[lease.FencingToken] {
			t.Fatalf("duplicate parallel lease: %#v", lease)
		}
		seenJobs[lease.ID] = true
		seenTokens[lease.FencingToken] = true
	}
	if len(seenJobs) != count {
		t.Fatalf("claimed %d unique jobs, want %d", len(seenJobs), count)
	}
}

func TestDeferredParentPreservesItsFinalAttemptForCompletion(t *testing.T) {
	queue, db := openTestQueue(t)
	parentID, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "parent", IdempotencyKey: "parent-final-attempt", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Fail(context.Background(), *first, errors.New("temporary"), time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil || second == nil || second.Attempts != 2 {
		t.Fatalf("final parent attempt = %#v, %v", second, err)
	}
	ready, err := queue.AwaitChildren(context.Background(), *second, []Spec{{Lane: LaneInteractive, Kind: "child", IdempotencyKey: "parent-final-attempt:child", Required: true}})
	if err != nil || ready {
		t.Fatalf("AwaitChildren = %v, %v; want deferred", ready, err)
	}
	var attempts int
	if err := db.Pool.QueryRow(context.Background(), `SELECT attempts FROM sync_jobs WHERE id=$1`, parentID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("deferred parent attempts = %d, want 1", attempts)
	}
	child, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil || child == nil || child.Kind != "child" {
		t.Fatalf("child claim = %#v, %v", child, err)
	}
	if err := queue.Complete(context.Background(), *child); err != nil {
		t.Fatal(err)
	}
	resumed, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil || resumed == nil || resumed.ID != parentID || resumed.Attempts != 2 {
		t.Fatalf("resumed parent = %#v, %v", resumed, err)
	}
}

func TestExpiredRequiredChildFailsWaitingParent(t *testing.T) {
	queue, db := openTestQueue(t)
	parentID, err := queue.Enqueue(context.Background(), Spec{Lane: LaneInteractive, Kind: "parent", IdempotencyKey: "expired-child-parent"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := queue.Claim(context.Background(), LaneInteractive, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := queue.AwaitChildren(context.Background(), *parent, []Spec{{Lane: LaneInteractive, Kind: "child", IdempotencyKey: "expired-child-parent:child", Required: true, MaxAttempts: 1}})
	if err != nil || ready {
		t.Fatalf("AwaitChildren = %v, %v; want deferred", ready, err)
	}
	child, err := queue.Claim(context.Background(), LaneInteractive, "worker", 15*time.Millisecond)
	if err != nil || child == nil {
		t.Fatalf("child claim = %#v, %v", child, err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := queue.Claim(context.Background(), LaneInteractive, "cleanup", time.Minute); err != nil {
		t.Fatal(err)
	}
	var status, lastError string
	if err := db.Pool.QueryRow(context.Background(), `SELECT status,last_error FROM sync_jobs WHERE id=$1`, parentID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "dead" || lastError != "required child stage failed" {
		t.Fatalf("parent terminal state = %s/%q", status, lastError)
	}
}

func openTestQueue(t *testing.T) (*Queue, *store.Store) {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		conn.Close(ctx)
		t.Fatalf("create isolated schema: %v", err)
	}
	conn.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	db, err := store.Open(context.Background(), fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema))
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup, err := pgx.Connect(cleanupCtx, baseURL)
		if err == nil {
			_, _ = cleanup.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(cleanupCtx)
		}
	})
	return NewQueue(db), db
}
