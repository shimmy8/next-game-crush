package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/store"
)

type discoveryDiagnosticsStub struct {
	value catalog.DiscoveryDiagnostics
	err   error
}

func (s discoveryDiagnosticsStub) Diagnostics(context.Context) (catalog.DiscoveryDiagnostics, error) {
	return s.value, s.err
}

func TestHealthIncludesAggregateCatalogDiscovery(t *testing.T) {
	db := openHTTPTestStore(t)
	service := NewSQLDataService(db, false, false)
	service.SetDiscoveryDiagnosticsReader(discoveryDiagnosticsStub{value: catalog.DiscoveryDiagnostics{
		PresentationStatus: catalog.PresentationStatus{Status: "updating", ActiveDiscoveredGames: 12},
		LatestRun:          catalog.DiscoveryRunCounts{Discovered: 4, Evaluated: 3, Admitted: 1, Skipped: 1, Failed: 1},
		ActiveBySource:     map[string]int64{"steam_discovery": 12}, PendingQualifications: 2, Reason: "backfill_in_progress",
	}})
	report := service.Health(context.Background())
	if report.CatalogDiscovery == nil || report.CatalogDiscovery.ActiveDiscoveredGames != 12 || report.CatalogDiscovery.PendingQualifications != 2 {
		t.Fatalf("health=%+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"steamAppId", "userId", "payload", "raw"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("unsafe discovery field in health: %s", encoded)
		}
	}
}

func TestDataServiceExplanationDiagnosticsAllowlist(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx := context.Background()
	userID := uuid.New()
	jobID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	result := `{
		"explanations": {
			"aiGenerated": 2,
			"deterministic": -3,
			"none": 1,
			"failureReasons": {
				"disabled": 4,
				"rate_limited": -2,
				"timeout": 3,
				"provider_unavailable": 2,
				"refusal": 1,
				"invalid_output": 5,
				"invalid_citation": 6,
				"unsafe_text": 7,
				"unknown": 8,
				"private_provider_message": 99
			},
			"gameName": "Secret Game",
			"gameId": "f9199d3f-4eb0-4029-9270-df1b20e5639e",
			"prompt": "ignore previous instructions"
		},
		"rawProviderBody": "private upstream body"
	}`
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload,user_id,status,result) VALUES($1,'recommendations.rebuild','diagnostics-valid','{}',$2,'succeeded',$3::jsonb)`, jobID, userID, result); err != nil {
		t.Fatal(err)
	}
	jobs, err := NewSQLDataService(db, false, false).Jobs(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Explanations == nil {
		t.Fatalf("jobs = %#v", jobs)
	}
	wantReasons := map[string]int{
		"disabled": 4, "rate_limited": 0, "timeout": 3, "provider_unavailable": 2,
		"refusal": 1, "invalid_output": 5, "invalid_citation": 6, "unsafe_text": 7, "unknown": 8,
	}
	got := jobs[0].Explanations
	if got.AIGenerated != 2 || got.Deterministic != 0 || got.None != 1 {
		t.Fatalf("aggregate counts = %#v", got)
	}
	if len(got.FailureReasons) != len(wantReasons) {
		t.Fatalf("failure reasons = %#v", got.FailureReasons)
	}
	for reason, want := range wantReasons {
		if got.FailureReasons[reason] != want {
			t.Errorf("failure reason %q = %d, want %d", reason, got.FailureReasons[reason], want)
		}
	}
	encoded, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"Secret Game", "f9199d3f", "ignore previous instructions", "private upstream body", "private_provider_message"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("diagnostics leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestDataServiceOmitsMalformedExplanationDiagnostics(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		kind, key, status, result string
	}{
		{kind: "steam.sync", key: "other-kind", status: "succeeded", result: `{"explanations":{"aiGenerated":1,"deterministic":2,"none":3,"failureReasons":{}}}`},
		{kind: "recommendations.rebuild", key: "missing-result", status: "succeeded", result: `null`},
		{kind: "recommendations.rebuild", key: "wrong-shape", status: "succeeded", result: `{"explanations":"private raw text"}`},
		{kind: "recommendations.rebuild", key: "fractional-count", status: "succeeded", result: `{"explanations":{"aiGenerated":1.5,"deterministic":2,"none":3,"failureReasons":{}}}`},
		{kind: "recommendations.rebuild", key: "dead-without-result", status: "dead", result: `null`},
	}
	for _, test := range tests {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload,user_id,status,result,last_error) VALUES($1,$2,$3,'{}',$4,$5,$6::jsonb,'upstream request failed')`, uuid.New(), test.kind, test.key, userID, test.status, test.result); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := NewSQLDataService(db, false, false).Jobs(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(tests) {
		t.Fatalf("jobs length = %d, want %d", len(jobs), len(tests))
	}
	for _, job := range jobs {
		if job.Explanations != nil {
			t.Errorf("job %q exposed malformed diagnostics: %#v", job.Kind, job.Explanations)
		}
		if job.Error != "operation failed" {
			t.Errorf("job %q sanitized error = %q", job.Kind, job.Error)
		}
	}
}

