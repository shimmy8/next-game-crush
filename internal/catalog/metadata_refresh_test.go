package catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

func insertReservationCandidate(t *testing.T, repo *SearchRepository, appID int64, rev CandidateRevision, fingerprint string, next time.Time) {
	t.Helper()
	_, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO catalog_search_current
        (steam_app_id,run_revision,observation_ordinal,fingerprint,release_text,precision,review_band,next_refresh_at)
        VALUES($1,$2,$3,$4,'Oct 18, 2026','day','positive',$5)`, appID, rev.RunRevision, rev.ObservationOrdinal, fingerprint, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Queries.UpsertMetadataCandidateHead(context.Background(), dbgen.UpsertMetadataCandidateHeadParams{
		SteamAppID: appID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal,
		Fingerprint: fingerprint, NowAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateReservationDeduplicatesEveryIngress(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	rev := CandidateRevision{RunRevision: 1, ObservationOrdinal: 1}
	insertReservationCandidate(t, repo, 620, rev, "stable-product", now.Add(-time.Minute))
	for _, feed := range []string{"popular_new", "recent", "coming_soon"} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_memberships(feed,steam_app_id,published_run_revision) VALUES($1,620,1)`, feed); err != nil {
			t.Fatal(err)
		}
	}
	var winners, charged atomic.Int32
	var winner MetadataReservation
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reservation, err := repo.ReserveMetadata(ctx, 620, rev, "stable-product", now)
			if err != nil {
				t.Errorf("ingress %d: %v", i, err)
				return
			}
			if reservation.Granted {
				winners.Add(1)
				mu.Lock()
				winner = reservation
				mu.Unlock()
				granted, _, err := repo.ReserveBudget(ctx, "appdetails", fmt.Sprintf("reservation:%s", reservation.ID), 1000, now)
				if err != nil || !granted {
					t.Errorf("budget: %v granted=%v", err, granted)
					return
				}
				charged.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 || charged.Load() != 1 {
		t.Fatalf("winners=%d charged=%d", winners.Load(), charged.Load())
	}
	var provenance, used int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog_search_memberships WHERE steam_app_id=620),(SELECT used FROM catalog_budget_reservations WHERE kind='appdetails')`).Scan(&provenance, &used); err != nil {
		t.Fatal(err)
	}
	if provenance != 3 || used != 1 {
		t.Fatalf("provenance=%d used=%d", provenance, used)
	}
	if err := repo.ReleaseMetadata(ctx, winner, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// A review-only revision retains the stable search fingerprint and stays
	// inside metadata TTL, so it updates search state without Store/AI work.
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_current SET run_revision=2,observation_ordinal=1,review_total=99,positive_percent=95,next_refresh_at=$2 WHERE steam_app_id=$1`, 620, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.ReserveMetadata(ctx, 620, CandidateRevision{2, 1}, "stable-product", now.Add(2*time.Second)); err != nil || got.Granted {
		t.Fatalf("review-only reservation=%+v err=%v", got, err)
	}
	// A material fingerprint change wakes immediately despite next_refresh_at.
	if got, err := repo.ReserveMetadata(ctx, 620, CandidateRevision{3, 1}, "changed-product", now.Add(3*time.Second)); err != nil || !got.Granted {
		t.Fatalf("material reservation=%+v err=%v", got, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT used FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("unexpected budget used=%d err=%v", used, err)
	}
}

