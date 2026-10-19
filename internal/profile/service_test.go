package profile

import (
	"context"
	"encoding/json"
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

func TestLatestProfileReportsMissingSummaryAsUnavailable(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID, profileID := uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,summary,algorithm_metadata) VALUES($1,$2,1,NULL,'{"confidence":0.5,"evidenceCount":2}')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}

	got, err := repository.LatestProfile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "" || got.SummaryStatus != SummaryUnavailable {
		t.Fatalf("missing summary = %q/%q, want empty/%q", got.Summary, got.SummaryStatus, SummaryUnavailable)
	}

	if _, err := database.Pool.Exec(ctx, `UPDATE preference_profiles SET summary='Любит вдумчивые кооперативные игры.' WHERE id=$1`, profileID); err != nil {
		t.Fatal(err)
	}
	got, err = repository.LatestProfile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Любит вдумчивые кооперативные игры." || got.SummaryStatus != SummaryReady {
		t.Fatalf("ready summary = %q/%q", got.Summary, got.SummaryStatus)
	}
}

func TestLoadSignalsIncludesLastPlayedAt(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID, gameID := uuid.New(), uuid.New()
	lastPlayed := time.Date(2016, time.October, 18, 12, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,570,'Dota 2','test',now())`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,source,last_successful_fetch_at) VALUES($1,$2,100000,0,$3,'test',now())`, userID, gameID, lastPlayed); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, Dimensions)
	vector[0] = 1
	if err := database.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: "dota", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}

	signals, err := repository.LoadSignals(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || !signals[0].LastPlayedAt.Equal(lastPlayed) {
		t.Fatalf("signals = %#v, want last played %s", signals, lastPlayed)
	}
}

func TestLoadActivitySignalsIgnoreDirectActionsAndPreserveCompletion(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID, gameID, profileID := uuid.New(), uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,730,'Completion fixture','test',now())`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,basis,algorithm_version,updated_at) VALUES($1,$2,2,false,1.85,'ending','completion-v1',now())`, userID, gameID); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, Dimensions)
	vector[0] = 1
	if err := database.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: "completion", Vector: vector}); err != nil {
		t.Fatal(err)
	}
	recommendationID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score) VALUES($1,$2,$3,$4,'test',1)`, recommendationID, userID, gameID, profileID); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}
	insertFeedback := func(action string, at time.Time) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at) VALUES($1,$2,$3,$4,$5,$6)`, uuid.New(), recommendationID, userID, action, uuid.NewString(), at); err != nil {
			t.Fatal(err)
		}
	}
	assertSignal := func() {
		t.Helper()
		signals, err := repository.LoadSignals(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		if len(signals) != 1 {
			t.Fatalf("signals = %#v, want one", signals)
		}
		got := signals[0]
		if got.CompletionWeight != 1.85 {
			t.Fatalf("activity signal leaked direct feedback: %#v", got)
		}
		var weight float64
		if err := database.Pool.QueryRow(ctx, `SELECT weight FROM user_game_completion_projections WHERE user_id=$1 AND game_id=$2`, userID, gameID).Scan(&weight); err != nil || weight != 1.85 {
			t.Fatalf("stored projection weight = %v, err=%v", weight, err)
		}
	}

	base := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	insertFeedback("dislike", base)
	insertFeedback("like", base.Add(time.Minute))
	assertSignal()
	insertFeedback("dislike", base.Add(2*time.Minute))
	assertSignal()
	insertFeedback("like", base.Add(3*time.Minute))
	assertSignal()
	insertFeedback("exclude", base.Add(4*time.Minute))
	assertSignal()
}

func TestProfileCompletionVersionMetadataAndBuildIdentity(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	legacyID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata,build_key) VALUES($1,$2,1,'{"algorithmVersion":"profile-v3-direct-feedback-once"}',$3)`, legacyID, userID, "same-source-build"); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.rebuildWithKey(ctx, userID, "same-source-build")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == legacyID || got.Version != 2 {
		t.Fatalf("completion build reused legacy profile: %#v", got)
	}
	if AlgorithmVersion != "activity-profile-v5" {
		t.Fatalf("algorithm version = %q", AlgorithmVersion)
	}
	var metadata []byte
	var buildKey string
	if err := database.Pool.QueryRow(ctx, `SELECT algorithm_metadata,build_key FROM preference_profiles WHERE id=$1`, got.ID).Scan(&metadata, &buildKey); err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatal(err)
	}
	if values["algorithmVersion"] != AlgorithmVersion || values["completionAlgorithmVersion"] != "completion-v1" {
		t.Fatalf("metadata = %s", metadata)
	}
	if buildKey == "same-source-build" || !strings.Contains(buildKey, AlgorithmVersion) || !strings.Contains(buildKey, "completion-v1") {
		t.Fatalf("build key = %q, want versioned completion identity", buildKey)
	}
	replayed, err := service.rebuildWithKey(ctx, userID, "same-source-build")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != got.ID || replayed.Version != got.Version {
		t.Fatalf("versioned build did not replay: first=%#v replay=%#v", got, replayed)
	}
}

func TestLaterRebuildEnqueuesFreshIdempotentSummaryAfterDeadAttempt(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID, gameID := uuid.New(), uuid.New()
	seedActivityGame(t, database, userID, gameID, true)
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(database)
	service.SetQueue(queue)
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "profile-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second), jobs.WithRetryBase(time.Millisecond))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("ai.profile_summary", func(context.Context, jobs.Lease) error { return permanentSummaryError{} }); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, UserID: userID})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker: %v", err)
		}
	})

	for _, key := range []string{"sync:first:profile:v1", "sync:later:profile:v1"} {
		if _, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: JobKind, IdempotencyKey: key, Payload: payload, MaxAttempts: 3}); err != nil {
			t.Fatal(err)
		}
		waitForProfileJobs(t, database, key)
		var encoded []byte
		if err := database.Pool.QueryRow(ctx, `SELECT result FROM sync_jobs WHERE idempotency_key=$1`, key).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var result struct {
			ProfileID        uuid.UUID        `json:"profileId"`
			ProfileVersion   int64            `json:"profileVersion"`
			PublicationState PublicationState `json:"publicationState"`
			SummaryRequired  bool             `json:"summaryRequired"`
		}
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		if result.ProfileID == uuid.Nil || result.ProfileVersion == 0 || result.PublicationState != PublicationBuilding || !result.SummaryRequired {
			t.Fatalf("profile job result = %s", encoded)
		}
	}

	var profiles, summaries, distinctSummaryKeys int
	if err := database.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM preference_profiles WHERE user_id=$1),
		(SELECT count(*) FROM sync_jobs WHERE kind='ai.profile_summary' AND user_id=$1),
		(SELECT count(DISTINCT idempotency_key) FROM sync_jobs WHERE kind='ai.profile_summary' AND user_id=$1)`, userID).Scan(&profiles, &summaries, &distinctSummaryKeys); err != nil {
		t.Fatal(err)
	}
	if profiles != 2 || summaries != 2 || distinctSummaryKeys != 2 {
		t.Fatalf("profiles/summaries/distinct keys = %d/%d/%d, want 2/2/2", profiles, summaries, distinctSummaryKeys)
	}
}

