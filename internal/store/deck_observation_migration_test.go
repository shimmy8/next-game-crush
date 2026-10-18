package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestIndependentDeckObservationMigrationTupleAndSafeRollback(t *testing.T) {
	ctx, database, provider := searchMigrationDatabase(t)
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	knownID, deckOnlyID := uuid.New(), uuid.New()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	migrationExec(t, ctx, database, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,1801,'Known platforms','test',$3),($2,1802,'Deck only','test',$3)`, knownID, deckOnlyID, at)
	migrationExec(t, ctx, database, `INSERT INTO game_compatibility(game_id,windows,mac,linux,platforms_last_successful_fetch_at,deck_status,deck_availability,deck_last_attempt_at,deck_last_successful_fetch_at) VALUES($1,true,false,true,$2,'verified','available',$2,$2)`, knownID, at)
	readState := func(id uuid.UUID) string {
		t.Helper()
		var state string
		if err := database.QueryRowContext(ctx, `SELECT to_jsonb(c)::text FROM game_compatibility c WHERE game_id=$1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	knownBefore := readState(knownID)
	if _, err := provider.UpTo(ctx, 18); err != nil {
		t.Fatal(err)
	}
	if readState(knownID) != knownBefore {
		t.Fatal("upgrade changed the existing compatibility observation")
	}
	migrationExec(t, ctx, database, `INSERT INTO game_compatibility(game_id,deck_status,deck_availability,deck_last_attempt_at,deck_last_successful_fetch_at) VALUES($1,'playable','available',$2,$2)`, deckOnlyID, at)
	deckOnlyBefore := readState(deckOnlyID)
	assertPartialTupleRejected := func() {
		t.Helper()
		// Every partial combination must fail; only all-null and fully observed
		// platform tuples are meaningful.
		for mask := 1; mask < 15; mask++ {
			var windows, mac, linux, fetchedAt any
			if mask&1 != 0 {
				windows = true
			}
			if mask&2 != 0 {
				mac = false
			}
			if mask&4 != 0 {
				linux = true
			}
			if mask&8 != 0 {
				fetchedAt = at
			}
			_, err := database.ExecContext(ctx, `UPDATE game_compatibility SET windows=$2,mac=$3,linux=$4,platforms_last_successful_fetch_at=$5 WHERE game_id=$1`, deckOnlyID, windows, mac, linux, fetchedAt)
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "23514" || pgError.ConstraintName != "game_compatibility_platform_observation" {
				t.Fatalf("partial tuple mask=%04b: error=%v, want platform observation check violation", mask, err)
			}
		}
	}
	assertPartialTupleRejected()
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("rollback unexpectedly accepted a Deck-only row")
	} else {
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23502" {
			t.Fatalf("rollback failed for an unexpected reason: %v", err)
		}
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 18 {
		t.Fatalf("refused rollback version=%d err=%v", version, err)
	}
	if readState(deckOnlyID) != deckOnlyBefore || readState(knownID) != knownBefore {
		t.Fatal("refused rollback changed persisted observations")
	}
	// Refusal must roll back the entire ALTER, including removal of the check.
	assertPartialTupleRejected()
	migrationExec(t, ctx, database, `UPDATE game_compatibility SET windows=false,mac=true,linux=false,platforms_last_successful_fetch_at=$2 WHERE game_id=$1`, deckOnlyID, at)
	completeBefore := readState(deckOnlyID)
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("rollback after platform observation: %v", err)
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 17 {
		t.Fatalf("successful rollback version=%d err=%v", version, err)
	}
	var notNullColumns int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='game_compatibility' AND column_name IN ('windows','mac','linux','platforms_last_successful_fetch_at') AND is_nullable='NO'`).Scan(&notNullColumns); err != nil || notNullColumns != 4 {
		t.Fatalf("restored NOT NULL columns=%d err=%v", notNullColumns, err)
	}
	if readState(deckOnlyID) != completeBefore || readState(knownID) != knownBefore {
		t.Fatal("successful rollback changed persisted observations")
	}
}