func TestDeleteUserDataRetainsSharedCatalogAndRemovesOwnedRows(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx := context.Background()
	userID, gameID, wishlistGameID, profileID, recID, deliveryID, jobID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	generationID, classificationID := uuid.New(), uuid.New()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id)VALUES($1)`, []any{userID}},
		{`INSERT INTO steam_accounts(user_id,steam_id)VALUES($1,'76561197960287930')`, []any{userID}},
		{`INSERT INTO sessions(id,user_id,csrf_token,expires_at)VALUES($1,$2,'csrf',now()+interval '1 hour')`, []any{uuid.New(), userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,42,'Shared game','test',now())`, []any{gameID}},
		{`INSERT INTO catalog_entries(game_id,origin)VALUES($1,'seed')`, []any{gameID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,43,'Wishlist discovery','steam_store',now())`, []any{wishlistGameID}},
		{`INSERT INTO catalog_entries(game_id,origin)VALUES($1,'wishlist')`, []any{wishlistGameID}},
		{`INSERT INTO user_games(user_id,game_id,source,last_successful_fetch_at)VALUES($1,$2,'test',now())`, []any{userID, gameID}},
		{`INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at)VALUES($1,$2,'test',now())`, []any{userID, wishlistGameID}},
		{`INSERT INTO achievement_summaries(user_id,game_id,source,last_successful_fetch_at)VALUES($1,$2,'test',now())`, []any{userID, gameID}},
		{`INSERT INTO game_achievement_definitions(game_id,api_name,display_name,description,metadata_source,definition_content_hash,last_successful_fetch_at) VALUES($1,'ENDING','The End','Reach an ending','player_achievements','definition-hash',now())`, []any{gameID}},
		{`INSERT INTO user_achievement_unlocks(user_id,game_id,api_name,present_in_latest,achieved_in_latest,ever_achieved,unlocked_at,snapshot_generation,source,last_successful_fetch_at) VALUES($1,$2,'ENDING',true,true,true,now(),1,'player_achievements',now())`, []any{userID, gameID}},
		{`INSERT INTO achievement_semantic_classification_generations(id,game_id,model,prompt_version,schema_version,definition_set_hash,lexical_rule_version,status,activated_at) VALUES($1,$2,'fixture','achievement-semantics-v1','achievement-semantics-schema-v1','set-hash','lexical-completion-v1','active',now())`, []any{generationID, gameID}},
		{`INSERT INTO achievement_semantic_classifications(id,generation_id,game_id,api_name,definition_content_hash,class,model,prompt_version,schema_version,input_content_hash,classification_source,lexical_rule_version) VALUES($1,$2,$3,'ENDING','definition-hash','base_game_ending','fixture','achievement-semantics-v1','achievement-semantics-schema-v1','input-hash','lexical','lexical-completion-v1')`, []any{classificationID, generationID, gameID}},
		{`INSERT INTO user_game_completion_evidence(user_id,game_id,kind,api_name,definition_content_hash,semantic_classification_id,unlocked_at,observed_at,algorithm_version) VALUES($1,$2,'ending_marker','ENDING','definition-hash',$3,now(),now(),'completion-v1')`, []any{userID, gameID, classificationID}},
		{`INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,completion_at,basis,algorithm_version) VALUES($1,$2,1,false,1.5,now(),'ending','completion-v1')`, []any{userID, gameID}},
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO profile_embeddings(profile_id,polarity,model,input_content_hash,embedding)VALUES($1,'positive','test','x',$2)`, []any{profileID, zeroVectorLiteral()}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key)VALUES($1,$2,$3,1,'v1','build')`, []any{uuid.New(), userID, profileID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score)VALUES($1,$2,$3,$4,'v1',80)`, []any{recID, userID, gameID, profileID}},
		{`INSERT INTO catalog_explanation_cache(user_id,game_id,model,input_hash,explanation)VALUES($1,$2,'fixture','personalized',jsonb_build_object('text','Owned game: private context'))`, []any{userID, gameID}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'like','event')`, []any{uuid.New(), recID, userID}},
		{`INSERT INTO notification_preferences(user_id)VALUES($1)`, []any{userID}},
		{`INSERT INTO email_deliveries(id,user_id,idempotency_key,status)VALUES($1,$2,'delivery','pending')`, []any{deliveryID, userID}},
		{`INSERT INTO sync_jobs(id,kind,idempotency_key,payload,user_id)VALUES($1,'steam.sync','job',jsonb_build_object('version',1,'user_id',$2::text),$2::uuid)`, []any{jobID, userID}},
		{`INSERT INTO sync_jobs(id,kind,idempotency_key,payload,user_id)VALUES($1,'completion.project','completion-project',jsonb_build_object('version',1,'user_id',$2::text,'game_id',$3::text),$2::uuid)`, []any{uuid.New(), userID, gameID}},
		{`INSERT INTO sync_jobs(id,kind,idempotency_key,payload)VALUES($1,'achievement.classify','achievement-classify',jsonb_build_object('version',1,'game_id',$2::text))`, []any{uuid.New(), gameID}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %s: %v", statement.sql, err)
		}
	}
	if err := NewSQLDataService(db, false, true).DeleteUserData(ctx, userID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"users", "sessions", "steam_accounts", "user_games", "wishlist_entries", "achievement_summaries", "user_achievement_unlocks", "user_game_completion_evidence", "user_game_completion_projections", "preference_profiles", "profile_embeddings", "recommendations", "catalog_explanation_cache", "feedback_events", "notification_preferences", "email_deliveries"} {
		var count int
		if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s count=%d, want 0", table, count)
		}
	}
	var games int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM games WHERE id=$1`, gameID).Scan(&games); err != nil {
		t.Fatal(err)
	}
	if games != 1 {
		t.Fatalf("shared games count=%d", games)
	}
	for table, want := range map[string]int{"game_achievement_definitions": 1, "achievement_semantic_classification_generations": 1, "achievement_semantic_classifications": 1} {
		var count int
		if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE game_id=$1", gameID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("shared %s count=%d, want %d", table, count, want)
		}
	}
	var sharedJobs, ownedJobs int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE user_id IS NULL),count(*) FILTER (WHERE user_id IS NOT NULL) FROM sync_jobs`).Scan(&sharedJobs, &ownedJobs); err != nil {
		t.Fatal(err)
	}
	if sharedJobs != 1 || ownedJobs != 0 {
		t.Fatalf("jobs shared=%d owned=%d, want 1/0", sharedJobs, ownedJobs)
	}
	var wishlistCatalog int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entries WHERE origin='wishlist'`).Scan(&wishlistCatalog); err != nil {
		t.Fatal(err)
	}
	if wishlistCatalog != 0 {
		t.Fatalf("wishlist catalog entries count=%d", wishlistCatalog)
	}
}

