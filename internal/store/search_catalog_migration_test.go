package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pressly/goose/v3"
	migrationfiles "next-game-crush/db/migrations"
	dbgen "next-game-crush/internal/store/sqlc"
)

func searchMigrationDatabase(t *testing.T) (context.Context, *sql.DB, *goose.Provider) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path != "/next_game_crush_test" {
		t.Fatal("requires isolated next_game_crush_test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "search_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close(context.Background())
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrationfiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err = p.UpTo(ctx, 16); err != nil {
		t.Fatal(err)
	}
	return ctx, db, p
}

func migrationExec(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func publishSearchMembershipForTest(ctx context.Context, s *Store, args dbgen.PublishFeedMembershipParams) (bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	queries := s.Queries.WithTx(tx)
	if _, err := queries.LockSearchFeedPublication(ctx, args.Feed); err != nil {
		return false, err
	}
	published, err := queries.PublishFeedMembership(ctx, args)
	if err != nil {
		return false, err
	}
	return published, tx.Commit(ctx)
}

func TestSteamSearchMigrationPreservesVersion16State(t *testing.T) {
	ctx, db, p := searchMigrationDatabase(t)
	for _, stmt := range []string{
		`INSERT INTO users(id) VALUES('00000000-0000-4000-8000-000000000001')`,
		`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES('00000000-0000-4000-8000-000000000002',42,'Seed','steam',now())`,
		`INSERT INTO catalog_entries(game_id,origin) SELECT id,'seed' FROM games`,
		`INSERT INTO catalog_entry_sources(game_id,source) SELECT id,'seed' FROM games`,
		`INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status) VALUES('steam',42,now(),'admitted')`,
		`INSERT INTO preference_profiles(id,user_id,version) VALUES('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000001',1)`,
		`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES(gen_random_uuid(),'00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000003',7,'v1','existing',true)`,
		`INSERT INTO recommendations(id,user_id,game_id,profile_id,ranking_version,algorithm_version,score) VALUES(gen_random_uuid(),'00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000003',7,'v1',10)`,
		`INSERT INTO catalog_rebuild_state(user_id,dirty_revision,enqueued_revision) VALUES('00000000-0000-4000-8000-000000000001',3,2)`,
		`INSERT INTO sync_jobs(id,kind,idempotency_key,payload,result) VALUES(gen_random_uuid(),'catalog.refresh','interactive','{"version":1}','{"saved":true}')`,
	} {
		migrationExec(t, ctx, db, stmt)
	}
	if _, err := p.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	var good bool
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT count(*)=1 FROM games) AND (SELECT count(*)=1 FROM catalog_entry_sources) AND
		(SELECT count(*)=1 FROM catalog_discovery_observations) AND (SELECT count(*)=1 FROM recommendations WHERE ranking_version=7) AND
		(SELECT count(*)=1 FROM recommendation_runs WHERE ranking_version=7 AND published) AND
		(SELECT count(*)=1 FROM catalog_rebuild_state WHERE dirty_revision=3 AND enqueued_revision=2) AND
		(SELECT count(*)=1 FROM sync_jobs WHERE lane='interactive' AND status='queued') AND
		(SELECT legacy_enqueue_enabled AND legacy_cutover_at IS NULL FROM catalog_workflow_state WHERE singleton)`).Scan(&good)
	if err != nil || !good {
		t.Fatalf("version 16 state not preserved: good=%v err=%v", good, err)
	}
	var first, second int64
	if err := db.QueryRowContext(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind) VALUES('daily',now(),'scheduled') RETURNING run_revision`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind) VALUES('manual',now(),'manual') RETURNING run_revision`).Scan(&second); err != nil || second <= first {
		t.Fatalf("revision allocation not monotonic: %d %d %v", first, second, err)
	}
	for _, stmt := range []string{
		`INSERT INTO catalog_workflow_state(singleton) VALUES(false)`,
		`INSERT INTO catalog_workflow_state(singleton) VALUES(true)`,
		`UPDATE sync_jobs SET lane='other'`,
		`INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind) SELECT schedule_name,occurrence_at,kind FROM catalog_search_runs LIMIT 1`,
		`UPDATE catalog_search_runs SET status='other'`,
		`INSERT INTO catalog_search_feeds(run_id,feed) SELECT id,'other' FROM catalog_search_runs LIMIT 1`,
		`INSERT INTO catalog_search_feeds(run_id,feed,status) SELECT id,'recent','other' FROM catalog_search_runs LIMIT 1`,
		`INSERT INTO catalog_search_feeds(run_id,feed,status,boundary_stop) SELECT id,'popular_new','complete',true FROM catalog_search_runs LIMIT 1`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal) VALUES(1,1,0)`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,review_band) VALUES(1,1,1,'other')`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,positive_percent) VALUES(1,1,1,101)`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,review_total) VALUES(1,1,1,-1)`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,tombstone_reason) VALUES(1,1,1,'private text')`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,review_label) VALUES(1,1,1,repeat('x',65))`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,release_text) VALUES(1,1,1,repeat('x',513))`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,fingerprint) VALUES(1,1,1,repeat('x',129))`,
		`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,precision,release_date) VALUES(1,1,1,'month',current_date)`,
		`INSERT INTO catalog_budget_reservations(kind,hour_at) VALUES('other',date_trunc('hour',now()))`,
		`INSERT INTO catalog_store_reservations(steam_app_id,run_revision,observation_ordinal,lease_until,status) VALUES(42,1,1,now(),'other')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("accepted invalid search state: %s", stmt)
		}
	}
	migrationExec(t, ctx, db, `INSERT INTO catalog_search_feeds(run_id,feed) SELECT id,'recent' FROM catalog_search_runs WHERE run_revision=$1`, first)
	migrationExec(t, ctx, db, `INSERT INTO catalog_search_pages(run_id,feed,start_offset,page_generation,observation_ordinal) SELECT id,'recent',0,0,1 FROM catalog_search_runs WHERE run_revision=$1`, first)
	migrationExec(t, ctx, db, `INSERT INTO catalog_search_observations(run_id,feed,start_offset,page_generation,steam_app_id,run_revision,observation_ordinal) SELECT id,'recent',0,0,42,run_revision,1 FROM catalog_search_runs WHERE run_revision=$1`, first)
	migrationExec(t, ctx, db, `INSERT INTO catalog_search_worklist(run_id,steam_app_id,run_revision,observation_ordinal) SELECT id,42,run_revision,1 FROM catalog_search_runs WHERE run_revision=$1`, first)
	migrationExec(t, ctx, db, `INSERT INTO catalog_store_reservations(steam_app_id,run_revision,observation_ordinal,lease_until) VALUES(42,1,1,now()+interval '1 minute')`)
	for _, stmt := range []string{
		`INSERT INTO catalog_search_pages SELECT * FROM catalog_search_pages`,
		`INSERT INTO catalog_search_worklist SELECT * FROM catalog_search_worklist`,
		`INSERT INTO catalog_store_reservations SELECT * FROM catalog_store_reservations`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Fatal("duplicate durable identity accepted")
		}
	}
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "disable discovery instead") {
		t.Fatalf("populated rollback not refused: %v", err)
	}
}