func TestProfileWorkerRecordsInsufficientWithoutSummaryJob(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(database)
	service.SetQueue(queue)
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "profile-insufficient-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second), jobs.WithRetryBase(time.Millisecond))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker: %v", err)
		}
	})
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, UserID: userID})
	key := "sync:empty:profile:v1"
	if _, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: JobKind, IdempotencyKey: key, Payload: payload, UserID: userID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		var result []byte
		err := database.Pool.QueryRow(ctx, `SELECT status,result FROM sync_jobs WHERE idempotency_key=$1`, key).Scan(&status, &result)
		if err == nil && status == "succeeded" {
			var decoded struct {
				PublicationState PublicationState `json:"publicationState"`
				SummaryRequired  bool             `json:"summaryRequired"`
			}
			if err := json.Unmarshal(result, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.PublicationState != PublicationInsufficient || decoded.SummaryRequired {
				t.Fatalf("job result = %s", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("insufficient profile job did not succeed: status=%q err=%v", status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var summaries int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='ai.profile_summary' AND user_id=$1`, userID).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if summaries != 0 {
		t.Fatalf("insufficient profile summary jobs = %d, want 0", summaries)
	}
}

func TestLatestPublishedProfileKeepsPublishedWhileBuilding(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	oldID, draftID := uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,summary,algorithm_metadata,publication_state,published_at,summary_source) VALUES
		($1,$3,1,'Стабильный опубликованный профиль','{"confidence":0.7,"evidenceCount":3}','published',now()-interval '1 day','legacy'),
		($2,$3,2,NULL,'{"confidence":0.9,"evidenceCount":8}','building',NULL,NULL)`, oldID, draftID, userID); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}
	got, err := repository.LatestPublishedProfile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != oldID || got.Version != 1 || got.Summary != "Стабильный опубликованный профиль" || !got.Updating ||
		got.Confidence != 0.7 || got.EvidenceCount != 3 || got.SummarySource != SummarySourceLegacy || got.PublishedAt == nil {
		t.Fatalf("visible profile = %#v", got)
	}
}

func TestLatestPublishedProfileClearsUpdatingAfterFailure(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	oldID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,summary,algorithm_metadata,publication_state,published_at,summary_source) VALUES
		($1,$2,1,'Стабильный профиль','{}','published',now()-interval '1 day','legacy'),
		($3,$2,2,NULL,'{}','failed',NULL,NULL)`, oldID, userID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed")
	got, err := repository.LatestPublishedProfile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != oldID || got.Updating {
		t.Fatalf("visible profile = %#v", got)
	}
}

func TestLatestPublishedProfileTreatsInsufficientAsAuthoritativeEmpty(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,summary,algorithm_metadata,publication_state,published_at,summary_source) VALUES
		($1,$3,1,'Старый профиль','{}','published',now()-interval '1 day','legacy'),
		($2,$3,2,NULL,'{}','insufficient',NULL,NULL)`, uuid.New(), uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed")
	if _, err := repository.LatestPublishedProfile(ctx, userID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("error = %v, want pgx.ErrNoRows", err)
	}
}

type permanentSummaryError struct{}

func (permanentSummaryError) Error() string   { return "summary unavailable" }
func (permanentSummaryError) Retryable() bool { return false }

func waitForProfileJobs(t *testing.T, database *store.Store, profileKey string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var profileStatus, summaryStatus string
		err := database.Pool.QueryRow(context.Background(), `SELECT profile.status, summary.status
			FROM sync_jobs profile
			JOIN sync_jobs summary ON summary.idempotency_key=profile.idempotency_key || ':summary:v1'
			WHERE profile.idempotency_key=$1`, profileKey).Scan(&profileStatus, &summaryStatus)
		if err == nil && profileStatus == "succeeded" && summaryStatus == "dead" {
			return
		}
		if err != nil && !errorsIsNoRows(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("profile %q did not complete with an independently dead summary job: profile=%q summary=%q err=%v", profileKey, profileStatus, summaryStatus, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func errorsIsNoRows(err error) bool { return err == pgx.ErrNoRows }

func openProfileStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = connection.Close(ctx)
		t.Fatal(err)
	}
	_ = connection.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	database, err := store.Open(ctx, fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanup, cleanupErr := pgx.Connect(context.Background(), baseURL)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return database
}
