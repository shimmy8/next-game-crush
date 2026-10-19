package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
)

func TestInvalidOutputPreservesLastSuccessfulDatabaseValue(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	gameID := uuid.New()
	if err := database.UpsertGame(ctx, store.Game{ID: gameID, SteamAppID: 620, Name: "Portal 2", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	repository := NewSQLRepository(database)
	previous := GameFeatures{Themes: []string{"science fiction"}, Mechanics: []string{}, Modes: []string{"single-player"}, Tone: []string{}, EvidenceKeys: []string{"title"}}
	if err := repository.SaveGameFeatures(ctx, gameID, "gpt-6-luna", "previous-hash", previous); err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository, &fakeClient{}, "gpt-6-luna", "text-embedding-3-small")
	if err := service.processFeatures(ctx, gameID); err == nil {
		t.Fatal("invalid output was accepted")
	}
	var hash string
	var stored []byte
	if err := database.Pool.QueryRow(ctx, `SELECT input_content_hash,features FROM game_features WHERE game_id=$1 AND model='gpt-6-luna'`, gameID).Scan(&hash, &stored); err != nil {
		t.Fatal(err)
	}
	if hash != "previous-hash" || !strings.Contains(string(stored), "science fiction") {
		t.Fatalf("last successful value changed: hash=%q features=%s", hash, stored)
	}
}

func TestFeatureJobEnqueuesEmbeddingAfterValidatedFeatures(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	gameID := uuid.New()
	if err := database.UpsertGame(ctx, store.Game{ID: gameID, SteamAppID: 620, Name: "Portal 2", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, EmbeddingDimensions)
	client := &fakeClient{features: GameFeatures{Themes: []string{"puzzle"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"title"}}, vectors: [][]float32{vector}}
	queue := jobs.NewQueue(database)
	service, _ := NewService(NewSQLRepository(database), client, "gpt-6-luna", "text-embedding-3-small")
	service.SetQueue(queue)
	worker := jobs.NewWorker(queue, jobs.LaneInteractive, "ai-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
	jobID, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: FeaturesJobKind, IdempotencyKey: "ai.features:v1:test", Payload: payload, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := database.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if status == "dead" || time.Now().After(deadline) {
			t.Fatalf("feature job status=%s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var features, embeddings int
	if err := database.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM game_features WHERE game_id=$1),(SELECT count(*) FROM game_embeddings WHERE game_id=$1)`, gameID).Scan(&features, &embeddings); err != nil {
		t.Fatal(err)
	}
	if features != 1 || embeddings != 1 {
		t.Fatalf("features/embeddings=%d/%d", features, embeddings)
	}
}

func TestLoadProfileEvidenceIncludesLastPlayedAt(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	userID, profileID, gameID := uuid.New(), uuid.New(), uuid.New()
	lastPlayed := time.Date(2016, time.October, 18, 12, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertGame(ctx, store.Game{ID: gameID, SteamAppID: 570, Name: "Dota 2", Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO activity_profile_evidence(profile_id,game_id,playtime_minutes,last_played_at) VALUES($1,$2,100000,$3)`, profileID, gameID, lastPlayed); err != nil {
		t.Fatal(err)
	}

	evidence, err := NewSQLRepository(database).LoadProfileEvidence(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Evidence) != 1 || !strings.Contains(evidence.Evidence[0].Value, "last_played_at=2016-10-18T12:00:00Z") {
		t.Fatalf("profile evidence = %#v", evidence.Evidence)
	}
}

func TestLoadProfileEvidenceProtectsTwoWeekActivityAndCompletionFromLimit(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	userID, profileID := uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	completedID := insertAIEvidenceGame(t, database, 20_001, "Completed Story")
	recentID := insertAIEvidenceGame(t, database, 20_002, "Two Week Activity")
	wishlistID := insertAIEvidenceGame(t, database, 20_003, "Wishlist Must Not Leak")
	completionAt := time.Date(2018, time.May, 4, 12, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `INSERT INTO activity_profile_evidence(profile_id,game_id,completion_ending_count,completion_hundred_percent_basis,completion_weight,completion_at,completion_basis) VALUES($1,$2,2,true,1.85,$3,'both')`, profileID, completedID, completionAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO activity_profile_evidence(profile_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,achieved,total) VALUES($1,$2,15,240,now(),3,10)`, profileID, recentID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, userID, wishlistID); err != nil {
		t.Fatal(err)
	}

	repository := NewSQLRepository(database)
	evidence, err := repository.LoadProfileEvidence(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Evidence) != 2 {
		t.Fatalf("evidence count = %d, want 2", len(evidence.Evidence))
	}
	again, err := repository.LoadProfileEvidence(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence, again) {
		t.Fatal("profile evidence order is not stable across reads")
	}
	joined := evidence.Evidence[0].Value + "\n" + evidence.Evidence[1].Value
	for _, forbidden := range []string{"wishlist", "direct_feedback", "positive_feedback", "completion_overridden", wishlistID.String()} {
		if strings.Contains(strings.ToLower(joined), strings.ToLower(forbidden)) {
			t.Fatalf("unsafe profile evidence contains %q: %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "completion_basis=both") || !strings.Contains(joined, "recent_minutes=240") {
		t.Fatalf("incomplete profile evidence: %s", joined)
	}
	if evidence.Facts.PlayedGames != 1 || evidence.Facts.RecentlyPlayedGames != 1 || evidence.Facts.GamesWithAchievements != 1 || evidence.Facts.CompletedGames != 1 {
		t.Fatalf("activity facts = %#v", evidence.Facts)
	}
}

func TestProfilePublicationRejectsLateLeaseAndTerminalResult(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	newDraft := func(version int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		metadata := fmt.Sprintf(`{"signalCounts":{"playtime":%d,"recent":1,"achievements":1,"completion":1}}`, version)
		if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata,publication_state) VALUES($1,$2,$3,$4,'building')`, id, userID, version, metadata); err != nil {
			t.Fatal(err)
		}
		return id
	}
	claim := func(profileID uuid.UUID, key string) jobs.Lease {
		t.Helper()
		payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, ProfileID: profileID})
		queue := jobs.NewQueue(database)
		if _, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: ProfileSummaryJobKind, IdempotencyKey: key, Payload: payload, UserID: userID, MaxAttempts: 3}); err != nil {
			t.Fatal(err)
		}
		lease, err := queue.Claim(ctx, jobs.LaneInteractive, "summary-worker", time.Minute)
		if err != nil || lease == nil {
			t.Fatalf("claim: lease=%#v err=%v", lease, err)
		}
		return *lease
	}

	repository := NewSQLRepository(database)
	first := newDraft(1)
	lease := claim(first, "summary:first")
	output := ProfileSummary{Summary: "Сводка фактической игровой активности."}
	if err := repository.PublishProfileSummary(ctx, lease, first, "text-v1", "hash-v1", output, "openai"); err != nil {
		t.Fatal(err)
	}
	// A retry after publish but before durable job completion is idempotent.
	if err := repository.PublishProfileSummary(ctx, lease, first, "text-v1", "hash-v1", output, "openai"); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}

	second := newDraft(2)
	secondLease := claim(second, "summary:second")
	stale := secondLease
	stale.FencingToken = uuid.New()
	if err := repository.PublishProfileSummary(ctx, stale, second, "text-v1", "hash-v2", output, "openai"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale publish error = %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE sync_jobs SET status='dead',worker_id=NULL,fencing_token=NULL,lease_until=NULL WHERE id=$1`, secondLease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.HandleTerminalProfileSummaryFailure(ctx, secondLease, second, "text-v1"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("late terminal error = %v", err)
	}
	var state string
	if err := database.Pool.QueryRow(ctx, `SELECT publication_state FROM preference_profiles WHERE id=$1`, second).Scan(&state); err != nil || state != "building" {
		t.Fatalf("late result changed state=%q err=%v", state, err)
	}

	third := newDraft(3)
	expired := claim(third, "summary:expired")
	if _, err := database.Pool.Exec(ctx, `UPDATE sync_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishProfileSummary(ctx, expired, third, "text-v1", "hash-v3", output, "openai"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("expired publish error = %v", err)
	}
	if _, err := repository.HandleTerminalProfileSummaryFailure(ctx, expired, third, "text-v1"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("expired terminal error = %v", err)
	}
}

