package jobs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

func TestEnqueueAndClaimSQLUseTheSameLane(t *testing.T) {
	_, db := openTestQueue(t)
	ctx := context.Background()
	for _, lane := range []Lane{LaneInteractive, LaneCatalog} {
		id, err := db.Queries.EnqueueJob(ctx, dbgen.EnqueueJobParams{
			ID: uuidToPG(uuid.New()), Lane: string(lane), Kind: "lane-test", IdempotencyKey: string(lane),
			Payload: []byte(`{}`), MaxAttempts: 3, Priority: 50, Required: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := db.Pool.QueryRow(ctx, `SELECT lane FROM sync_jobs WHERE id=$1`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != string(lane) {
			t.Fatalf("stored lane=%s, want %s", stored, lane)
		}
		row, err := db.Queries.ClaimJob(ctx, dbgen.ClaimJobParams{Lane: string(lane), WorkerID: textToPG("worker"), FencingToken: uuidToPG(uuid.New()), LeaseDuration: durationToPG(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if row.ID != id || row.Lane != string(lane) {
			t.Fatalf("claimed wrong lane: %s", row.Lane)
		}
	}
}

func TestDuplicateKeyCannotMoveLanes(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "sync", IdempotencyKey: "duplicate-lane", Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, Spec{Lane: LaneCatalog, Kind: "catalog", IdempotencyKey: "duplicate-lane", Priority: 100}); err == nil {
		t.Fatal("duplicate moved lanes")
	}
	var lane string
	var priority int
	if err := db.Pool.QueryRow(ctx, `SELECT lane,priority FROM sync_jobs WHERE id=$1`, id).Scan(&lane, &priority); err != nil {
		t.Fatal(err)
	}
	if lane != string(LaneInteractive) || priority != 10 {
		t.Fatalf("duplicate mutated job: %s/%d", lane, priority)
	}
}

func TestLaneClaimCannotCross(t *testing.T) {
	for _, lane := range []Lane{LaneInteractive, LaneCatalog} {
		t.Run(string(lane), func(t *testing.T) {
			queue, _ := openTestQueue(t)
			other := LaneCatalog
			if lane == LaneCatalog {
				other = LaneInteractive
			}
			if _, err := queue.Enqueue(context.Background(), Spec{Lane: other, Kind: "other", IdempotencyKey: "other"}); err != nil {
				t.Fatal(err)
			}
			if lease, err := queue.Claim(context.Background(), lane, "worker", time.Minute); err != nil || lease != nil {
				t.Fatalf("cross-lane claim: %v/%v", lease, err)
			}
		})
	}
}

func TestLaneValidationBeforeSQL(t *testing.T) {
	for _, lane := range []Lane{"", "unknown"} {
		if _, err := EnqueueInTx(context.Background(), dbgen.New(nil), Spec{Lane: lane, Kind: "root", IdempotencyKey: "root"}); err == nil {
			t.Fatal("invalid root lane accepted")
		}
		if _, err := EnqueueInTx(context.Background(), dbgen.New(nil), Spec{Lane: "unknown", Kind: "child", IdempotencyKey: "child", ParentJobID: uuid.New()}); err == nil {
			t.Fatal("invalid child lane accepted")
		}
		queue := NewQueue(&store.Store{})
		if _, err := queue.Claim(context.Background(), lane, "worker", time.Minute); err == nil {
			t.Fatal("invalid claim lane accepted")
		}
	}
}

func TestChildSafelyInheritsLane(t *testing.T) {
	for _, lane := range []Lane{LaneInteractive, LaneCatalog} {
		t.Run(string(lane), func(t *testing.T) {
			queue, db := openTestQueue(t)
			ctx := context.Background()
			parent, err := queue.Enqueue(ctx, Spec{Lane: lane, Kind: "parent", IdempotencyKey: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			for i, childLane := range []Lane{"", lane} {
				child, err := queue.Enqueue(ctx, Spec{Lane: childLane, Kind: "child", IdempotencyKey: fmt.Sprintf("child:%d", i), ParentJobID: parent})
				if err != nil {
					t.Fatal(err)
				}
				var stored string
				if err := db.Pool.QueryRow(ctx, `SELECT lane FROM sync_jobs WHERE id=$1`, child).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if stored != string(lane) {
					t.Fatalf("child lane=%s, want %s", stored, lane)
				}
			}
			lease, err := queue.Claim(ctx, lane, "worker", time.Minute)
			if err != nil || lease == nil {
				t.Fatalf("parent claim: %v", err)
			}
			if _, err := queue.AwaitChildren(ctx, *lease, []Spec{{Kind: "awaited", IdempotencyKey: "awaited"}}); err != nil {
				t.Fatal(err)
			}
			var stored string
			if err := db.Pool.QueryRow(ctx, `SELECT lane FROM sync_jobs WHERE idempotency_key='awaited'`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != string(lane) {
				t.Fatalf("awaited child lane=%s", stored)
			}
		})
	}
}

func TestCrossLaneChildRequiresCatalogRebuildBridge(t *testing.T) {
	queue, db := openTestQueue(t)
	ctx := context.Background()
	parent, err := queue.Enqueue(ctx, Spec{Lane: LaneCatalog, Kind: "catalog", IdempotencyKey: "catalog"})
	if err != nil {
		t.Fatal(err)
	}
	child := Spec{Lane: LaneInteractive, Kind: "recommendations.rebuild", IdempotencyKey: "rebuild", ParentJobID: parent, UserID: uuid.New()}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, child.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, child); err == nil {
		t.Fatal("cross-lane child accepted")
	}
	lease, err := queue.Claim(ctx, LaneCatalog, "catalog", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	// An invalid child must fail validation even without a usable SQL connection.
	if _, err := NewQueue(&store.Store{}).AwaitChildren(ctx, *lease, []Spec{child}); err == nil {
		t.Fatal("AwaitChildren accepted cross-lane injection")
	}
	if _, err := NewQueue(&store.Store{}).AwaitChildren(ctx, *lease, []Spec{{Lane: "unknown", Kind: "bad", IdempotencyKey: "bad"}}); err == nil {
		t.Fatal("AwaitChildren accepted unknown lane")
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	child.ParentJobID = uuid.Nil
	if _, err := EnqueueInTx(ctx, db.Queries.WithTx(tx), child); err != nil {
		t.Fatalf("detached interactive rebuild rejected: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := queue.Claim(ctx, LaneInteractive, "interactive", time.Minute)
	if err != nil || got == nil || got.ParentJobID != uuid.Nil || got.UserID != child.UserID {
		t.Fatalf("detached bridge=%v/%v", got, err)
	}
}

func TestCatalogBacklogCannotConsumeInteractiveCapacity(t *testing.T) {
	queue, _ := openTestQueue(t)
	ctx := context.Background()
	for i := range 100 {
		if _, err := queue.Enqueue(ctx, Spec{Lane: LaneCatalog, Kind: "catalog", IdempotencyKey: fmt.Sprintf("catalog:%d", i), Priority: 100}); err != nil {
			t.Fatal(err)
		}
	}
	id, err := queue.Enqueue(ctx, Spec{Lane: LaneInteractive, Kind: "steam.sync", IdempotencyKey: "sync", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := range 4 {
		workers.Go(func() {
			lease, err := queue.Claim(ctx, LaneCatalog, fmt.Sprintf("catalog:%d", i), time.Minute)
			if err != nil || lease == nil {
				t.Errorf("catalog claim: %v", err)
				return
			}
			if lease.Lane != LaneCatalog || lease.Kind != "catalog" {
				t.Error("catalog worker claimed interactive job")
			}
		})
	}
	workers.Wait()
	lease, err := queue.Claim(ctx, LaneInteractive, "interactive", time.Minute)
	if err != nil || lease == nil || lease.ID != id || lease.Lane != LaneInteractive {
		t.Fatalf("interactive sync blocked: %v/%v", lease, err)
	}
}