func TestUserSyncRefreshUsesDurableSearchQualification(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	queue := jobs.NewQueue(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_, rev := insertMetadataRun(t, db, "daily", "scheduled", now.Add(-time.Hour), 621, "user-sync", "complete", nil)
	insertReservationCandidate(t, repo, 621, rev, "user-sync", now.Add(-time.Minute))
	cfg := DefaultDiscoveryConfig()
	count, err := repo.EnqueueUserSyncCandidates(ctx, queue, cfg, "sync:durable", []RefreshTarget{{AppID: 621, Priority: 100}}, now)
	if err != nil || count != 1 {
		t.Fatalf("enqueued=%d err=%v", count, err)
	}
	var kind, lane, status string
	if err := db.Pool.QueryRow(ctx, `SELECT kind,lane,status FROM sync_jobs WHERE kind=$1`, SearchQualificationJobKind).Scan(&kind, &lane, &status); err != nil {
		t.Fatal(err)
	}
	if kind != SearchQualificationJobKind || lane != string(jobs.LaneCatalog) || status != "queued" {
		t.Fatalf("job kind=%q lane=%q status=%q", kind, lane, status)
	}
	var ingressStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_metadata_ingress_work WHERE steam_app_id=621 AND ingress_kind='user_sync'`).Scan(&ingressStatus); err != nil {
		t.Fatal(err)
	}
	if ingressStatus != "reserved" {
		t.Fatalf("ingress status=%q", ingressStatus)
	}
}

func TestUserSyncBootstrapsHeadlessCandidateIntoSharedQualification(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	queue := jobs.NewQueue(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	cfg := DefaultDiscoveryConfig()

	count, err := repo.EnqueueUserSyncCandidates(ctx, queue, cfg, "sync:headless", []RefreshTarget{{AppID: 622, Priority: 100}}, now)
	if err != nil || count != 1 {
		t.Fatalf("enqueued=%d err=%v", count, err)
	}
	var currentRun, currentOrdinal, headRun, headOrdinal int64
	var currentFingerprint, headFingerprint, lane, kind string
	if err := db.Pool.QueryRow(ctx, `SELECT c.material_run_revision,c.material_observation_ordinal,c.material_fingerprint,
		h.material_run_revision,h.material_observation_ordinal,h.material_fingerprint
		FROM catalog_search_current c JOIN catalog_metadata_heads h USING(steam_app_id)
		WHERE c.steam_app_id=622`).Scan(&currentRun, &currentOrdinal, &currentFingerprint, &headRun, &headOrdinal, &headFingerprint); err != nil {
		t.Fatal(err)
	}
	if currentRun < 1 || currentOrdinal != 1 || currentRun != headRun || currentOrdinal != headOrdinal ||
		currentFingerprint != "user_sync:v1" || currentFingerprint != headFingerprint {
		t.Fatalf("current=%d/%d/%q head=%d/%d/%q", currentRun, currentOrdinal, currentFingerprint, headRun, headOrdinal, headFingerprint)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT lane,kind FROM sync_jobs WHERE kind=$1`, SearchQualificationJobKind).Scan(&lane, &kind); err != nil {
		t.Fatal(err)
	}
	if lane != string(jobs.LaneCatalog) || kind != SearchQualificationJobKind {
		t.Fatalf("job lane=%q kind=%q", lane, kind)
	}

	// A later user-sync occurrence shares the existing Store lease instead of
	// issuing a duplicate request while the first qualification is in flight.
	count, err = repo.EnqueueUserSyncCandidates(ctx, queue, cfg, "sync:headless:replay", []RefreshTarget{{AppID: 622, Priority: 100}}, now.Add(time.Second))
	if err != nil || count != 0 {
		t.Fatalf("replay enqueued=%d err=%v", count, err)
	}
	var jobsCount int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1`, SearchQualificationJobKind).Scan(&jobsCount); err != nil {
		t.Fatal(err)
	}
	if jobsCount != 1 {
		t.Fatalf("qualification jobs=%d, want 1", jobsCount)
	}

	// Headless revisions consume the same global allocator, so any later Search
	// observation is necessarily newer and can safely supersede this material.
	_, searchRevision := insertMetadataRun(t, db, "daily", "scheduled", now.Add(time.Hour), 700, "search", "complete", nil)
	if searchRevision.RunRevision <= currentRun {
		t.Fatalf("search revision=%d, want > headless revision=%d", searchRevision.RunRevision, currentRun)
	}
}

func TestUserSyncHeadlessIngressRecoversAcrossCrashBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		reserveBeforeRun bool
	}{
		{name: "after bootstrap"},
		{name: "after reservation before enqueue", reserveBeforeRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openCatalogTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			cfg := DefaultDiscoveryConfig()
			ingress := MetadataIngress{Kind: "user_sync", Identity: "sync:crash:" + tc.name}
			target := RefreshTarget{AppID: 623, Priority: 100}
			repo := NewSearchRepository(db)
			rev, fingerprint, err := repo.ensureUserSyncCandidate(ctx, ingress, target, now)
			if err != nil {
				t.Fatal(err)
			}
			if tc.reserveBeforeRun {
				item, err := repo.ReserveIngressMetadata(ctx, ingress, target.AppID, rev, fingerprint, cfg.AppDetailsHourlyBudget, now)
				if err != nil || !item.Reservation.Granted {
					t.Fatalf("reservation=%+v err=%v", item, err)
				}
			}
			// Simulate a process restart: only durable DB state is available to a
			// fresh producer and no qualification job was enqueued by the old one.
			restarted := NewSearchRepository(db)
			queue := jobs.NewQueue(db)
			count, err := restarted.EnqueueReadyIngressQualifications(ctx, queue, cfg, now.Add(time.Second))
			if err != nil || count != 1 {
				t.Fatalf("recovered=%d err=%v", count, err)
			}
			var jobsCount int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1 AND lane='catalog'`, SearchQualificationJobKind).Scan(&jobsCount); err != nil {
				t.Fatal(err)
			}
			if jobsCount != 1 {
				t.Fatalf("qualification jobs=%d, want 1", jobsCount)
			}
		})
	}
}

