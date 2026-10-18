package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSearchDiagnosticsExposeFixedSafeAggregates(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var runID string
	if err := db.Pool.QueryRow(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status,started_at,completed_at,last_successful_at)
		VALUES('daily',$1,'scheduled','complete',$2,$3,$3) RETURNING id::text`, now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	for _, feed := range SearchDiagnosticFeeds {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_feeds(run_id,feed,status,pages,observed_count,natural_end,monotonic,boundary_stop,completed_at)
			VALUES($1,$2,'complete',2,5,true,true,false,$3)`, runID, feed, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current
		(steam_app_id,run_revision,observation_ordinal,release_date,precision,coming_soon,review_band,review_total,active,tombstone_reason,last_successful_appdetails_at,next_eligibility_at)
		VALUES (101,1,1,$1,'day',true,'positive',10,true,'',$2,$3),
		       (102,1,2,$4,'day',false,'mixed_or_below',20,false,'mixed_or_below',$2,NULL),
		       (103,1,3,$5,'day',false,'unrated',0,true,'',$2,NULL),
		       (104,1,4,$6,'day',true,'positive',10,false,'outside_window',NULL,NULL),
		       (105,1,5,NULL,'unknown',NULL,'unknown',0,false,'unavailable',NULL,NULL)`,
		now.Add(10*24*time.Hour), now.Add(-time.Hour), now.Add(-30*time.Minute), now.Add(-2*24*time.Hour), now.Add(-2*24*time.Hour), now.Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current
		(steam_app_id,run_revision,observation_ordinal,precision,release_year,release_month,release_quarter,coming_soon,review_band,active)
		VALUES (106,1,6,'month',2026,11,NULL,true,'unknown',true),
		       (107,1,7,'quarter',2026,NULL,3,false,'positive',true),
		       (108,1,8,'year',2027,NULL,NULL,true,'unknown',true),
		       (109,1,9,'coming_soon',NULL,NULL,NULL,true,'unknown',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_budget_reservations(kind,hour_at,used) VALUES('appdetails',date_trunc('hour',$1::timestamptz),7),('ai',date_trunc('hour',$1::timestamptz),3)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_memberships(feed,steam_app_id,published_run_revision) VALUES('popular_upcoming',101,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_host_throttles(host,concurrency_limit,consecutive_failures,success_count,next_eligible_at)
		VALUES('store.steampowered.com',2,1,9,$1) ON CONFLICT(host) DO UPDATE SET concurrency_limit=2,consecutive_failures=1,success_count=9,next_eligible_at=EXCLUDED.next_eligible_at`, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload,lane,status,available_at)
		VALUES(gen_random_uuid(),'catalog.search.qualify','diagnostics-catalog','{}','catalog','waiting',$1),
		      (gen_random_uuid(),'steam.sync','diagnostics-interactive','{}','interactive','queued',$1)`, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var reconciliationAt = now.Add(-10 * time.Minute)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status,completed_at,last_successful_at)
		VALUES('reconciliation',$1,'reconciliation','complete',$1,$1)`, reconciliationAt); err != nil {
		t.Fatal(err)
	}

	reader := NewSearchDiagnosticsReader(db, SearchDiagnosticsConfig{
		Enabled: true, StoreBudgetPerHour: 1000, AIBudgetPerHour: 100,
		StaleAfter: 48 * time.Hour, Now: func() time.Time { return now },
	})
	got, err := reader.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SearchDiagnosticsReady || got.LastScheduledOccurrenceAt == nil || got.LastCompleteRun == nil {
		t.Fatalf("status/run=%+v", got)
	}
	if got.LastCompleteRun.Kind != "scheduled" || got.FreshAt == nil || !got.FreshAt.Equal(now.Add(-time.Hour)) || got.LastReconciliationAt == nil || !got.LastReconciliationAt.Equal(reconciliationAt) {
		t.Fatalf("bounded/reconciliation freshness mixed: %+v", got)
	}
	if len(got.Feeds) != len(SearchDiagnosticFeeds) || got.Feeds["popular_upcoming"].Pages != 2 || got.Feeds["popular_upcoming"].Active != 1 {
		t.Fatalf("feeds=%+v", got.Feeds)
	}
	if got.Budgets.Store.Used != 7 || got.Budgets.Store.Remaining != 993 || got.Budgets.Store.Deferred != 0 || got.Budgets.AI.Used != 3 || got.Budgets.AI.Remaining != 97 {
		t.Fatalf("budgets=%+v", got.Budgets)
	}
	if got.Lanes.Catalog.Deferred != 1 || got.Lanes.Interactive.Queued != 1 || got.Throttles["store"].Concurrency != 2 {
		t.Fatalf("lanes=%+v throttles=%+v", got.Lanes, got.Throttles)
	}
	if got.Rejections.ReviewRejected != 1 || got.Rejections.OutsideWindow != 1 || got.Rejections.Unavailable != 1 || got.Admitted.Upcoming != 4 || got.Admitted.Released != 2 {
		t.Fatalf("admitted=%+v rejected=%+v", got.Admitted, got.Rejections)
	}
	if got.Cache.MetadataEntries != 3 || got.Cache.AIEntries != 0 || got.Due.Count != 1 || got.Due.NextAt == nil {
		t.Fatalf("cache=%+v due=%+v", got.Cache, got.Due)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"101", "102", "steam_app_id", "payload", "raw", "name", "cacheHits", "aiDeferred"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("diagnostics leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestSearchDiagnosticsStatusMapping(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reader := NewSearchDiagnosticsReader(db, SearchDiagnosticsConfig{Enabled: false, Now: func() time.Time { return now }})
	if got, err := reader.Diagnostics(ctx); err != nil || got.Status != SearchDiagnosticsDisabled {
		t.Fatalf("disabled=%+v err=%v", got, err)
	}
	reader = NewSearchDiagnosticsReader(db, SearchDiagnosticsConfig{Enabled: true, Now: func() time.Time { return now }})
	if got, err := reader.Diagnostics(ctx); err != nil || got.Status != SearchDiagnosticsUnavailable {
		t.Fatalf("empty=%+v err=%v", got, err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status,completed_at,last_successful_at)
		VALUES('reconciliation',$1,'reconciliation','complete',$1,$1)`, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Diagnostics(ctx); err != nil || got.Status != SearchDiagnosticsUnavailable || got.LastCompleteRun != nil || got.FreshAt != nil || got.LastReconciliationAt == nil {
		t.Fatalf("reconciliation incorrectly established readiness=%+v err=%v", got, err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,status,started_at) VALUES('initial',$1,'initial','running',$1)`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Diagnostics(ctx); err != nil || got.Status != SearchDiagnosticsBackfilling {
		t.Fatalf("backfilling=%+v err=%v", got, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE catalog_search_runs SET status='complete',completed_at=$1,last_successful_at=$1 WHERE schedule_name='initial'`, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	reader.cfg.StaleAfter = 48 * time.Hour
	if got, err := reader.Diagnostics(ctx); err != nil || got.Status != SearchDiagnosticsStale {
		t.Fatalf("stale=%+v err=%v", got, err)
	}
}

func TestPublicSearchStatusIsHonestAboutPartialAndFailedRuns(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	completed := now.Add(-time.Hour)
	last := &SearchRunDiagnostic{Status: "complete", CompletedAt: &completed}
	for _, test := range []struct {
		name   string
		last   *SearchRunDiagnostic
		latest *string
		active int64
		want   SearchDiagnosticsStatus
	}{
		{name: "ready", last: last, latest: stringPointer("complete"), want: SearchDiagnosticsReady},
		{name: "initial backfill", latest: stringPointer("running"), active: 1, want: SearchDiagnosticsBackfilling},
		{name: "never available", latest: stringPointer("failed"), want: SearchDiagnosticsUnavailable},
		{name: "partial after success", last: last, latest: stringPointer("partial"), want: SearchDiagnosticsStale},
		{name: "failed after success", last: last, latest: stringPointer("failed"), want: SearchDiagnosticsStale},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := publicSearchStatus(test.last, test.latest, test.active, now, 48*time.Hour); got != test.want {
				t.Fatalf("status=%q want %q", got, test.want)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }
