package catalog

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
	migrationfiles "next-game-crush/db/migrations"
	"next-game-crush/internal/jobs"
)

var retiredLegacyKinds = []string{
	"catalog.discovery.tick",
	"catalog.discover.incremental",
	"catalog.discover.backfill",
	"catalog.qualify",
	"catalog.rebuild_wakeup",
}

func TestLegacyDiscoveryJobsDrainedBeforeHandlerRetirement(t *testing.T) {
	t.Run("version 16 migration preserves audit bytes while terminalizing every state", func(t *testing.T) {
		ctx, db, migrations := legacyVersion16Database(t)
		payload := `{"version":1,"audit":"payload"}`
		result := `{"audit":"result"}`
		for _, kind := range retiredLegacyKinds {
			for _, status := range []string{"queued", "running", "waiting", "retryable"} {
				id, token := uuid.New(), uuid.New()
				if _, err := db.ExecContext(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,status,payload,result,worker_id,fencing_token,lease_until)
					VALUES($1,$2,$3,$4,$5::jsonb,$6::jsonb,CASE WHEN $4='running' THEN 'previous-worker' END,
					CASE WHEN $4='running' THEN $7::uuid END,CASE WHEN $4='running' THEN now()+interval '1 hour' END)`,
					id, kind, "retirement:migration:"+kind+":"+status, status, payload, result, token); err != nil {
					t.Fatal(err)
				}
				if status == "running" {
					if _, err := db.ExecContext(ctx, `INSERT INTO job_attempts(id,job_id,attempt,worker_id,fencing_token,status,started_at)
						VALUES($1,$2,1,'previous-worker',$3,'running',now())`, uuid.New(), id, token); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if _, err := migrations.UpTo(ctx, 17); err != nil {
			t.Fatal(err)
		}
		var jobsRetired, attemptsRetired int
		var auditPreserved bool
		if err := db.QueryRowContext(ctx, `SELECT count(*),bool_and(payload=$1::jsonb AND result=$2::jsonb)
			FROM sync_jobs WHERE kind=ANY($3::text[]) AND status='dead' AND last_error='superseded_catalog_workflow'`,
			payload, result, retiredLegacyKinds).Scan(&jobsRetired, &auditPreserved); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_attempts WHERE status='dead' AND error='superseded_catalog_workflow' AND finished_at IS NOT NULL`).Scan(&attemptsRetired); err != nil {
			t.Fatal(err)
		}
		if jobsRetired != len(retiredLegacyKinds)*4 || attemptsRetired != len(retiredLegacyKinds) || !auditPreserved {
			t.Fatalf("jobs=%d attempts=%d audit=%v", jobsRetired, attemptsRetired, auditPreserved)
		}
	})

	t.Run("previous binary work drains without successors after startup cutover", func(t *testing.T) {
		store := openCatalogTestStore(t)
		repo := NewDiscoveryRepository(store)
		queue := jobs.NewQueue(store)
		ctx := context.Background()
		payload := []byte(`{"version":1,"audit":"payload"}`)
		result := []byte(`{"audit":"result"}`)

		if _, err := store.Pool.Exec(ctx, `UPDATE catalog_workflow_state SET legacy_enqueue_enabled=true,legacy_cutover_at=NULL WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		tickID, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: "catalog.discovery.tick", IdempotencyKey: "retirement:running:tick", Payload: payload, MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		tick, err := queue.Claim(ctx, jobs.LaneInteractive, "previous-worker-tick", time.Hour)
		if err != nil || tick == nil || tick.ID != tickID {
			t.Fatalf("tick claim=%+v err=%v", tick, err)
		}
		scanID, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: "catalog.discover.incremental", IdempotencyKey: "retirement:running:scan", Payload: payload, MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		scan, err := queue.Claim(ctx, jobs.LaneInteractive, "previous-worker-scan", time.Hour)
		if err != nil || scan == nil || scan.ID != scanID {
			t.Fatalf("scan claim=%+v err=%v", scan, err)
		}
		for index, status := range []string{"queued", "waiting", "retryable"} {
			id, enqueueErr := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: retiredLegacyKinds[index],
				IdempotencyKey: "retirement:previous:" + status, Payload: payload, MaxAttempts: 3})
			if enqueueErr != nil {
				t.Fatal(enqueueErr)
			}
			if _, err = store.Pool.Exec(ctx, `UPDATE sync_jobs SET status=$2,result=$3 WHERE id=$1`, id, status, result); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = store.Pool.Exec(ctx, `UPDATE sync_jobs SET result=$2 WHERE id=$1`, tick.ID, result); err != nil {
			t.Fatal(err)
		}
		if _, err = store.Pool.Exec(ctx, `UPDATE sync_jobs SET result=$2 WHERE id=$1`, scan.ID, result); err != nil {
			t.Fatal(err)
		}

		cutover, err := repo.CutOverLegacyDiscovery(ctx)
		if err != nil || cutover.Cancelled != 3 || cutover.RunningFenced != 2 {
			t.Fatalf("cutover=%+v err=%v", cutover, err)
		}
		if err = queue.Complete(ctx, *tick); err != nil {
			t.Fatal(err)
		}
		outcome, err := queue.Fail(ctx, *scan, errors.New("previous binary failure"), time.Now().UTC())
		if err != nil || outcome != jobs.FailureDead {
			t.Fatalf("scan outcome=%q err=%v", outcome, err)
		}

		cfg := DefaultDiscoveryConfig()
		cfg.Enabled = true
		scheduler, err := NewSearchScheduler(NewSearchRepository(store), queue, cfg, func() time.Time {
			return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = scheduler.EnsureScheduled(ctx); err != nil {
			t.Fatal(err)
		}
		if err = scheduler.Nudge(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = scheduler.EnqueueManual(ctx, time.Date(2026, 10, 19, 12, 1, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.CutOverLegacyDiscovery(ctx); err != nil {
			t.Fatal(err)
		}
		if count, err := repo.CountNonterminalLegacyJobs(ctx); err != nil || count != 0 {
			t.Fatalf("nonterminal=%d err=%v", count, err)
		}
		var successors int
		var auditPreserved bool
		if err = store.Pool.QueryRow(ctx, `SELECT
			count(*) FILTER(WHERE kind=ANY($1::text[]) AND id NOT IN ($2,$3) AND idempotency_key NOT LIKE 'retirement:previous:%'),
			bool_and(payload=$4::jsonb AND result=$5::jsonb)
			FROM sync_jobs WHERE kind=ANY($1::text[])`, retiredLegacyKinds, tick.ID, scan.ID, payload, result).
			Scan(&successors, &auditPreserved); err != nil {
			t.Fatal(err)
		}
		if successors != 0 || !auditPreserved {
			t.Fatalf("successors=%d audit=%v", successors, auditPreserved)
		}
	})
}

func legacyVersion16Database(t *testing.T) (context.Context, *sql.DB, *goose.Provider) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(raw)
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	if err != nil || parsed.Path != "/next_game_crush_test" {
		t.Fatal("requires isolated next_game_crush_test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "legacy_retirement_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close(context.Background())
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	database, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, database, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	if _, err = provider.UpTo(ctx, 16); err != nil {
		t.Fatal(err)
	}
	return ctx, database, provider
}
