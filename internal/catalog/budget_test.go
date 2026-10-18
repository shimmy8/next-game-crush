package catalog

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"next-game-crush/internal/jobs"
)

func TestStoreAndAIBudgetsSurviveRestartAndCompetingInstances(t *testing.T) {
	db := openCatalogTestStore(t)
	a, b := NewSearchRepository(db), NewSearchRepository(db)
	now := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)
	ctx := context.Background()
	for _, repo := range []*SearchRepository{a, b, a, b} {
		granted, _, err := repo.ReserveBudget(ctx, "appdetails", "same-request", 1, now)
		if err != nil || !granted {
			t.Fatalf("idempotent reservation=%v err=%v", granted, err)
		}
	}
	granted, next, err := b.ReserveBudget(ctx, "appdetails", "different-request", 1, now)
	if err != nil || granted || !next.Equal(now.Truncate(time.Hour).Add(time.Hour)) {
		t.Fatalf("full=%v next=%v err=%v", granted, next, err)
	}
	if granted, _, err = a.ReserveBudget(ctx, "ai", "feature:1", 1, now); err != nil || !granted {
		t.Fatalf("independent ai=%v err=%v", granted, err)
	}

	// Separate repository values model separate processes sharing the same
	// database bucket. Exactly the limit may win under contention.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			repo := a
			if i%2 != 0 {
				repo = b
			}
			ok, _, err := repo.ReserveBudget(ctx, "ai", fmt.Sprintf("feature:%d", i+100), 10, now.Add(time.Hour))
			if err != nil {
				t.Errorf("reserve %d: %v", i, err)
				return
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 10 {
		t.Fatalf("competing wins=%d", wins.Load())
	}
}

func TestTransportRetryDoesNotSpendAnotherUniqueUnit(t *testing.T) {
	repo := NewSearchRepository(openCatalogTestStore(t))
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for range 4 {
		if granted, _, err := repo.ReserveBudget(context.Background(), "appdetails", "app:620:reservation:abc", 1, now); err != nil || !granted {
			t.Fatalf("retry granted=%v err=%v", granted, err)
		}
	}
	var used int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT used FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

func TestBudgetExhaustionDefersWithoutDeadLetter(t *testing.T) {
	repo := NewSearchRepository(openCatalogTestStore(t))
	now := time.Date(2026, 10, 18, 12, 59, 0, 0, time.UTC)
	if ok, _, err := repo.ReserveBudget(context.Background(), "appdetails", "first", 1, now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ok, next, err := repo.ReserveBudget(context.Background(), "appdetails", "second", 1, now)
	if err != nil || ok || !next.Equal(time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("ok=%v next=%v err=%v", ok, next, err)
	}
	queue := jobs.NewQueue(repo.store)
	id, err := queue.Enqueue(context.Background(), jobs.Spec{Lane: jobs.LaneCatalog, Kind: "budget-wait", IdempotencyKey: "budget-wait:1", MaxAttempts: 4})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(context.Background(), jobs.LaneCatalog, "budget-worker", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim=%v err=%v", lease, err)
	}
	// The fixture clock is historical, while Queue validates against wall time.
	deferAt := time.Now().UTC().Add(time.Minute)
	if err := queue.DeferUntil(context.Background(), *lease, deferAt); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT status,attempts FROM sync_jobs WHERE id=$1`, id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempts != 0 {
		t.Fatalf("deferred status=%s attempts=%d", status, attempts)
	}
}

func TestPruneGateHistoryKeepsReferencedAndNonterminalState(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour).Truncate(time.Hour)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_budget_reservations(kind,hour_at,reservations,used) VALUES
		('appdetails',$1,'{"orphan":true}',1),('appdetails',$2,'{"reserved":true}',1),
		('appdetails',$3,'{"work-referenced":true}',1)`, old, old.Add(time.Hour), old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_store_reservations(steam_app_id,run_revision,observation_ordinal,status,lease_until,updated_at) VALUES
        (1001,1,1,'released',$1,$2)`, now.Add(-time.Hour), old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_store_reservations(steam_app_id,run_revision,observation_ordinal,status,lease_until,updated_at,budget_hour,budget_identity_hash) VALUES
        (1002,1,1,'reserved',$1,$2,$3,$4)`, now.Add(time.Hour), old, old.Add(time.Hour), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_store_reservations(steam_app_id,run_revision,observation_ordinal,status,lease_until,updated_at,budget_hour,budget_identity_hash) VALUES
		(1003,1,1,'released',$1,$2,$3,$4)`, now.Add(-time.Hour), old, old.Add(2*time.Hour), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_metadata_ingress_work(steam_app_id,ingress_kind,ingress_identity,run_revision,observation_ordinal,
		material_run_revision,material_observation_ordinal,status) VALUES
		(1003,'user_sync','prune-test',1,1,1,1,'deferred')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_host_throttle_slots SET fencing_token=gen_random_uuid(),lease_until=$1,updated_at=$2 WHERE host='store.steampowered.com' AND slot=1`, now.Add(-time.Hour), old); err != nil {
		t.Fatal(err)
	}
	if err := repo.PruneGateHistory(ctx, now.Add(-24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	var orphan, reserved, workReferenced, budgets, slots int
	if err := db.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER(WHERE steam_app_id=1001),count(*) FILTER(WHERE steam_app_id=1002),
		count(*) FILTER(WHERE steam_app_id=1003),
        (SELECT count(*) FROM catalog_budget_reservations),
        (SELECT count(*) FROM catalog_host_throttle_slots WHERE fencing_token IS NOT NULL)
		FROM catalog_store_reservations`).Scan(&orphan, &reserved, &workReferenced, &budgets, &slots); err != nil {
		t.Fatal(err)
	}
	if orphan != 0 || reserved != 1 || workReferenced != 1 || budgets != 2 || slots != 0 {
		t.Fatalf("orphan=%d reserved=%d work-referenced=%d budgets=%d slots=%d", orphan, reserved, workReferenced, budgets, slots)
	}
}
