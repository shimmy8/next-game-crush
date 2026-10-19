package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLegacyDiscoveryPresentationReadsSearchState(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	runID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(id,schedule_name,occurrence_at,kind,status)
		VALUES($1,'daily',$2,'scheduled','queued')`, runID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,active)
		VALUES(620,1,1,true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_metadata_ingress_work(steam_app_id,ingress_kind,ingress_identity,
		run_revision,observation_ordinal,material_run_revision,material_observation_ordinal,status)
		VALUES(620,'scheduled',$1,1,1,1,1,'pending')`, runID.String()+":620"); err != nil {
		t.Fatal(err)
	}
	repo := NewDiscoveryRepository(db)
	status, err := repo.SearchPresentationStatus(ctx, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "updating" || status.ActiveDiscoveredGames != 1 {
		t.Fatalf("status=%+v", status)
	}
	diagnostics, err := repo.SearchDiscoveryDiagnostics(ctx, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.PendingQualifications != 1 || diagnostics.Reason != "backfill_in_progress" {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
}
