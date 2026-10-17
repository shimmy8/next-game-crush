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
)

func TestEnqueueIsIdempotent(t *testing.T) {
	queue, _ := openTestQueue(t)
	spec := Spec{Kind: "sync", IdempotencyKey: "sync:user:1", Payload: []byte(`{"user":"1"}`), MaxAttempts: 3}
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

func TestClaimSkipsLeasedJob(t *testing.T) {
	queue, _ := openTestQueue(t)
	_, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), "worker-a", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	second, err := queue.Claim(context.Background(), "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatalf("second worker claimed leased job: %#v", second)
	}
}

func TestExpiredLeaseCanBeReclaimed(t *testing.T) {
	queue, _ := openTestQueue(t)
	id, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "reclaim"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), "worker-a", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	second, err := queue.Claim(context.Background(), "worker-b", time.Minute)
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
	id, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "crash-loop", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), "worker-a", 15*time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	time.Sleep(30 * time.Millisecond)
	second, err := queue.Claim(context.Background(), "worker-b", 15*time.Millisecond)
	if err != nil || second == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	time.Sleep(30 * time.Millisecond)
	third, err := queue.Claim(context.Background(), "worker-c", time.Minute)
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
	_, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := queue.Claim(context.Background(), "worker-a", 15*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	current, err := queue.Claim(context.Background(), "worker-b", time.Minute)
	if err != nil || current == nil {
		t.Fatalf("reclaim = %#v, %v", current, err)
	}
	if err := queue.Complete(context.Background(), *stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Complete error = %v, want ErrLeaseLost", err)
	}
	if err := queue.Fail(context.Background(), *stale, errors.New("late"), time.Now()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Fail error = %v, want ErrLeaseLost", err)
	}
	if err := queue.Renew(context.Background(), *stale, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Renew error = %v, want ErrLeaseLost", err)
	}
}

func TestRetryBackoffAndAttemptExhaustion(t *testing.T) {
	queue, db := openTestQueue(t)
	id, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: "retry", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().Add(2 * time.Second)
	if err := queue.Fail(context.Background(), *first, errors.New("temporary"), retryAt); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim != nil {
		t.Fatal("job was claimed before retry_at")
	}
	if _, err := db.Pool.Exec(context.Background(), "UPDATE sync_jobs SET available_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	second, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	if err := queue.Fail(context.Background(), *second, errors.New("permanent"), time.Now()); err != nil {
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
	claim, err = queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil || claim != nil {
		t.Fatalf("exhausted claim = %#v, %v", claim, err)
	}
}

func TestParallelClaimsAreUnique(t *testing.T) {
	queue, _ := openTestQueue(t)
	const count = 16
	for i := 0; i < count; i++ {
		_, err := queue.Enqueue(context.Background(), Spec{Kind: "sync", IdempotencyKey: uuid.NewString()})
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
			lease, err := queue.Claim(context.Background(), uuid.NewString(), time.Minute)
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
	parentID, err := queue.Enqueue(context.Background(), Spec{Kind: "parent", IdempotencyKey: "parent-final-attempt", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Fail(context.Background(), *first, errors.New("temporary"), time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil || second == nil || second.Attempts != 2 {
		t.Fatalf("final parent attempt = %#v, %v", second, err)
	}
	ready, err := queue.AwaitChildren(context.Background(), *second, []Spec{{Kind: "child", IdempotencyKey: "parent-final-attempt:child", Required: true}})
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
	child, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil || child == nil || child.Kind != "child" {
		t.Fatalf("child claim = %#v, %v", child, err)
	}
	if err := queue.Complete(context.Background(), *child); err != nil {
		t.Fatal(err)
	}
	resumed, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil || resumed == nil || resumed.ID != parentID || resumed.Attempts != 2 {
		t.Fatalf("resumed parent = %#v, %v", resumed, err)
	}
}

func TestExpiredRequiredChildFailsWaitingParent(t *testing.T) {
	queue, db := openTestQueue(t)
	parentID, err := queue.Enqueue(context.Background(), Spec{Kind: "parent", IdempotencyKey: "expired-child-parent"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := queue.Claim(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := queue.AwaitChildren(context.Background(), *parent, []Spec{{Kind: "child", IdempotencyKey: "expired-child-parent:child", Required: true, MaxAttempts: 1}})
	if err != nil || ready {
		t.Fatalf("AwaitChildren = %v, %v; want deferred", ready, err)
	}
	child, err := queue.Claim(context.Background(), "worker", 15*time.Millisecond)
	if err != nil || child == nil {
		t.Fatalf("child claim = %#v, %v", child, err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := queue.Claim(context.Background(), "cleanup", time.Minute); err != nil {
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