func TestSearchObservationQueryFencesAndPreservesSuccessfulReview(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	args := dbgen.UpsertCurrentSearchObservationParams{
		SteamAppID: 42, RunRevision: 2, ObservationOrdinal: 1, Precision: "unknown",
		ReviewBand: "unknown", ReviewObservedAt: timestamptz(time.Now()), Priority: 50,
	}
	if n, err := s.Queries.UpsertCurrentSearchObservation(ctx, args); err != nil || n != 1 {
		t.Fatalf("insert: %d %v", n, err)
	}
	var observed *time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT review_observed_at FROM catalog_search_current WHERE steam_app_id=42`).Scan(&observed); err != nil || observed != nil {
		t.Fatalf("unknown review cannot have successful observation time: %v %v", observed, err)
	}
	args.ObservationOrdinal = 2
	args.ReviewBand = "positive"
	args.ReviewLabel = "Positive"
	args.ReviewTotal = 200
	args.PositivePercent = pgtype.Int4{Int32: 90, Valid: true}
	if n, err := s.Queries.UpsertCurrentSearchObservation(ctx, args); err != nil || n != 1 {
		t.Fatalf("review update: %d %v", n, err)
	}
	args.RunRevision = 1
	args.ObservationOrdinal = 100
	args.ReviewBand = "mixed_or_below"
	if n, err := s.Queries.UpsertCurrentSearchObservation(ctx, args); err != nil || n != 0 {
		t.Fatalf("stale review update: %d %v", n, err)
	}
	args.RunRevision = 2
	args.ObservationOrdinal = 3
	args.ReviewBand = "unknown"
	args.ReviewLabel = ""
	args.ReviewTotal = 0
	args.PositivePercent = pgtype.Int4{}
	if n, err := s.Queries.UpsertCurrentSearchObservation(ctx, args); err != nil || n != 1 {
		t.Fatalf("unknown update: %d %v", n, err)
	}
	var good bool
	if err := s.Pool.QueryRow(ctx, `SELECT review_band='positive' AND review_label='Positive' AND positive_percent=90 AND review_total=200 AND review_observed_at IS NOT NULL AND observation_ordinal=3 FROM catalog_search_current WHERE steam_app_id=42`).Scan(&good); err != nil || !good {
		t.Fatalf("last successful review was lost: %v %v", good, err)
	}
}

func TestSearchObservationPrecisionTransitionsClearPriorExactDate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i, precision := range []string{"day", "month", "quarter", "year", "coming_soon", "unknown"} {
		t.Run(precision, func(t *testing.T) {
			appID := int64(100 + i)
			if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,precision,release_text,release_date,release_year,release_month) VALUES($1,1,1,'day','Oct 18, 2026','2026-10-18',2026,10)`, appID); err != nil {
				t.Fatal(err)
			}
			args := dbgen.UpsertCurrentSearchObservationParams{SteamAppID: appID, RunRevision: 2, ObservationOrdinal: 1, Precision: precision, ReleaseText: "new " + precision, ReviewBand: "unknown", ReviewObservedAt: timestamptz(time.Now()), Priority: 50}
			// Supply all fields to prove the statement retains only fields that
			// belong to the new precision, even if a caller carries old values.
			args.ReleaseDate = pgtype.Date{Time: time.Date(2027, 11, 20, 0, 0, 0, 0, time.UTC), Valid: true}
			args.ReleaseYear = pgtype.Int4{Int32: 2027, Valid: true}
			args.ReleaseMonth = pgtype.Int4{Int32: 11, Valid: true}
			args.ReleaseQuarter = pgtype.Int4{Int32: 4, Valid: true}
			if n, err := s.Queries.UpsertCurrentSearchObservation(ctx, args); err != nil || n != 1 {
				t.Fatalf("day -> %s failed: rows=%d err=%v", precision, n, err)
			}
			var date *time.Time
			var year, month, quarter *int32
			var raw string
			if err := s.Pool.QueryRow(ctx, `SELECT release_date,release_year,release_month,release_quarter,release_text FROM catalog_search_current WHERE steam_app_id=$1`, appID).Scan(&date, &year, &month, &quarter, &raw); err != nil {
				t.Fatal(err)
			}
			if (date != nil) != (precision == "day") || (year != nil) != (precision == "day" || precision == "month" || precision == "quarter" || precision == "year") || (month != nil) != (precision == "day" || precision == "month") || (quarter != nil) != (precision == "quarter") || raw != "new "+precision {
				t.Fatalf("incoherent release fields: date=%v year=%v month=%v quarter=%v raw=%s", date, year, month, quarter, raw)
			}
			if date != nil && !date.Equal(args.ReleaseDate.Time) || year != nil && *year != 2027 || month != nil && *month != 11 || quarter != nil && *quarter != 4 {
				t.Fatal("transition retained prior release values")
			}
		})
	}
}

