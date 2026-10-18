package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCatalogMetadataGatesMigrationUpgradesVersion18(t *testing.T) {
	ctx, db, p := searchMigrationDatabase(t)
	if _, err := p.UpTo(ctx, 18); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	runID := uuid.New()
	reservationID := uuid.New()
	fencingToken := uuid.New()
	var revision int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO catalog_search_runs (id, schedule_name, occurrence_at, kind, status, started_at)
		VALUES ($1, 'manual', $2, 'manual', 'running', $2)
		RETURNING run_revision`, runID, now).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO catalog_search_current (steam_app_id, run_revision, observation_ordinal, fingerprint, release_text, precision, review_band, first_seen_at, last_seen_at)
		VALUES (1901, $1 + 1, 2, 'fp-1901', 'Coming Soon', 'coming_soon', 'positive', $2, $2)`, revision, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO catalog_search_worklist (run_id, steam_app_id, run_revision, observation_ordinal, priority, status, created_at)
		VALUES ($1, 1901, $2, 1, 77, 'reserved', $3)`, runID, revision, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO catalog_budget_reservations (kind, hour_at, reservations, used)
		VALUES ('appdetails', date_trunc('hour', $1::timestamptz), '{"one":true,"two":true,"three":true}', 3)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO catalog_host_throttles(host,concurrency_limit,consecutive_failures,updated_at)
		VALUES($1,2,3,$2)`, "store.steampowered.com", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO catalog_store_reservations (steam_app_id, id, run_revision, observation_ordinal, fingerprint, status, fencing_token, lease_until, created_at, updated_at)
		VALUES (1901, $1, $2, 1, 'fp-1901', 'reserved', $3, $4, $5, $5)`, reservationID, revision, fencingToken, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	// A different-fingerprint current candidate must wake immediately instead
	// of inheriting the old reservation's reserved work state.
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_search_current
		(steam_app_id,run_revision,observation_ordinal,fingerprint,release_text,precision,review_band)
		VALUES(1902,$1+1,1,'new-1902','Coming Soon','coming_soon','positive')`, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_search_worklist(run_id,steam_app_id,run_revision,observation_ordinal,status)
		VALUES($1,1902,$2,1,'reserved')`, runID, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_store_reservations(steam_app_id,run_revision,observation_ordinal,fingerprint,status,fencing_token,lease_until)
		VALUES(1902,$1,1,'old-1902','reserved',gen_random_uuid(),$2)`, revision, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// ABA must not preserve rev1/F1 merely because the latest rev3 is F1 again;
	// the intervening persisted F2 observation proves a material transition.
	var middleID, latestID uuid.UUID
	var middleRevision, latestRevision int64
	if err := db.QueryRowContext(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind) VALUES('manual',$1,'manual') RETURNING id,run_revision`, now.Add(time.Minute)).Scan(&middleID, &middleRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind) VALUES('manual',$1,'manual') RETURNING id,run_revision`, now.Add(2*time.Minute)).Scan(&latestID, &latestRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_search_feeds(run_id,feed) VALUES($1,'popular_new')`, middleID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_search_pages(run_id,feed,start_offset,page_generation,observation_ordinal)
		VALUES($1,'popular_new',0,0,1)`, middleID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_search_observations(run_id,feed,start_offset,page_generation,steam_app_id,run_revision,observation_ordinal,fingerprint)
		VALUES($1,'popular_new',0,0,1903,$2,1,'F2')`, middleID, middleRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,fingerprint,release_text,precision,review_band)
		VALUES(1903,$1,1,'F1','Coming Soon','coming_soon','positive')`, latestRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_search_worklist(run_id,steam_app_id,run_revision,observation_ordinal,status)
		VALUES($1,1903,$2,1,'reserved')`, latestID, latestRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_store_reservations(steam_app_id,run_revision,observation_ordinal,fingerprint,status,fencing_token,lease_until)
		VALUES(1903,$1,1,'F1','reserved',gen_random_uuid(),$2)`, revision, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 19); err != nil {
		t.Fatal(err)
	}
	var slots int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM catalog_host_throttle_slots`).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if slots != 48 {
		t.Fatalf("slots=%d", slots)
	}
	for _, table := range []string{"catalog_host_throttle_slots", "catalog_metadata_provenance", "catalog_metadata_ingress_work", "catalog_metadata_heads"} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s exists=%v err=%v", table, exists, err)
		}
	}
	for _, column := range []string{"budget_hour", "budget_identity_hash"} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='catalog_store_reservations' AND column_name=$1)`, column).Scan(&exists); err != nil || !exists {
			t.Fatalf("column %s exists=%v err=%v", column, exists, err)
		}
	}
	var currentFingerprint, workStatus, reservationStatus, headFingerprint string
	var currentMaterialRevision, currentMaterialOrdinal, workMaterialRevision, workMaterialOrdinal, headLatestRevision, headLatestOrdinal, headMaterialRevision, headMaterialOrdinal int64
	var budgetUsed, throttleLimit, throttleFailures int
	if err := db.QueryRowContext(ctx, `SELECT fingerprint FROM catalog_search_current WHERE steam_app_id=1901`).Scan(&currentFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM catalog_search_worklist WHERE run_id=$1 AND steam_app_id=1901`, runID).Scan(&workStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT material_run_revision,material_observation_ordinal FROM catalog_search_current WHERE steam_app_id=1901`).Scan(&currentMaterialRevision, &currentMaterialOrdinal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT material_run_revision,material_observation_ordinal FROM catalog_search_worklist WHERE run_id=$1 AND steam_app_id=1901`, runID).Scan(&workMaterialRevision, &workMaterialOrdinal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT latest_run_revision,latest_observation_ordinal,material_run_revision,material_observation_ordinal,material_fingerprint FROM catalog_metadata_heads WHERE steam_app_id=1901`).Scan(&headLatestRevision, &headLatestOrdinal, &headMaterialRevision, &headMaterialOrdinal, &headFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT used FROM catalog_budget_reservations WHERE kind='appdetails' AND hour_at=date_trunc('hour', $1::timestamptz)`, now).Scan(&budgetUsed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT concurrency_limit, consecutive_failures FROM catalog_host_throttles WHERE host=$1`, "store.steampowered.com").Scan(&throttleLimit, &throttleFailures); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM catalog_store_reservations WHERE steam_app_id=1901 AND id=$1`, reservationID).Scan(&reservationStatus); err != nil {
		t.Fatal(err)
	}
	if currentFingerprint != "fp-1901" || workStatus != "reserved" || budgetUsed != 3 || throttleLimit != 2 || throttleFailures != 3 || reservationStatus != "reserved" ||
		currentMaterialRevision != revision || currentMaterialOrdinal != 1 || workMaterialRevision != revision || workMaterialOrdinal != 1 ||
		headLatestRevision != revision+1 || headLatestOrdinal != 2 || headMaterialRevision != revision || headMaterialOrdinal != 1 || headFingerprint != "fp-1901" {
		t.Fatalf("preservation current=%q material=%d/%d work=%q/%d/%d head=%d/%d material=%d/%d/%q budget=%d throttle=%d/%d reservation=%q", currentFingerprint, currentMaterialRevision, currentMaterialOrdinal, workStatus, workMaterialRevision, workMaterialOrdinal, headLatestRevision, headLatestOrdinal, headMaterialRevision, headMaterialOrdinal, headFingerprint, budgetUsed, throttleLimit, throttleFailures, reservationStatus)
	}
	var changedStatus, abaStatus string
	var abaMaterial int64
	if err := db.QueryRowContext(ctx, `SELECT status FROM catalog_search_worklist WHERE steam_app_id=1902`).Scan(&changedStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT w.status,h.material_run_revision FROM catalog_search_worklist w JOIN catalog_metadata_heads h USING(steam_app_id) WHERE w.steam_app_id=1903`).Scan(&abaStatus, &abaMaterial); err != nil {
		t.Fatal(err)
	}
	if changedStatus != "pending" || abaStatus != "pending" || abaMaterial != latestRevision {
		t.Fatalf("material migration wake changed=%q aba=%q/%d want_revision=%d", changedStatus, abaStatus, abaMaterial, latestRevision)
	}
}
