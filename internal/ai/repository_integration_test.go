package ai

import (
	"context"
	"encoding/json"
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
	worker := jobs.NewWorker(queue, "ai-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, GameID: gameID})
	jobID, err := queue.Enqueue(ctx, jobs.Spec{Kind: FeaturesJobKind, IdempotencyKey: "ai.features:v1:test", Payload: payload, MaxAttempts: 3})
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
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at,last_played_at) VALUES($1,$2,100000,0,'test',now(),$3)`, userID, gameID, lastPlayed); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, profileID, userID); err != nil {
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
	for index := 0; index < 101; index++ {
		gameID := uuid.New()
		if err := database.UpsertGame(ctx, store.Game{ID: gameID, SteamAppID: int64(10_000 + index), Name: fmt.Sprintf("History %03d", index), Source: "test", FetchedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at,last_played_at) VALUES($1,$2,$3,0,'test',now(),$4)`, userID, gameID, 10_000-index, time.Date(2020, time.January, 1, 0, 0, index, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}

	completedID := insertAIEvidenceGame(t, database, 20_001, "Completed Story")
	overriddenID := insertAIEvidenceGame(t, database, 20_002, "Completed But Disliked")
	recentID := insertAIEvidenceGame(t, database, 20_003, "Two Week Activity")
	feedbackID := insertAIEvidenceGame(t, database, 20_004, "Standalone Like")
	wishlistID := insertAIEvidenceGame(t, database, 20_005, "Wishlist Intent")
	completionAt := time.Date(2018, time.May, 4, 12, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,completion_at,basis,algorithm_version) VALUES($1,$2,2,true,1.85,$3,'both','completion-v1'),($1,$4,1,false,1.5,NULL,'ending','completion-v1')`, userID, completedID, completionAt, overriddenID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at,last_played_at) VALUES($1,$2,15,240,'test',now(),now())`, userID, recentID); err != nil {
		t.Fatal(err)
	}
	insertAIDirectFeedback(t, database, userID, profileID, overriddenID, "like", time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	insertAIDirectFeedback(t, database, userID, profileID, overriddenID, "dislike", time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC))
	insertAIDirectFeedback(t, database, userID, profileID, feedbackID, "like", time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC))
	if _, err := database.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, userID, wishlistID); err != nil {
		t.Fatal(err)
	}

	repository := NewSQLRepository(database)
	evidence, err := repository.LoadProfileEvidence(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Evidence) != 100 {
		t.Fatalf("evidence count = %d, want 100", len(evidence.Evidence))
	}
	again, err := repository.LoadProfileEvidence(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence, again) {
		t.Fatal("profile evidence order is not stable across reads")
	}
	indexes := map[string]int{}
	values := map[string]string{}
	joined := strings.Builder{}
	for index, item := range evidence.Evidence {
		joined.WriteString(item.Value)
		joined.WriteByte('\n')
		for _, name := range []string{"Completed Story", "Completed But Disliked", "Two Week Activity", "Standalone Like", "Wishlist Intent", "History"} {
			if strings.Contains(item.Value, name) {
				if _, found := indexes[name]; !found {
					indexes[name], values[name] = index, item.Value
				}
			}
		}
	}
	if !(indexes["Completed Story"] < indexes["Two Week Activity"] && indexes["Completed But Disliked"] < indexes["Two Week Activity"] && indexes["Two Week Activity"] < indexes["Standalone Like"] && indexes["Standalone Like"] < indexes["Wishlist Intent"] && indexes["Wishlist Intent"] < indexes["History"]) {
		t.Fatalf("tier indexes = %#v", indexes)
	}
	for _, required := range []string{"completion_basis=both", "ending_count=2", "completion_at=2018-05-04T12:00:00Z", "completion_overridden=false"} {
		if !strings.Contains(values["Completed Story"], required) {
			t.Fatalf("completed evidence missing %q: %s", required, values["Completed Story"])
		}
	}
	for _, required := range []string{"completion_basis=ending", "ending_count=1", "completion_at=unknown", "completion_overridden=true", "positive_feedback=false"} {
		if !strings.Contains(values["Completed But Disliked"], required) {
			t.Fatalf("overridden evidence missing %q: %s", required, values["Completed But Disliked"])
		}
	}
	if strings.Contains(joined.String(), "completion_weight") || strings.Contains(joined.String(), completedID.String()) || strings.Contains(joined.String(), overriddenID.String()) {
		t.Fatalf("unsafe profile evidence: %s", joined.String())
	}
	if strings.Count(joined.String(), "History ") >= 101 {
		t.Fatal("low-priority history was not clipped after selection")
	}
}

func TestSelectProfileEvidenceDeduplicatesAndUsesStableTieBreaks(t *testing.T) {
	id := func(value string) uuid.UUID { return uuid.MustParse("00000000-0000-0000-0000-" + value) }
	at := func(value time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: value, Valid: true} }
	old, recent := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC), time.Date(2021, time.January, 1, 0, 0, 0, 0, time.UTC)

	completion := profileEvidenceSignal{id: id("000000000001"), name: "Completion", completionBasis: "ending", completionWeight: 1.5}
	recentDuplicate := id("000000000002")
	feedback := profileEvidenceSignal{id: id("000000000003"), name: "Feedback", feedbackAction: "dislike"}
	wishlist := profileEvidenceSignal{id: id("000000000004"), name: "Wishlist", wishlisted: true}
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
				wishlist,
				{id: recentDuplicate, name: "Recent duplicate history", played: 99_999},
				feedback,
				{id: recentDuplicate, name: "Recent duplicate", recent: 30},
				completion,
			},
			want: []uuid.UUID{completion.id, recentDuplicate, feedback.id, wishlist.id},
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