func TestMetadataTTLByReleasePhaseAndTombstone(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	date := now.AddDate(0, 0, 2)
	far := now.AddDate(0, 0, 10)
	cases := []struct {
		name       string
		window     ReleaseWindow
		tombstone  string
		generation int
		want       time.Duration
	}{
		{"near exact", ReleaseWindow{Precision: PrecisionDay, Date: &date}, "", 0, 24 * time.Hour},
		{"far exact", ReleaseWindow{Precision: PrecisionDay, Date: &far}, "", 0, 72 * time.Hour},
		{"month", ReleaseWindow{Precision: PrecisionMonth}, "", 0, 72 * time.Hour},
		{"coming soon", ReleaseWindow{Precision: PrecisionComingSoon}, "", 0, 7 * 24 * time.Hour},
		{"stable tombstone", ReleaseWindow{}, "mixed_or_below", 0, 30 * 24 * time.Hour},
		{"first exhausted", ReleaseWindow{}, "unavailable", 1, 24 * time.Hour},
		{"second exhausted", ReleaseWindow{}, "unavailable", 2, 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MetadataRefreshTTL(tc.window, tc.tombstone, tc.generation, now); got != tc.want {
				t.Fatalf("ttl=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestMetadataReservationFencesStaleRaceAndMaterialPreempts(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC()
	insertReservationCandidate(t, repo, 777, CandidateRevision{1, 1}, "old", now.Add(-time.Hour))
	first, err := repo.ReserveMetadata(ctx, 777, CandidateRevision{1, 1}, "old", now)
	if err != nil || !first.Granted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_current SET run_revision=2,observation_ordinal=1,fingerprint='new' WHERE steam_app_id=777`); err != nil {
		t.Fatal(err)
	}
	preempt, err := repo.ReserveMetadata(ctx, 777, CandidateRevision{2, 1}, "new", now.Add(time.Second))
	if err != nil || !preempt.Granted || preempt.FencingToken == first.FencingToken {
		t.Fatalf("preempt=%+v err=%v", preempt, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_current SET run_revision=3,observation_ordinal=1 WHERE steam_app_id=777`); err != nil {
		t.Fatal(err)
	}
	duplicate, err := repo.ReserveMetadata(ctx, 777, CandidateRevision{3, 1}, "new", now.Add(2*time.Second))
	if err != nil || duplicate.Granted || duplicate.Reason != "leased" {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}

	// Commit a newer projection while an older reservation statement waits on
	// the current-row lock. Read Committed row-lock rechecking must reject old.
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(777)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_metadata_heads SET latest_run_revision=5,latest_observation_ordinal=1,latest_fingerprint='latest',material_run_revision=5,material_observation_ordinal=1,material_fingerprint='latest' WHERE steam_app_id=777`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_search_current SET run_revision=5,observation_ordinal=1,fingerprint='latest' WHERE steam_app_id=777`); err != nil {
		t.Fatal(err)
	}
	done := make(chan MetadataReservation, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := repo.ReserveMetadata(ctx, 777, CandidateRevision{4, 1}, "stale", now.Add(3*time.Second))
		done <- got
		errs <- err
	}()
	time.Sleep(25 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := <-done, <-errs; err != nil || got.Granted || got.Reason != "stale" {
		t.Fatalf("stale=%+v err=%v", got, err)
	}
}

func insertMetadataRun(t *testing.T, db *store.Store, schedule, kind string, occurrence time.Time, appID int64, fingerprint, status string, next *time.Time) (uuid.UUID, CandidateRevision) {
	t.Helper()
	var id uuid.UUID
	var revision int64
	if err := db.Pool.QueryRow(context.Background(), `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind) VALUES($1,$2,$3) RETURNING id,run_revision`, schedule, occurrence, kind).Scan(&id, &revision); err != nil {
		t.Fatal(err)
	}
	var ordinal int64 = 1
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO catalog_search_worklist(run_id,steam_app_id,run_revision,observation_ordinal,status,next_retry_at) VALUES($1,$2,$3,$4,$5,$6)`, id, appID, revision, ordinal, status, next); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO catalog_metadata_provenance(steam_app_id,ingress_kind,ingress_identity,run_revision,observation_ordinal) VALUES($1,$2,$3,$4,$5)`, appID, kind, id.String(), revision, ordinal); err != nil {
		t.Fatal(err)
	}
	return id, CandidateRevision{revision, ordinal}
}

func TestMetadataWorkflowOrdersAndDurablyDefers(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_, dueRev := insertMetadataRun(t, db, "reconciliation", "reconciliation", now.Add(-3*time.Hour), 801, "due", "deferred", func() *time.Time { x := now.Add(-time.Minute); return &x }())
	_, searchRev := insertMetadataRun(t, db, "daily", "scheduled", now.Add(-2*time.Hour), 802, "search", "pending", nil)
	_, reconRev := insertMetadataRun(t, db, "reconciliation", "reconciliation", now.Add(-time.Hour), 803, "recon", "pending", nil)
	for app, item := range map[int64]struct {
		rev CandidateRevision
		fp  string
	}{801: {dueRev, "due"}, 802: {searchRev, "search"}, 803: {reconRev, "recon"}} {
		insertReservationCandidate(t, repo, app, item.rev, item.fp, now.Add(-time.Minute))
	}
	for _, want := range []int64{801, 802, 803} {
		item, err := repo.ClaimMetadataWork(ctx, 10, now)
		if err != nil || !item.BudgetGranted || item.Reservation.AppID != want {
			t.Fatalf("want %d item=%+v err=%v", want, item, err)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_worklist SET status='complete' WHERE steam_app_id=$1`, want); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE catalog_store_reservations SET status='complete' WHERE steam_app_id=$1`, want); err != nil {
			t.Fatal(err)
		}
	}

	// Exhaust a new hour and ensure the next candidate remains durable deferred.
	later := now.Add(time.Hour)
	_, blockedRev := insertMetadataRun(t, db, "manual", "manual", later, 804, "blocked", "pending", nil)
	insertReservationCandidate(t, repo, 804, blockedRev, "blocked", later.Add(-time.Minute))
	if ok, _, err := repo.ReserveBudget(ctx, "appdetails", "fills-hour", 1, later); err != nil || !ok {
		t.Fatal(ok, err)
	}
	item, err := repo.ClaimMetadataWork(ctx, 1, later)
	if err != nil || item.BudgetGranted || !item.NextEligibleAt.Equal(later.Truncate(time.Hour).Add(time.Hour)) {
		t.Fatalf("deferred=%+v err=%v", item, err)
	}
	var status string
	var next time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT status,next_retry_at FROM catalog_search_worklist WHERE steam_app_id=804`).Scan(&status, &next); err != nil {
		t.Fatal(err)
	}
	if status != "deferred" || !next.Equal(item.NextEligibleAt) {
		t.Fatalf("state=%s/%v", status, next)
	}
}

func TestAllMetadataIngressesConvergeOnOneReservationAndBudget(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_, scheduled := insertMetadataRun(t, db, "daily", "scheduled", now.Add(-3*time.Hour), 900, "same", "pending", nil)
	_, manual := insertMetadataRun(t, db, "manual", "manual", now.Add(-2*time.Hour), 900, "same", "pending", nil)
	_, reconciliation := insertMetadataRun(t, db, "reconciliation", "reconciliation", now.Add(-time.Hour), 900, "same", "pending", nil)
	insertReservationCandidate(t, repo, 900, reconciliation, "same", now.Add(-time.Minute))
	var granted atomic.Int32
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := repo.ClaimMetadataWork(ctx, 100, now)
			if err != nil && !errors.Is(err, ErrNoMetadataWork) {
				t.Errorf("claim: %v", err)
				return
			}
			if item.BudgetGranted {
				granted.Add(1)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		item, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "user_sync", Identity: "user:allowlisted"}, 900, reconciliation, "same", 100, now)
		if err != nil {
			t.Errorf("user sync: %v", err)
			return
		}
		if item.BudgetGranted {
			granted.Add(1)
		}
	}()
	wg.Wait()
	if granted.Load() != 1 {
		t.Fatalf("granted=%d", granted.Load())
	}
	var worklists, provenance, used, reservations int
	if err := db.Pool.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM catalog_search_worklist WHERE steam_app_id=900),
        (SELECT count(*) FROM catalog_metadata_provenance WHERE steam_app_id=900),
        (SELECT used FROM catalog_budget_reservations WHERE kind='appdetails'),
        (SELECT count(*) FROM catalog_store_reservations WHERE steam_app_id=900)`).Scan(&worklists, &provenance, &used, &reservations); err != nil {
		t.Fatal(err)
	}
	if worklists != 3 || provenance != 4 || used != 1 || reservations != 1 {
		t.Fatalf("worklists=%d provenance=%d used=%d reservations=%d revisions=%+v/%+v", worklists, provenance, used, reservations, scheduled, manual)
	}
}

func TestMetadataWorkRecoversClaimAfterCrashWithoutNewBudgetUnit(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_, rev := insertMetadataRun(t, db, "daily", "scheduled", now, 910, "crash", "pending", nil)
	insertReservationCandidate(t, repo, 910, rev, "crash", now.Add(-time.Minute))
	first, err := repo.ClaimMetadataWork(ctx, 10, now)
	if err != nil || !first.BudgetGranted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	restarted := NewSearchRepository(db)
	replayAt := now.Add(metadataReservationLease + time.Second)
	replayed, err := restarted.ClaimMetadataWork(ctx, 10, replayAt)
	if err != nil || !replayed.BudgetGranted || replayed.Reservation.FencingToken == first.Reservation.FencingToken {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	var used int
	if err := db.Pool.QueryRow(ctx, `SELECT used FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

func TestReserveMetadataMaterialAdvanceSupersedesSearchWorkImmediately(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	runID, oldRev := insertMetadataRun(t, db, "daily", "scheduled", now, 915, "old", "pending", nil)
	insertReservationCandidate(t, repo, 915, oldRev, "old", now.Add(-time.Minute))
	old, err := repo.ClaimMetadataWork(ctx, 10, now)
	if err != nil || !old.BudgetGranted {
		t.Fatalf("old=%+v err=%v", old, err)
	}
	newRev := CandidateRevision{RunRevision: oldRev.RunRevision + 1, ObservationOrdinal: 1}
	newReservation, err := repo.ReserveMetadata(ctx, 915, newRev, "new", now.Add(time.Second))
	if err != nil || !newReservation.Granted {
		t.Fatalf("new=%+v err=%v", newReservation, err)
	}
	var status string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_worklist WHERE run_id=$1 AND steam_app_id=915`, runID).Scan(&status); err != nil || status != "superseded" {
		t.Fatalf("old work status=%q err=%v", status, err)
	}
}

func TestNewerPageMakesReservedWorkClaimableWithoutStaleOverwrite(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	runID, oldRev := insertMetadataRun(t, db, "daily", "scheduled", now, 920, "old", "pending", nil)
	insertReservationCandidate(t, repo, 920, oldRev, "old", now.Add(-time.Minute))
	old, err := repo.ClaimMetadataWork(ctx, 10, now)
	if err != nil || !old.BudgetGranted {
		t.Fatal(old, err)
	}
	newRev := CandidateRevision{RunRevision: oldRev.RunRevision, ObservationOrdinal: oldRev.ObservationOrdinal + 1}
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_current SET run_revision=$2,observation_ordinal=$3,fingerprint='new' WHERE steam_app_id=$1`, 920, newRev.RunRevision, newRev.ObservationOrdinal); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Queries.UpsertMetadataCandidateHead(ctx, dbgen.UpsertMetadataCandidateHeadParams{SteamAppID: 920, RunRevision: newRev.RunRevision, ObservationOrdinal: newRev.ObservationOrdinal, Fingerprint: "new", NowAt: pgtype.Timestamptz{Time: now.Add(time.Second), Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Queries.UpsertSearchWorklist(ctx, dbgen.UpsertSearchWorklistParams{
		RunID: catalogUUID(runID), SteamAppID: 920, RunRevision: newRev.RunRevision,
		ObservationOrdinal: newRev.ObservationOrdinal, Priority: 90,
		MaterialRunRevision: newRev.RunRevision, MaterialObservationOrdinal: newRev.ObservationOrdinal,
	}); err != nil {
		t.Fatal(err)
	}
	newItem, err := repo.ClaimMetadataWork(ctx, 10, now.Add(2*time.Second))
	if err != nil || !newItem.BudgetGranted || newItem.RunID != runID || newItem.Reservation.FencingToken == old.Reservation.FencingToken {
		t.Fatalf("new=%+v err=%v", newItem, err)
	}
	stale, err := repo.ReserveMetadata(ctx, 920, oldRev, "old", now.Add(3*time.Second))
	if err != nil || stale.Granted || stale.Reason != "stale" {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
}

func TestMaterialBudgetDeferralPreservesEntitlementUntilNextHour(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	insertReservationCandidate(t, repo, 930, CandidateRevision{1, 1}, "old", now.Add(24*time.Hour))
	old, err := repo.ReserveMetadata(ctx, 930, CandidateRevision{1, 1}, "old", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseMetadata(ctx, old, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind) VALUES('manual',$1,'manual')`, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, newRev := insertMetadataRun(t, db, "manual", "manual", now, 930, "new", "pending", nil)
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_current SET run_revision=$2,observation_ordinal=$3,fingerprint='new',next_refresh_at=$4 WHERE steam_app_id=$1`, 930, newRev.RunRevision, newRev.ObservationOrdinal, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Queries.UpsertMetadataCandidateHead(ctx, dbgen.UpsertMetadataCandidateHeadParams{SteamAppID: 930, RunRevision: newRev.RunRevision, ObservationOrdinal: newRev.ObservationOrdinal, Fingerprint: "new", NowAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := repo.ReserveBudget(ctx, "appdetails", "fill", 1, now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	deferred, err := repo.ClaimMetadataWork(ctx, 1, now)
	if err != nil || deferred.BudgetGranted {
		t.Fatalf("deferred=%+v err=%v", deferred, err)
	}
	var fp string
	var revision int64
	if err := db.Pool.QueryRow(ctx, `SELECT fingerprint,run_revision FROM catalog_store_reservations WHERE steam_app_id=930`).Scan(&fp, &revision); err != nil {
		t.Fatal(err)
	}
	if fp != "old" || revision != 1 {
		t.Fatalf("reservation poisoned=%q/%d", fp, revision)
	}
	granted, err := repo.ClaimMetadataWork(ctx, 1, now.Truncate(time.Hour).Add(time.Hour))
	if err != nil || !granted.BudgetGranted || granted.Reservation.Revision != newRev {
		t.Fatalf("granted=%+v err=%v", granted, err)
	}
}

func TestIngressBudgetDeferralSurvivesRestart(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	rev := CandidateRevision{1, 1}
	insertReservationCandidate(t, repo, 940, rev, "ingress", now.Add(-time.Minute))
	if ok, _, err := repo.ReserveBudget(ctx, "appdetails", "fill", 1, now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	deferred, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "user_sync", Identity: "sync:940"}, 940, rev, "ingress", 1, now)
	if err != nil || deferred.BudgetGranted {
		t.Fatalf("deferred=%+v err=%v", deferred, err)
	}
	var status string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_metadata_ingress_work WHERE steam_app_id=940`).Scan(&status); err != nil || status != "deferred" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	restarted := NewSearchRepository(db)
	ready, err := restarted.ClaimIngressMetadataWork(ctx, 1, now.Truncate(time.Hour).Add(time.Hour))
	if err != nil || !ready.BudgetGranted {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	// Simulate a crash after the handoff. Expired lease replays with its original
	// budget identity instead of spending a second unit in another hour.
	replay, err := restarted.ClaimIngressMetadataWork(ctx, 1, now.Truncate(time.Hour).Add(2*time.Hour))
	if err != nil || !replay.BudgetGranted {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	var total int
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(sum(used),0)::integer FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&total); err != nil || total != 2 {
		t.Fatalf("budget total=%d err=%v", total, err)
	}
}

func TestReleaseMetadataDurablyRequeuesAndUsesCurrentHourBudget(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	_, rev := insertMetadataRun(t, db, "daily", "scheduled", now, 950, "retry", "pending", nil)
	insertReservationCandidate(t, repo, 950, rev, "retry", now.Add(-time.Minute))
	first, err := repo.ClaimMetadataWork(ctx, 10, now)
	if err != nil || !first.BudgetGranted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := repo.ReleaseMetadata(ctx, first.Reservation, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_worklist WHERE steam_app_id=950`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("search status=%q err=%v", status, err)
	}
	second, err := repo.ClaimMetadataWork(ctx, 10, now.Truncate(time.Hour).Add(time.Hour))
	if err != nil || !second.BudgetGranted || second.Reservation.FencingToken == first.Reservation.FencingToken {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	var used int
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(sum(used),0)::integer FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&used); err != nil || used != 2 {
		t.Fatalf("used=%d err=%v", used, err)
	}

	insertReservationCandidate(t, repo, 951, CandidateRevision{1, 1}, "ingress-retry", now.Add(-time.Minute))
	ingress, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "user_sync", Identity: "sync:951"}, 951, CandidateRevision{1, 1}, "ingress-retry", 10, now)
	if err != nil || !ingress.BudgetGranted {
		t.Fatalf("ingress=%+v err=%v", ingress, err)
	}
	if err := repo.ReleaseMetadata(ctx, ingress.Reservation, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_metadata_ingress_work WHERE steam_app_id=951`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("ingress status=%q err=%v", status, err)
	}
}

func TestReviewOnlyObservationPreservesInflightMaterialQualification(t *testing.T) {
	cfg := DefaultDiscoveryConfig()
	service, repo := newSearchScanForTest(t, &searchStub{}, cfg)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	firstRun := searchTestRun(t, repo, cfg, now)
	firstObservation := testObservation(960, "20 Oct, 2026", steam.ReviewPositive, "Positive")
	firstObservation.Review.Total = 10
	if err := service.CommitPage(ctx, firstRun, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{firstObservation}}); err != nil {
		t.Fatal(err)
	}
	first, err := repo.ClaimMetadataWork(ctx, 10, now)
	if err != nil || !first.BudgetGranted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	secondRun := searchTestRun(t, repo, cfg, now.Add(time.Hour))
	secondObservation := firstObservation
	secondObservation.Review.Total = 99
	if err := service.CommitPage(ctx, secondRun, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{secondObservation}}); err != nil {
		t.Fatal(err)
	}
	var latestRevision, materialRevision, materialOrdinal int64
	var firstStatus string
	if err := repo.store.Pool.QueryRow(ctx, `SELECT run_revision,material_run_revision,material_observation_ordinal FROM catalog_search_current WHERE steam_app_id=960`).Scan(&latestRevision, &materialRevision, &materialOrdinal); err != nil {
		t.Fatal(err)
	}
	if err := repo.store.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_worklist WHERE run_id=$1 AND steam_app_id=960`, firstRun.ID).Scan(&firstStatus); err != nil {
		t.Fatal(err)
	}
	if latestRevision != secondRun.Revision || materialRevision != first.Reservation.Revision.RunRevision || materialOrdinal != first.Reservation.Revision.ObservationOrdinal || firstStatus != "reserved" {
		t.Fatalf("latest=%d material=%d/%d reservation=%+v first_status=%s", latestRevision, materialRevision, materialOrdinal, first.Reservation.Revision, firstStatus)
	}
	duplicate, err := repo.ClaimMetadataWork(ctx, 10, now.Add(time.Second))
	if err != nil || duplicate.BudgetGranted || duplicate.Reservation.Reason != "deduplicated" {
		t.Fatalf("review duplicate=%+v err=%v", duplicate, err)
	}
	var used int
	if err := repo.store.Pool.QueryRow(ctx, `SELECT used FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("used=%d err=%v", used, err)
	}
	if err := repo.ReleaseMetadata(ctx, first.Reservation, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	retry, err := repo.ClaimMetadataWork(ctx, 10, now.Add(3*time.Second))
	if err != nil || !retry.BudgetGranted || retry.Reservation.Revision != first.Reservation.Revision {
		t.Fatalf("material retry=%+v err=%v", retry, err)
	}
}