func TestOverlappingFeedPublicationsObserveCommittedMembership(t *testing.T) {
	for _, feed := range []string{"popular_new", "recent"} {
		t.Run(feed, func(t *testing.T) {
			s := openTestStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			now := time.Now().UTC()
			runs := make([]dbgen.CatalogSearchRun, 2)
			for i := range runs {
				var err error
				runs[i], err = s.Queries.CreateOrGetSearchRun(ctx, dbgen.CreateOrGetSearchRunParams{ScheduleName: "daily", OccurrenceAt: timestamptz(now.Add(time.Duration(i) * time.Hour)), Kind: "scheduled", ConfigSnapshot: []byte(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_search_feeds(run_id,feed,status,natural_end) VALUES($1,$2,'complete',true)`, runs[i].ID, feed); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Queries.CommitSearchPage(ctx, dbgen.CommitSearchPageParams{RunID: runs[0].ID, Feed: feed, ObservationOrdinal: 1, ResultCount: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Queries.InsertSearchObservation(ctx, dbgen.InsertSearchObservationParams{RunID: runs[0].ID, Feed: feed, SteamAppID: 42, RunRevision: runs[0].RunRevision, ObservationOrdinal: 1, Precision: "unknown", ReviewBand: "unknown", ObservedAt: timestamptz(now)}); err != nil {
				t.Fatal(err)
			}
			tx1, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer tx1.Rollback(context.Background())
			q1 := s.Queries.WithTx(tx1)
			if _, err := q1.LockSearchFeedPublication(ctx, feed); err != nil {
				t.Fatal(err)
			}
			if published, err := q1.PublishFeedMembership(ctx, dbgen.PublishFeedMembershipParams{RunID: runs[0].ID, Feed: feed, PublishedAt: timestamptz(now)}); err != nil || !published {
				t.Fatalf("first publish: %v %v", published, err)
			}
			tx2, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer tx2.Rollback(context.Background())
			pid := int32(tx2.Conn().PgConn().PID())
			result := make(chan error, 1)
			go func() {
				q2 := s.Queries.WithTx(tx2)
				if _, err := q2.LockSearchFeedPublication(ctx, feed); err != nil {
					result <- err
					return
				}
				published, err := q2.PublishFeedMembership(ctx, dbgen.PublishFeedMembershipParams{RunID: runs[1].ID, Feed: feed, PublishedAt: timestamptz(now)})
				if err == nil && !published {
					err = errors.New("newer publication was skipped")
				}
				if err == nil {
					err = tx2.Commit(ctx)
				}
				result <- err
			}()
			// Wait for PostgreSQL to confirm the second publication is blocked by
			// the first transaction; no timing assumption controls the overlap.
			for {
				var blocked bool
				if err := s.Pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("second publication never blocked")
				case <-time.After(time.Millisecond):
				}
			}
			if err := tx1.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			var revision int64
			var omissions int32
			var active bool
			if err := s.Pool.QueryRow(ctx, `SELECT published_run_revision,omission_count,active FROM catalog_search_memberships WHERE feed=$1 AND steam_app_id=42`, feed).Scan(&revision, &omissions, &active); err != nil {
				t.Fatal(err)
			}
			if revision != runs[1].RunRevision || omissions != 1 || active != (feed == "recent") {
				t.Fatalf("newer omission missed concurrent addition: revision=%d want=%d omissions=%d active=%v", revision, runs[1].RunRevision, omissions, active)
			}
		})
	}
}

func TestCatalogBudgetQuerySerializesUniqueIdentities(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	hour := timestamptz(time.Now().UTC().Truncate(time.Hour))
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Queries.ReserveCatalogBudget(ctx, dbgen.ReserveCatalogBudgetParams{Kind: "appdetails", HourAt: hour, IdentityHash: fmt.Sprintf("identity-%d", i%10), BudgetLimit: 5})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var used, identities int
	if err := s.Pool.QueryRow(ctx, `SELECT used,(SELECT count(*) FROM jsonb_object_keys(reservations)) FROM catalog_budget_reservations WHERE kind='appdetails'`).Scan(&used, &identities); err != nil || used != 5 || identities != 5 {
		t.Fatalf("budget overspent or charged duplicate: used=%d identities=%d err=%v", used, identities, err)
	}
}

func TestSearchCatalogQueriesFencePublicationAndStoreOwnership(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	create := dbgen.CreateOrGetSearchRunParams{ScheduleName: "daily", OccurrenceAt: timestamptz(now), Kind: "scheduled", ConfigSnapshot: []byte(`{}`)}
	first, err := s.Queries.CreateOrGetSearchRun(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Queries.CreateOrGetSearchRun(ctx, create)
	if err != nil || replay.ID != first.ID || replay.RunRevision != first.RunRevision {
		t.Fatalf("occurrence replay changed identity: %v", err)
	}
	ordinal, err := s.Queries.AllocateObservationOrdinal(ctx, first.ID)
	if err != nil || ordinal != 1 {
		t.Fatalf("allocate ordinal: %d %v", ordinal, err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_search_feeds(run_id,feed,status,natural_end) VALUES($1,'recent','complete',true)`, first.ID); err != nil {
		t.Fatal(err)
	}
	page := dbgen.CommitSearchPageParams{RunID: first.ID, Feed: "recent", ObservationOrdinal: ordinal, ResultCount: 1}
	if _, err := s.Queries.CommitSearchPage(ctx, page); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queries.CommitSearchPage(ctx, page); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("page replay was not idempotent: %v", err)
	}
	observation := dbgen.InsertSearchObservationParams{RunID: first.ID, Feed: "recent", SteamAppID: 42, RunRevision: first.RunRevision, ObservationOrdinal: ordinal, Precision: "unknown", ReviewBand: "positive", ObservedAt: timestamptz(now)}
	if _, err := s.Queries.InsertSearchObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	publish := dbgen.PublishFeedMembershipParams{RunID: first.ID, Feed: "recent", PublishedAt: timestamptz(now)}
	if published, err := publishSearchMembershipForTest(ctx, s, publish); err != nil || !published {
		t.Fatalf("initial publication: %v %v", published, err)
	}
	for omission := 1; omission <= 2; omission++ {
		create.OccurrenceAt = timestamptz(now.Add(time.Duration(omission) * time.Hour))
		run, err := s.Queries.CreateOrGetSearchRun(ctx, create)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO catalog_search_feeds(run_id,feed,status,natural_end) VALUES($1,'recent','complete',true)`, run.ID); err != nil {
			t.Fatal(err)
		}
		publish.RunID = run.ID
		if published, err := publishSearchMembershipForTest(ctx, s, publish); err != nil || !published {
			t.Fatalf("omission publication: %v %v", published, err)
		}
		var active bool
		var omissions int
		if err := s.Pool.QueryRow(ctx, `SELECT active,omission_count FROM catalog_search_memberships WHERE feed='recent' AND steam_app_id=42`).Scan(&active, &omissions); err != nil || omissions != omission || active != (omission == 1) {
			t.Fatalf("omission=%d active=%v count=%d err=%v", omission, active, omissions, err)
		}
	}
	publish.RunID = first.ID
	if published, err := publishSearchMembershipForTest(ctx, s, publish); err != nil || published {
		t.Fatalf("stale publication: %v %v", published, err)
	}
	reservation := dbgen.AcquireStoreReservationParams{SteamAppID: 42, ID: uuidToPG(uuid.New()), RunRevision: first.RunRevision, ObservationOrdinal: 1, FencingToken: uuidToPG(uuid.New()), LeaseUntil: timestamptz(now.Add(time.Minute)), NowAt: timestamptz(now)}
	if _, err := s.Queries.AcquireStoreReservation(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	competitor := reservation
	competitor.ID = uuidToPG(uuid.New())
	competitor.FencingToken = uuidToPG(uuid.New())
	if _, err := s.Queries.AcquireStoreReservation(ctx, competitor); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reservation ownership lost: %v", err)
	}
	release := dbgen.ReleaseStoreReservationParams{SteamAppID: 42, ID: reservation.ID, FencingToken: competitor.FencingToken, NowAt: timestamptz(now)}
	if n, err := s.Queries.ReleaseStoreReservation(ctx, release); err != nil || n != 0 {
		t.Fatalf("stale release: %d %v", n, err)
	}
	release.FencingToken = reservation.FencingToken
	if n, err := s.Queries.ReleaseStoreReservation(ctx, release); err != nil || n != 1 {
		t.Fatalf("owner release: %d %v", n, err)
	}
}

func TestSteamSearchMigrationCancelsEveryLegacyDiscoveryState(t *testing.T) {
	ctx, db, p := searchMigrationDatabase(t)
	for _, kind := range []string{"catalog.discovery.tick", "catalog.discover.incremental", "catalog.discover.backfill", "catalog.qualify", "catalog.rebuild_wakeup"} {
		for _, status := range []string{"queued", "running", "waiting", "retryable"} {
			id, token := uuid.New(), uuid.New()
			migrationExec(t, ctx, db, `INSERT INTO sync_jobs(id,kind,idempotency_key,status,payload,result,worker_id,fencing_token,lease_until) VALUES($1,$2,$3,$4,'{"version":1}','{"audit":true}',CASE WHEN $4='running' THEN 'old-worker' END,CASE WHEN $4='running' THEN $5::uuid END,CASE WHEN $4='running' THEN now()+interval '1 hour' END)`, id, kind, kind+":"+status, status, token)
			if status == "running" {
				migrationExec(t, ctx, db, `INSERT INTO job_attempts(id,job_id,attempt,worker_id,fencing_token,status,started_at) VALUES($1,$2,1,'old-worker',$3,'running',now())`, uuid.New(), id, token)
			}
		}
	}
	// A child whose kind is not itself a legacy producer must not resurrect its parent.
	migrationExec(t, ctx, db, `INSERT INTO sync_jobs(id,kind,idempotency_key,parent_job_id) SELECT gen_random_uuid(),'ai.features','legacy-child',id FROM sync_jobs WHERE idempotency_key='catalog.qualify:waiting'`)
	if _, err := p.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	var retired, attempts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sync_jobs WHERE lane='catalog' AND status='dead' AND worker_id IS NULL AND fencing_token IS NULL AND lease_until IS NULL AND last_error='superseded_catalog_workflow'`).Scan(&retired); err != nil || retired != 21 {
		t.Fatalf("retired=%d err=%v", retired, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_attempts WHERE status='dead' AND finished_at IS NOT NULL AND error='superseded_catalog_workflow'`).Scan(&attempts); err != nil || attempts != 5 {
		t.Fatalf("closed attempts=%d err=%v", attempts, err)
	}
	var preserved bool
	if err := db.QueryRowContext(ctx, `SELECT bool_and(payload='{"version":1}'::jsonb AND result='{"audit":true}'::jsonb) FROM sync_jobs WHERE idempotency_key<>'legacy-child'`).Scan(&preserved); err != nil || !preserved {
		t.Fatal("legacy audit data changed")
	}
	var claimable int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sync_jobs WHERE status IN ('queued','retryable') OR (status='running' AND lease_until<=now())`).Scan(&claimable); err != nil || claimable != 0 {
		t.Fatal("legacy jobs remain claimable")
	}
}
