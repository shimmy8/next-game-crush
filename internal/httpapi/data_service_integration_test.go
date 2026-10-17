package httpapi

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/store"
)

func TestDeleteUserDataRetainsSharedCatalogAndRemovesOwnedRows(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx := context.Background()
	userID, gameID, wishlistGameID, profileID, recID, deliveryID, jobID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
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
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO profile_embeddings(profile_id,polarity,model,input_content_hash,embedding)VALUES($1,'positive','test','x',$2)`, []any{profileID, zeroVectorLiteral()}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key)VALUES($1,$2,$3,1,'v1','build')`, []any{uuid.New(), userID, profileID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score)VALUES($1,$2,$3,$4,'v1',80)`, []any{recID, userID, gameID, profileID}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'like','event')`, []any{uuid.New(), recID, userID}},
		{`INSERT INTO notification_preferences(user_id)VALUES($1)`, []any{userID}},
		{`INSERT INTO email_deliveries(id,user_id,idempotency_key,status)VALUES($1,$2,'delivery','pending')`, []any{deliveryID, userID}},
		{`INSERT INTO sync_jobs(id,kind,idempotency_key,payload,user_id)VALUES($1,'steam.sync','job',jsonb_build_object('version',1,'user_id',$2::text),$2::uuid)`, []any{jobID, userID}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %s: %v", statement.sql, err)
		}
	}
	if err := NewSQLDataService(db, false, true).DeleteUserData(ctx, userID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"users", "sessions", "steam_accounts", "user_games", "wishlist_entries", "achievement_summaries", "preference_profiles", "profile_embeddings", "recommendations", "feedback_events", "notification_preferences", "email_deliveries", "sync_jobs"} {
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
	var wishlistCatalog int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_entries WHERE origin='wishlist'`).Scan(&wishlistCatalog); err != nil {
		t.Fatal(err)
	}
	if wishlistCatalog != 0 {
		t.Fatalf("wishlist catalog entries count=%d", wishlistCatalog)
	}
}

func TestDeleteFencesInFlightWorkerFromRecreatingData(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	userID, gameID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id)VALUES($1)`, userID); err != nil {
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