func TestIngressReviewOnlyObservationPreservesMaterialReservation(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	insertReservationCandidate(t, repo, 965, CandidateRevision{1, 1}, "same", now.Add(-time.Minute))
	first, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "user_sync", Identity: "library"}, 965, CandidateRevision{1, 1}, "same", 10, now)
	if err != nil || !first.BudgetGranted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	reviewOnly, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "user_sync", Identity: "library"}, 965, CandidateRevision{2, 1}, "same", 10, now.Add(time.Second))
	if err != nil || reviewOnly.BudgetGranted || reviewOnly.Reservation.Reason != "deduplicated" {
		t.Fatalf("review-only=%+v err=%v", reviewOnly, err)
	}
	var latestRevision, materialRevision int64
	var status string
	if err := db.Pool.QueryRow(ctx, `SELECT latest_run_revision,material_run_revision FROM catalog_metadata_heads WHERE steam_app_id=965`).Scan(&latestRevision, &materialRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_metadata_ingress_work WHERE steam_app_id=965 AND ingress_identity='library'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if latestRevision != 2 || materialRevision != 1 || status != "reserved" {
		t.Fatalf("latest=%d material=%d status=%q", latestRevision, materialRevision, status)
	}
	if err := repo.ReleaseMetadata(ctx, first.Reservation, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	retry, err := repo.ClaimIngressMetadataWork(ctx, 10, now.Add(3*time.Second))
	if err != nil || !retry.BudgetGranted || retry.Reservation.Revision != (CandidateRevision{1, 1}) {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	var used int
	if err := db.Pool.QueryRow(ctx, `SELECT used FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("used=%d err=%v", used, err)
	}
	stale, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "manual", Identity: "stale"}, 965, CandidateRevision{1, 2}, "older-different", 10, now.Add(4*time.Second))
	if err != nil || stale.Reservation.Reason != "stale" {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
	var staleWork int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_metadata_ingress_work WHERE steam_app_id=965 AND ingress_identity='stale'`).Scan(&staleWork); err != nil || staleWork != 0 {
		t.Fatalf("stale work=%d err=%v", staleWork, err)
	}
}

func TestCrossSourceMaterialPreemptionTerminalizesStaleIngress(t *testing.T) {
	for _, terminal := range []string{"complete", "release"} {
		t.Run(terminal, func(t *testing.T) {
			db := openCatalogTestStore(t)
			repo := NewSearchRepository(db)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			insertReservationCandidate(t, repo, 970, CandidateRevision{1, 1}, "old", now.Add(-time.Minute))
			old, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "user_sync", Identity: "old-source"}, 970, CandidateRevision{1, 1}, "old", 10, now)
			if err != nil || !old.BudgetGranted {
				t.Fatalf("old=%+v err=%v", old, err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_current SET run_revision=2,observation_ordinal=1,fingerprint='new',material_run_revision=2,material_observation_ordinal=1,material_fingerprint='new' WHERE steam_app_id=970`); err != nil {
				t.Fatal(err)
			}
			current, err := repo.ReserveIngressMetadata(ctx, MetadataIngress{Kind: "manual", Identity: "new-source"}, 970, CandidateRevision{2, 1}, "new", 10, now.Add(time.Second))
			if err != nil || !current.BudgetGranted {
				t.Fatalf("current=%+v err=%v", current, err)
			}
			var oldStatus string
			if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_metadata_ingress_work WHERE steam_app_id=970 AND ingress_identity='old-source'`).Scan(&oldStatus); err != nil || oldStatus != "superseded" {
				t.Fatalf("old status=%q err=%v", oldStatus, err)
			}
			if terminal == "release" {
				if err := repo.ReleaseMetadata(ctx, current.Reservation, now.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.Pool.Exec(ctx, `UPDATE catalog_store_reservations SET status='complete' WHERE steam_app_id=970`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Pool.Exec(ctx, `UPDATE catalog_metadata_ingress_work SET status='complete' WHERE steam_app_id=970 AND ingress_identity='new-source'`); err != nil {
					t.Fatal(err)
				}
			}
			var staleReserved int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_metadata_ingress_work WHERE steam_app_id=970 AND ingress_identity='old-source' AND status='reserved'`).Scan(&staleReserved); err != nil || staleReserved != 0 {
				t.Fatalf("stale reserved=%d err=%v", staleReserved, err)
			}
		})
	}
}

func waitForMetadataAdvisoryWaiters(t *testing.T, ctx context.Context, db *store.Store, appID int64, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiters int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype='advisory' AND classid=0 AND objid::bigint=$1 AND objsubid=1 AND NOT granted`, appID).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d advisory waiters on app %d: %v", want, appID, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("advisory lock for app %d never reached %d waiters", appID, want)
}

func TestMetadataAdvisoryLockSerializesPageClaimAndRelease(t *testing.T) {
	t.Run("page versus claim", func(t *testing.T) {
		cfg := DefaultDiscoveryConfig()
		service, repo := newSearchScanForTest(t, &searchStub{}, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		run := searchTestRun(t, repo, cfg, now)
		first := testObservation(980, "20 Oct, 2026", steam.ReviewPositive, "Positive")
		if err := service.CommitPage(ctx, run, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 2, HasMore: true, Items: []steam.SearchObservation{first}}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO catalog_metadata_ingress_work
			(steam_app_id,ingress_kind,ingress_identity,run_revision,observation_ordinal,fingerprint,
			 material_run_revision,material_observation_ordinal,material_fingerprint,status)
			VALUES(980,'user_sync','page-lock-ingress',$1,1,$2,$1,1,$2,'pending')`, run.Revision, first.Fingerprint); err != nil {
			t.Fatal(err)
		}
		blocker, err := repo.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(980)); err != nil {
			t.Fatal(err)
		}
		hook := make(chan struct{})
		service.beforePagePersistHook = func() error { close(hook); return nil }
		pageErr := make(chan error, 1)
		second := first
		second.Fingerprint = "material-new"
		go func() {
			pageErr <- service.CommitPage(ctx, run, steam.FeedPopularNew, 1, 0, steam.SearchPage{Start: 1, Count: 1, Total: 2, Items: []steam.SearchObservation{second}})
		}()
		<-hook
		waitForMetadataAdvisoryWaiters(t, ctx, repo.store, 980, 1)
		type claimOutcome struct {
			item MetadataWorkItem
			err  error
		}
		claimDone := make(chan claimOutcome, 1)
		go func() {
			item, err := repo.ClaimMetadataWork(ctx, 10, now)
			claimDone <- claimOutcome{item: item, err: err}
		}()
		waitForMetadataAdvisoryWaiters(t, ctx, repo.store, 980, 2)
		select {
		case err := <-pageErr:
			t.Fatalf("page completed before advisory unlock: %v", err)
		default:
		}
		select {
		case outcome := <-claimDone:
			t.Fatalf("claim completed before advisory unlock: %+v", outcome)
		default:
		}
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-pageErr; err != nil {
			t.Fatal(err)
		}
		claimed := <-claimDone
		if claimed.err != nil || !claimed.item.BudgetGranted {
			t.Fatalf("claim=%+v err=%v", claimed.item, claimed.err)
		}
		var headRevision, headOrdinal, currentRevision, currentOrdinal, currentMaterialRevision, currentMaterialOrdinal, workRevision, workOrdinal, workMaterialRevision, workMaterialOrdinal, reservationRevision, reservationOrdinal int64
		var headFingerprint, workStatus, ingressStatus, reservationStatus, reservationFingerprint string
		var currentFingerprint, currentMaterialFingerprint string
		var reservationID, reservationToken uuid.UUID
		if err := repo.store.Pool.QueryRow(ctx, `SELECT material_run_revision,material_observation_ordinal,material_fingerprint FROM catalog_metadata_heads WHERE steam_app_id=980`).Scan(&headRevision, &headOrdinal, &headFingerprint); err != nil {
			t.Fatal(err)
		}
		if err := repo.store.Pool.QueryRow(ctx, `SELECT run_revision,observation_ordinal,material_run_revision,material_observation_ordinal,status FROM catalog_search_worklist WHERE run_id=$1 AND steam_app_id=980`, run.ID).Scan(&workRevision, &workOrdinal, &workMaterialRevision, &workMaterialOrdinal, &workStatus); err != nil {
			t.Fatal(err)
		}
		if err := repo.store.Pool.QueryRow(ctx, `SELECT run_revision,observation_ordinal,fingerprint,material_run_revision,material_observation_ordinal,material_fingerprint FROM catalog_search_current WHERE steam_app_id=980`).Scan(&currentRevision, &currentOrdinal, &currentFingerprint, &currentMaterialRevision, &currentMaterialOrdinal, &currentMaterialFingerprint); err != nil {
			t.Fatal(err)
		}
		if err := repo.store.Pool.QueryRow(ctx, `SELECT status FROM catalog_metadata_ingress_work WHERE steam_app_id=980 AND ingress_identity='page-lock-ingress'`).Scan(&ingressStatus); err != nil {
			t.Fatal(err)
		}
		if err := repo.store.Pool.QueryRow(ctx, `SELECT id,run_revision,observation_ordinal,fingerprint,status,fencing_token FROM catalog_store_reservations WHERE steam_app_id=980`).Scan(&reservationID, &reservationRevision, &reservationOrdinal, &reservationFingerprint, &reservationStatus, &reservationToken); err != nil {
			t.Fatal(err)
		}
		if headRevision != run.Revision || headOrdinal != 2 || headFingerprint != "material-new" || workStatus != "reserved" || ingressStatus != "superseded" ||
			workRevision != run.Revision || workOrdinal != 2 || workMaterialRevision != headRevision || workMaterialOrdinal != headOrdinal || currentRevision != headRevision || currentOrdinal != headOrdinal || currentFingerprint != headFingerprint ||
			currentMaterialRevision != headRevision || currentMaterialOrdinal != headOrdinal || currentMaterialFingerprint != headFingerprint ||
			reservationRevision != headRevision || reservationOrdinal != headOrdinal || reservationFingerprint != headFingerprint || reservationStatus != "reserved" || reservationID != claimed.item.Reservation.ID || reservationToken != claimed.item.Reservation.FencingToken {
			t.Fatalf("head=%d/%d/%q work=%q ingress=%q reservation=%d/%d/%q/%q/%s claim=%+v", headRevision, headOrdinal, headFingerprint, workStatus, ingressStatus, reservationRevision, reservationOrdinal, reservationFingerprint, reservationStatus, reservationToken, claimed.item)
		}
	})

	t.Run("claim versus release", func(t *testing.T) {
		db := openCatalogTestStore(t)
		repo := NewSearchRepository(db)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		now := time.Now().UTC().Truncate(time.Second)
		_, rev := insertMetadataRun(t, db, "daily", "scheduled", now, 981, "claim-release", "pending", nil)
		insertReservationCandidate(t, repo, 981, rev, "claim-release", now.Add(-time.Minute))
		first, err := repo.ClaimMetadataWork(ctx, 10, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE catalog_store_reservations SET lease_until=$2 WHERE steam_app_id=$1`, 981, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_metadata_ingress_work
			(steam_app_id,ingress_kind,ingress_identity,run_revision,observation_ordinal,fingerprint,
			 material_run_revision,material_observation_ordinal,material_fingerprint,status)
			VALUES(981,'user_sync','release-lock-ingress',$1,$2,'claim-release',$1,$2,'claim-release','reserved')`, rev.RunRevision, rev.ObservationOrdinal); err != nil {
			t.Fatal(err)
		}
		blocker, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(981)); err != nil {
			t.Fatal(err)
		}
		releaseErr := make(chan error, 1)
		go func() { releaseErr <- repo.ReleaseMetadata(ctx, first.Reservation, now.Add(time.Minute)) }()
		waitForMetadataAdvisoryWaiters(t, ctx, db, 981, 1)
		type claimOutcome struct {
			item MetadataWorkItem
			err  error
		}
		claimDone := make(chan claimOutcome, 1)
		go func() {
			item, err := repo.ClaimMetadataWork(ctx, 10, now.Add(3*time.Minute))
			claimDone <- claimOutcome{item: item, err: err}
		}()
		waitForMetadataAdvisoryWaiters(t, ctx, db, 981, 2)
		select {
		case err := <-releaseErr:
			t.Fatalf("release completed before advisory unlock: %v", err)
		default:
		}
		select {
		case outcome := <-claimDone:
			t.Fatalf("claim completed before advisory unlock: %+v", outcome)
		default:
		}
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-releaseErr; err != nil {
			t.Fatal(err)
		}
		claimed := <-claimDone
		second := claimed.item
		if claimed.err != nil || !second.BudgetGranted || second.Reservation.FencingToken == first.Reservation.FencingToken {
			t.Fatalf("second=%+v err=%v", second, claimed.err)
		}
		var headRevision, headOrdinal, currentRevision, currentOrdinal, currentMaterialRevision, currentMaterialOrdinal, workMaterialRevision, workMaterialOrdinal, reservationRevision, reservationOrdinal int64
		var headFingerprint, workStatus, ingressStatus, reservationStatus, reservationFingerprint string
		var currentFingerprint, currentMaterialFingerprint string
		var reservationID, reservationToken uuid.UUID
		if err := db.Pool.QueryRow(ctx, `SELECT material_run_revision,material_observation_ordinal,material_fingerprint FROM catalog_metadata_heads WHERE steam_app_id=981`).Scan(&headRevision, &headOrdinal, &headFingerprint); err != nil {
			t.Fatal(err)
		}
		if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(material_run_revision,run_revision),COALESCE(material_observation_ordinal,observation_ordinal),status FROM catalog_search_worklist WHERE steam_app_id=981`).Scan(&workMaterialRevision, &workMaterialOrdinal, &workStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.Pool.QueryRow(ctx, `SELECT run_revision,observation_ordinal,fingerprint,COALESCE(material_run_revision,run_revision),COALESCE(material_observation_ordinal,observation_ordinal),COALESCE(material_fingerprint,fingerprint) FROM catalog_search_current WHERE steam_app_id=981`).Scan(&currentRevision, &currentOrdinal, &currentFingerprint, &currentMaterialRevision, &currentMaterialOrdinal, &currentMaterialFingerprint); err != nil {
			t.Fatal(err)
		}
		if err := db.Pool.QueryRow(ctx, `SELECT status FROM catalog_metadata_ingress_work WHERE steam_app_id=981 AND ingress_identity='release-lock-ingress'`).Scan(&ingressStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.Pool.QueryRow(ctx, `SELECT id,run_revision,observation_ordinal,fingerprint,status,fencing_token FROM catalog_store_reservations WHERE steam_app_id=981`).Scan(&reservationID, &reservationRevision, &reservationOrdinal, &reservationFingerprint, &reservationStatus, &reservationToken); err != nil {
			t.Fatal(err)
		}
		if headRevision != rev.RunRevision || headOrdinal != rev.ObservationOrdinal || headFingerprint != "claim-release" || workStatus != "reserved" || ingressStatus != "pending" ||
			workMaterialRevision != headRevision || workMaterialOrdinal != headOrdinal || currentRevision != headRevision || currentOrdinal != headOrdinal || currentFingerprint != headFingerprint ||
			currentMaterialRevision != headRevision || currentMaterialOrdinal != headOrdinal || currentMaterialFingerprint != headFingerprint ||
			reservationRevision != headRevision || reservationOrdinal != headOrdinal || reservationFingerprint != headFingerprint || reservationStatus != "reserved" || reservationID != second.Reservation.ID || reservationToken != second.Reservation.FencingToken {
			t.Fatalf("head=%d/%d/%q work=%q ingress=%q reservation=%d/%d/%q/%q/%s second=%+v", headRevision, headOrdinal, headFingerprint, workStatus, ingressStatus, reservationRevision, reservationOrdinal, reservationFingerprint, reservationStatus, reservationToken, second)
		}
	})
}