func TestTerminalSummaryFailurePublishesFallbackOnlyOnFirstRun(t *testing.T) {
	database := openAIIntegrationStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(database)
	repository := NewSQLRepository(database)
	gameID := insertAIEvidenceGame(t, database, 27_001, "Achievement fact")
	buildAndClaim := func(version int, key string) (uuid.UUID, jobs.Lease) {
		t.Helper()
		profileID := uuid.New()
		if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata,publication_state) VALUES($1,$2,$3,'{"signalCounts":{"playtime":2}}','building')`, profileID, userID, version); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Pool.Exec(ctx, `INSERT INTO activity_profile_evidence(profile_id,game_id,achieved,total) VALUES($1,$2,1,1)`, profileID, gameID); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, ProfileID: profileID})
		if _, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: ProfileSummaryJobKind, IdempotencyKey: key, Payload: payload, UserID: userID}); err != nil {
			t.Fatal(err)
		}
		lease, err := queue.Claim(ctx, jobs.LaneInteractive, "terminal-worker", time.Minute)
		if err != nil || lease == nil {
			t.Fatalf("claim: %#v %v", lease, err)
		}
		return profileID, *lease
	}
	first, firstLease := buildAndClaim(1, "terminal:first")
	published, err := repository.HandleTerminalProfileSummaryFailure(ctx, firstLease, first, "text-v1")
	if err != nil || !published {
		t.Fatalf("first fallback published=%v err=%v", published, err)
	}
	if err := queue.Complete(ctx, firstLease); err != nil {
		t.Fatal(err)
	}
	second, secondLease := buildAndClaim(2, "terminal:second")
	published, err = repository.HandleTerminalProfileSummaryFailure(ctx, secondLease, second, "text-v1")
	if err != nil || published {
		t.Fatalf("later fallback published=%v err=%v", published, err)
	}
	var firstState, secondState, hash, summary string
	if err := database.Pool.QueryRow(ctx, `SELECT publication_state,input_content_hash,summary FROM preference_profiles WHERE id=$1`, first).Scan(&firstState, &hash, &summary); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `SELECT publication_state FROM preference_profiles WHERE id=$1`, second).Scan(&secondState); err != nil {
		t.Fatal(err)
	}
	if firstState != "published" || secondState != "failed" || strings.TrimSpace(hash) == "" || hash == "deterministic-terminal-v1" || !strings.Contains(summary, "достижениями — 1") {
		t.Fatalf("states/hash/summary = %q/%q/%q/%q", firstState, secondState, hash, summary)
	}
}

func TestSelectProfileEvidenceDeduplicatesAndUsesStableTieBreaks(t *testing.T) {
	id := func(value string) uuid.UUID { return uuid.MustParse("00000000-0000-0000-0000-" + value) }
	at := func(value time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: value, Valid: true} }
	old, recent := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC), time.Date(2021, time.January, 1, 0, 0, 0, 0, time.UTC)

	completion := profileEvidenceSignal{id: id("000000000001"), name: "Completion", completionBasis: "ending", completionWeight: 1.5}
	recentDuplicate := id("000000000002")
	achievement := profileEvidenceSignal{id: id("000000000003"), name: "Achievement", achieved: 10}
	newerHistory := profileEvidenceSignal{id: id("000000000005"), name: "Zulu", played: 50, lastPlayedAt: at(recent)}
	normalizedFirst := profileEvidenceSignal{id: id("000000000006"), name: " alpha ", played: 50, lastPlayedAt: at(old)}
	rawFirst := profileEvidenceSignal{id: id("000000000007"), name: "Beta", played: 50, lastPlayedAt: at(old)}
	rawSecond := profileEvidenceSignal{id: id("000000000008"), name: "beta", played: 50, lastPlayedAt: at(old)}
	uuidFirst := profileEvidenceSignal{id: id("000000000009"), name: "Gamma", played: 50, lastPlayedAt: at(old)}
	uuidSecond := profileEvidenceSignal{id: id("000000000010"), name: "Gamma", played: 50, lastPlayedAt: at(old)}

	for _, test := range []struct {
		name  string
		input []profileEvidenceSignal
		want  []uuid.UUID
	}{
		{
			name: "priority material value and deduplication",
			input: []profileEvidenceSignal{
				{id: recentDuplicate, name: "Recent duplicate history", played: 99_999},
				achievement,
				{id: recentDuplicate, name: "Recent duplicate", recent: 30},
				completion,
			},
			want: []uuid.UUID{completion.id, recentDuplicate, achievement.id},
		},
		{
			name:  "date normalized name raw name and UUID ties",
			input: []profileEvidenceSignal{uuidSecond, rawSecond, normalizedFirst, newerHistory, uuidFirst, rawFirst},
			want:  []uuid.UUID{newerHistory.id, normalizedFirst.id, rawFirst.id, rawSecond.id, uuidFirst.id, uuidSecond.id},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := selectProfileEvidence(test.input, 20)
			if len(selected) != len(test.want) {
				t.Fatalf("selected count = %d, want %d", len(selected), len(test.want))
			}
			for index := range test.want {
				if selected[index].id != test.want[index] {
					t.Fatalf("selected[%d] = %s (%q), want %s", index, selected[index].id, selected[index].name, test.want[index])
				}
			}
		})
	}
}

func insertAIEvidenceGame(t *testing.T, database *store.Store, appID int64, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := database.UpsertGame(context.Background(), store.Game{ID: id, SteamAppID: appID, Name: name, Source: "test", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertAIDirectFeedback(t *testing.T, database *store.Store, userID, profileID, gameID uuid.UUID, action string, createdAt time.Time) {
	t.Helper()
	recommendationID := uuid.New()
	if _, err := database.Pool.Exec(context.Background(), `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score) VALUES($1,$2,$3,$4,$5,1)`, recommendationID, userID, gameID, profileID, "test-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(context.Background(), `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,created_at) VALUES($1,$2,$3,$4,$5,$6)`, uuid.New(), recommendationID, userID, action, uuid.NewString(), createdAt); err != nil {
		t.Fatal(err)
	}
}

func openAIIntegrationStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ai_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolate := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	database, err := store.Open(ctx, isolate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup, err := pgx.Connect(cleanupCtx, baseURL)
		if err == nil {
			_, _ = cleanup.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(cleanupCtx)
		}
	})
	return database
}