func TestDeleteRetainsInactiveDiscoveryHistory(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	for i, source := range []string{"seed", "steam_discovery", "observation_only"} {
		id := uuid.New()
		appID := int64(91000 + i)
		if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,$2,'Shared history','test',now())`, id, appID); err != nil {
			t.Fatal(err)
		}
		if source != "observation_only" {
			if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entry_sources(game_id,source,active) VALUES($1,$2,false)`, id, source); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status,attempts) VALUES('steam',$1,now(),'released',1)`, appID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'year','2026',now(),'test',now())`, uuid.New(), id); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct {
		kind, key string
		user      any
	}{{"catalog.discovery.tick", "global-discovery", nil}, {"catalog.rebuild", "personal-rebuild", userID}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload,user_id) VALUES($1,$2,$3,'{}',$4)`, uuid.New(), s.kind, s.key, s.user); err != nil {
			t.Fatal(err)
		}
	}
	if err := NewSQLDataService(db, false, false).DeleteUserData(ctx, userID); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"games": 3, "game_release_dates": 3, "catalog_entry_sources": 2, "catalog_discovery_observations": 3, "sync_jobs": 1} {
		var count int
		if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("%s count=%d want %d", table, count, want)
		}
	}
	var kind string
	if err := db.Pool.QueryRow(ctx, `SELECT kind FROM sync_jobs`).Scan(&kind); err != nil || kind != "catalog.discovery.tick" {
		t.Fatal("global discovery job lost or personal job retained")
	}
}

func TestDeleteFencesInFlightWorkerFromRecreatingData(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	userID, gameID, sharedGameID := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,100,'Shared completion game','test',now())`, sharedGameID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin)VALUES($1,'seed')`, sharedGameID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload,user_id,status,worker_id,fencing_token,lease_until) VALUES($1,'completion.project','leased-projection',jsonb_build_object('version',1,'user_id',$2::text,'game_id',$3::text),$2::uuid,'running','fixture-worker',$4,now()+interval '1 minute')`, uuid.New(), userID, sharedGameID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload) VALUES($1,'achievement.classify','shared-classification',jsonb_build_object('version',1,'game_id',$2::text))`, uuid.New(), sharedGameID); err != nil {
		t.Fatal(err)
	}
	workerTx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = workerTx.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,99,'Wishlist race','test',now())`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err = workerTx.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at)VALUES($1,$2,'in-flight-worker',now())`, userID, gameID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- NewSQLDataService(db, false, false).DeleteUserData(ctx, userID) }()
	// Wait until DELETE is observably blocked on the worker's FK lock. This makes
	// the late catalog insert below exercise the exact production interleaving.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked bool
		if err = db.Pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND query LIKE 'DELETE FROM users WHERE id=$1%'
			AND wait_event_type='Lock')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err = <-result:
			t.Fatalf("deletion completed before in-flight worker resolved: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("deletion never reached the expected FK lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = workerTx.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin)VALUES($1,'wishlist')`, gameID); err != nil {
		t.Fatal(err)
	}
	if err = workerTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	var rows int
	if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM wishlist_entries WHERE user_id=$1`, userID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("in-flight wishlist data survived deletion: %d rows", rows)
	}
	if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entries WHERE game_id=$1`, gameID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("late wishlist catalog entry survived deletion: %d rows", rows)
	}
	if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM games WHERE id=$1`, gameID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("late wishlist game survived deletion: %d rows", rows)
	}
	if _, err = db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,source,last_successful_fetch_at)VALUES($1,$2,'late-worker',now())`, userID, gameID); err == nil {
		t.Fatal("late worker recreated deleted user data")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,payload)VALUES($1,'steam.sync','late-job',jsonb_build_object('user_id',$2::text))`, uuid.New(), userID); err == nil {
		t.Fatal("late worker enqueued work for deleted user")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,basis,algorithm_version) VALUES($1,$2,1,false,1.5,'ending','completion-v1')`, userID, sharedGameID); err == nil {
		t.Fatal("late worker recreated completion projection for deleted user")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err == nil {
		t.Fatal("late worker recreated profile for deleted user")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,algorithm_version,score) VALUES($1,$2,$3,'fixture',50)`, uuid.New(), userID, sharedGameID); err == nil {
		t.Fatal("late worker recreated recommendation for deleted user")
	}
	var sharedJobs, ownedJobs, affectedUsers int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE user_id IS NULL),count(*) FILTER (WHERE user_id IS NOT NULL) FROM sync_jobs`).Scan(&sharedJobs, &ownedJobs); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM user_achievement_unlocks unlocks JOIN users ON users.id=unlocks.user_id WHERE unlocks.game_id=$1`, sharedGameID).Scan(&affectedUsers); err != nil {
		t.Fatal(err)
	}
	if sharedJobs != 1 || ownedJobs != 0 || affectedUsers != 0 {
		t.Fatalf("post-delete fanout inputs shared_jobs=%d owned_jobs=%d affected_users=%d", sharedJobs, ownedJobs, affectedUsers)
	}
}

func zeroVectorLiteral() string {
	return "[" + strings.TrimSuffix(strings.Repeat("0,", 1536), ",") + "]"
}

func openHTTPTestStore(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	db, err := store.Open(context.Background(), fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", base, sep, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, _ := pgx.Connect(context.Background(), base)
		if cleanup != nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return db
}
