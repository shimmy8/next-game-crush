package notifications

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
)

type allowAllNotificationEligibility struct{}

func (allowAllNotificationEligibility) Eligible(context.Context, uuid.UUID, time.Time) (bool, error) {
	return true, nil
}

type selectiveNotificationEligibility struct {
	allowed map[uuid.UUID]bool
	err     error
}

func (r selectiveNotificationEligibility) Eligible(_ context.Context, id uuid.UUID, _ time.Time) (bool, error) {
	return r.allowed[id], r.err
}
func (r selectiveNotificationEligibility) FilterEligibleGameIDs(_ context.Context, ids []uuid.UUID, _ time.Time) (map[uuid.UUID]bool, error) {
	if r.err != nil {
		return nil, r.err
	}
	result := map[uuid.UUID]bool{}
	for _, id := range ids {
		if r.allowed[id] {
			result[id] = true
		}
	}
	return result, nil
}

func TestCurrentEligibilityProtectsDigestInputBeforeLimits(t *testing.T) {
	db := openNotificationStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	userID, profileID := uuid.New(), uuid.New()
	eligible, expired := uuid.New(), uuid.New()
	setup := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO steam_accounts(user_id,steam_id,display_name) VALUES($1,'76561198000000001','Player')`, []any{userID}},
		{`INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,'ranking-v1','eligibility-digest',true)`, []any{uuid.New(), userID, profileID}},
	}
	for _, statement := range setup {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	for index, gameID := range []uuid.UUID{expired, eligible} {
		appID := int64(93001 + index)
		if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,$2,$3,'test',$4)`, gameID, appID, fmt.Sprintf("Game %d", index), now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, gameID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score,ranking_version) VALUES($1,$2,$3,$4,'ranking-v1',$5,1)`, uuid.New(), userID, gameID, profileID, 100-index); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := NewSQLRepository(db, selectiveNotificationEligibility{allowed: map[uuid.UUID]bool{eligible: true}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	input, err := repo.DigestInput(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Recommendations) != 1 || input.Recommendations[0].GameID != eligible || len(input.Wishlist) != 1 || input.Wishlist[0].GameID != eligible {
		t.Fatalf("digest leaked ineligible rows: %#v", input)
	}
	repo.eligibility = selectiveNotificationEligibility{err: errors.New("eligibility unavailable")}
	if _, err := repo.DigestInput(ctx, userID); err == nil {
		t.Fatal("eligibility failure did not fail digest closed")
	}
}
func (allowAllNotificationEligibility) FilterEligibleGameIDs(_ context.Context, ids []uuid.UUID, _ time.Time) (map[uuid.UUID]bool, error) {
	result := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

type schedulerStub struct {
	calls int
	id    uuid.UUID
	keys  map[string]uuid.UUID
}

func (s *schedulerStub) Enqueue(_ context.Context, spec jobs.Spec) (uuid.UUID, error) {
	if s.keys == nil {
		s.keys = map[string]uuid.UUID{}
	}
	if id, ok := s.keys[spec.IdempotencyKey]; ok {
		return id, nil
	}
	s.calls++
	id := uuid.New()
	s.keys[spec.IdempotencyKey] = id
	if s.id == uuid.Nil {
		s.id = id
	}
	return id, nil
}

func TestDigestDeliveryIdempotencySuppressionOwnershipAndActionReplay(t *testing.T) {
	db := openNotificationStore(t)
	ctx := context.Background()
	userID := uuid.New()
	gameID := uuid.New()
	profileID := uuid.New()
	recommendationID := uuid.New()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id)VALUES($1)`, []any{userID}},
		{`INSERT INTO steam_accounts(user_id,steam_id,display_name)VALUES($1,'76561198000000000','Player <unsafe>')`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,9001,'Game & One','test',$2)`, []any{gameID, now}},
		{`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,review_band,review_label,positive_percent,review_total,last_successful_appdetails_at,active)
			VALUES(9001,1,1,'positive','Very Positive',91,1284,$1,true)`, []any{now}},
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, []any{profileID, userID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published)VALUES($1,$2,$3,1,'ranking-v1','build',true)`, []any{uuid.New(), userID, profileID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score,explanation,ranking_version)VALUES($1,$2,$3,$4,'ranking-v1',95,'Because <facts>',1)`, []any{recommendationID, userID, gameID, profileID}},
		{`INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at)VALUES($1,$2,'test',$3)`, []any{userID, gameID, now}},
		{`INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at)VALUES($1,$2,'day',$3,'18 Oct 2026',$4,'test',$4)`, []any{uuid.New(), gameID, now.AddDate(0, 0, 1), now}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	repository, _ := NewSQLRepository(db, allowAllNotificationEligibility{}, func() time.Time { return now })
	projected, err := repository.DigestInput(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Recommendations) != 1 || projected.Recommendations[0].Reviews == nil || projected.Recommendations[0].Reviews.Band != "positive" ||
		projected.Recommendations[0].Reviews.PositivePercent == nil || *projected.Recommendations[0].Reviews.PositivePercent != 91 || projected.Recommendations[0].Reviews.Total != 1284 {
		t.Fatalf("projected reviews=%#v", projected.Recommendations)
	}
	signer, _ := NewSigner([]byte(strings.Repeat("x", 32)), actionPurpose)
	scheduler := &schedulerStub{}
	service, err := NewService(repository, scheduler, SenderFunc(func(context.Context, Message) (bool, error) { return true, nil }), signer, "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	first, err := service.SendTestDigest(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SendTestDigest(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || scheduler.calls != 1 {
		t.Fatalf("deliveries %s %s, enqueue calls=%d", first, second, scheduler.calls)
	}
	token := uuid.New()
	if _, err := repository.ClaimDelivery(ctx, first, "worker-a", token); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkFailed(ctx, first, "worker-b", uuid.New(), fmt.Errorf("wrong owner")); err != ErrDeliveryOwned {
		t.Fatalf("wrong owner err=%v", err)
	}
	if err := repository.MarkSent(ctx, first, "worker-a", token); err != nil {
		t.Fatal(err)
	}
	staleID := uuid.New()
	stale, _, err := repository.CreateDelivery(ctx, NewDelivery{Delivery: Delivery{ID: staleID, UserID: userID, Status: "pending", Recipient: "test@local", Subject: "stale", Text: "plain", HTML: "<p>plain</p>"}, Kind: "test", ContentHash: "stale", IdempotencyKey: "stale-attempt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ClaimDelivery(ctx, stale.ID, "dead-worker", uuid.New()); err != nil {
		t.Fatal(err)
	}
	reconciled, err := repository.ClaimDelivery(ctx, stale.ID, "new-worker", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != "ambiguous" {
		t.Fatalf("stale status=%s", reconciled.Status)
	}
	failedID := uuid.New()
	failed, _, err := repository.CreateDelivery(ctx, NewDelivery{Delivery: Delivery{ID: failedID, UserID: userID, Status: "pending", Recipient: "test@local", Subject: "failed", Text: "plain", HTML: "<p>plain</p>"}, Kind: "test", ContentHash: "failed", IdempotencyKey: "failed-attempt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE email_deliveries SET status='failed' WHERE id=$1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,status,attempts,max_attempts)VALUES($1,$2,$3,'dead',3,3)`, uuid.New(), JobKind, "digest-delivery:"+failed.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReviveFailedDeliveryJob(ctx, failed.ID); err != nil {
		t.Fatal(err)
	}
	var jobStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE idempotency_key=$1`, "digest-delivery:"+failed.ID.String()).Scan(&jobStatus); err != nil || jobStatus != "retryable" {
		t.Fatalf("revived status=%q err=%v", jobStatus, err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,status,attempts,max_attempts)VALUES($1,$2,$3,'dead',3,3)`, uuid.New(), JobKind, "digest-delivery:"+stale.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReviveFailedDeliveryJob(ctx, stale.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE idempotency_key=$1`, "digest-delivery:"+stale.ID.String()).Scan(&jobStatus); err != nil || jobStatus != "dead" {
		t.Fatalf("ambiguous job revived status=%q err=%v", jobStatus, err)
	}
	third, err := service.SendTestDigest(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if third != first || scheduler.calls != 1 {
		t.Fatalf("same-week repeat delivery=%s enqueue calls=%d", third, scheduler.calls)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE email_deliveries SET created_at=created_at-interval '7 days' WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	newProfileID, newRecommendationID := uuid.New(), uuid.New()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,2)`, []any{newProfileID, userID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published)VALUES($1,$2,$3,2,'ranking-v1','build-2',true)`, []any{uuid.New(), userID, newProfileID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score,explanation,ranking_version)VALUES($1,$2,$3,$4,'ranking-v1',95,'Because <facts>',2)`, []any{newRecommendationID, userID, gameID, newProfileID}},
	} {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	input, err := repository.DigestInput(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Recommendations) != 0 {
		t.Fatalf("unchanged recommendations repeated: %#v", input.Recommendations)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at)VALUES($1,$2,'day',$3,'25 Oct 2026',$4,'test',$4)`, uuid.New(), gameID, now.AddDate(0, 0, 8), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	input, err = repository.DigestInput(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Recommendations) != 1 || input.Recommendations[0].ID != newRecommendationID {
		t.Fatalf("release-only change was not included: %#v", input.Recommendations)
	}
	var action Action
	if err := db.Pool.QueryRow(ctx, `SELECT user_id,recommendation_id,action,issued_at,expires_at,nonce FROM email_action_nonces WHERE delivery_id=$1 AND action='like'`, first).Scan(&action.UserID, &action.RecommendationID, &action.Action, &action.IssuedAt, &action.ExpiresAt, &action.Nonce); err != nil {
		t.Fatal(err)
	}
	already, err := repository.ConsumeAction(ctx, action)
	if err != nil || already {
		t.Fatalf("first consume already=%v err=%v", already, err)
	}
	already, err = repository.ConsumeAction(ctx, action)
	if err != nil || !already {
		t.Fatalf("replay already=%v err=%v", already, err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM feedback_events WHERE idempotency_key=$1`, "email-action:"+action.Nonce.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("feedback count=%d err=%v", count, err)
	}
}

func openNotificationStore(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	database, err := store.Open(ctx, fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", base, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanup, _ := pgx.Connect(context.Background(), base)
		if cleanup != nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return database
}
