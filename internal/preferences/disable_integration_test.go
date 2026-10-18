package preferences

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
)

func TestDisableAdjustmentAtomicallyEnqueuesOnce(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, event := preferenceFixture(t, db)
	id := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)VALUES($1,$2,$3,$4,'mode','co-op','avoid',1,1)`, id, user, event, game); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published)SELECT $1,$2,id,7,'fixture','fixture',true FROM preference_profiles WHERE user_id=$2`, uuid.New(), user); err != nil {
		t.Fatal(err)
	}
	repo, _ := NewSQLRepository(db)
	var wg sync.WaitGroup
	values := make(chan DisableAccepted, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); v, err := repo.DisableAndEnqueue(ctx, user, id); values <- v; errs <- err }()
	}
	wg.Wait()
	close(values)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var job uuid.UUID
	fresh := 0
	for v := range values {
		if !v.AlreadyInactive {
			fresh++
		}
		if job == uuid.Nil {
			job = v.RebuildJobID
		}
		if v.RebuildJobID != job || v.PreviousRankingVersion != 7 {
			t.Fatalf("accept=%#v", v)
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh=%d", fresh)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='preferences.disable' AND user_id=$1`, user).Scan(&count); err != nil || count != 1 {
		t.Fatalf("jobs=%d err=%v", count, err)
	}
	if _, err := repo.DisableAndEnqueue(ctx, uuid.New(), id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign=%v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE recommendation_runs SET ranking_version=9 WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	repeat, err := repo.DisableAndEnqueue(ctx, user, id)
	if err != nil || repeat.RebuildJobID != job || repeat.PreviousRankingVersion != 7 || !repeat.AlreadyInactive {
		t.Fatalf("repeat=%#v err=%v", repeat, err)
	}
}

type terminalDisableFixtureError struct{}

func (terminalDisableFixtureError) Error() string   { return "fixture ranking failure" }
func (terminalDisableFixtureError) Retryable() bool { return false }

func TestDisableFailedRankingKeepsPublishedVersionAndAcceptedWorkflow(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, event := preferenceFixture(t, db)
	id := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)VALUES($1,$2,$3,$4,'mode','co-op','avoid',1,1)`, id, user, event, game); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published)SELECT $1,$2,id,7,'fixture','fixture',true FROM preference_profiles WHERE user_id=$2`, uuid.New(), user); err != nil {
		t.Fatal(err)
	}
	repo, _ := NewSQLRepository(db)
	queue := jobs.NewQueue(db)
	service, _ := NewService(repo, queue)
	accepted, err := service.Disable(ctx, user, id)
	if err != nil {
		t.Fatal(err)
	}
	worker := jobs.NewWorker(queue, "disable-failure", jobs.WithPollInterval(time.Millisecond*5))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("profile.rebuild", func(context.Context, jobs.Lease) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("recommendations.rebuild", func(context.Context, jobs.Lease) error { return terminalDisableFixtureError{} }); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, accepted.RebuildJobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "dead" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow status=%s", status)
		}
		time.Sleep(time.Millisecond * 10)
	}
	var version int64
	var active bool
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT max(ranking_version) FROM recommendation_runs WHERE user_id=$1 AND published),active FROM preference_adjustments WHERE id=$2`, user, id).Scan(&version, &active); err != nil || version != 7 || active {
		t.Fatalf("preserved=%d active=%v err=%v", version, active, err)
	}
	repeat, err := service.Disable(ctx, user, id)
	if err != nil || repeat.RebuildJobID != accepted.RebuildJobID || repeat.PreviousRankingVersion != 7 || !repeat.AlreadyInactive {
		t.Fatalf("repeat=%#v err=%v", repeat, err)
	}
}

func TestDisableAdjustmentDeletionRaceCannotRecreateJobs(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, event := preferenceFixture(t, db)
	id := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)VALUES($1,$2,$3,$4,'mode','co-op','avoid',1,1)`, id, user, event, game); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, user); err != nil {
		t.Fatal(err)
	}
	repo, _ := NewSQLRepository(db)
	finished := make(chan error, 1)
	go func() { _, err := repo.DisableAndEnqueue(ctx, user, id); finished <- err }()
	if _, err := tx.Exec(ctx, `INSERT INTO deleted_user_tombstones(user_id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disable after deletion=%v", err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE user_id=$1`, user).Scan(&count); err != nil || count != 0 {
		t.Fatalf("recreated jobs=%d err=%v", count, err)
	}
}

func TestDisableAdjustmentRollsBackQueueFailureAndFencesDeletedUser(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, event := preferenceFixture(t, db)
	id := uuid.New()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)VALUES($1,$2,$3,$4,'mode','co-op','avoid',1,1)`, []any{id, user, event, game}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published)SELECT $1,$2,id,7,'fixture','fixture',true FROM preference_profiles WHERE user_id=$2`, []any{uuid.New(), user}},
		{`CREATE FUNCTION reject_disable_job() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.kind='preferences.disable' THEN RAISE EXCEPTION 'fixture queue failure';END IF;RETURN NEW;END$$`, nil},
		{`CREATE TRIGGER reject_disable_job BEFORE INSERT ON sync_jobs FOR EACH ROW EXECUTE FUNCTION reject_disable_job()`, nil},
	} {
		if _, err := db.Pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	repo, _ := NewSQLRepository(db)
	if _, err := repo.DisableAndEnqueue(ctx, user, id); err == nil {
		t.Fatal("queue failure accepted")
	}
	var active bool
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT active,(SELECT count(*) FROM sync_jobs) FROM preference_adjustments WHERE id=$1`, id).Scan(&active, &count); err != nil || !active || count != 0 {
		t.Fatalf("rollback=%v/%d err=%v", active, count, err)
	}
	if _, err := db.Pool.Exec(ctx, `DROP TRIGGER reject_disable_job ON sync_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO deleted_user_tombstones(user_id)VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DisableAndEnqueue(ctx, user, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted user=%v", err)
	}
}
