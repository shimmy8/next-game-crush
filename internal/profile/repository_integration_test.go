package profile

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/store"
)

func TestLoadActivitySignalsExcludesWishlistFeedbackAndUnplayedOwnership(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 8)
	for i := range ids {
		ids[i] = uuid.New()
		if _, err := database.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,$2,'game','test',now())`, ids[i], 1000+i); err != nil {
			t.Fatal(err)
		}
		if err := database.PutGameEmbedding(ctx, store.GameEmbedding{GameID: ids[i], Model: "embed", ContentHash: ids[i].String(), Vector: unitVector(i)}); err != nil {
			t.Fatal(err)
		}
	}
	played, recent, achieved, completed, unplayed, wishlistOnly, feedbackOnly := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	for _, row := range []struct {
		id             uuid.UUID
		played, recent int
	}{{played, 10, 0}, {recent, 0, 5}, {achieved, 0, 0}, {unplayed, 0, 0}} {
		if _, err := database.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at) VALUES($1,$2,$3,$4,'test',now())`, userID, row.id, row.played, row.recent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at) VALUES($1,$2,1,10,'test',now())`, userID, achieved); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,basis,algorithm_version,updated_at) VALUES($1,$2,1,false,1.85,'ending','completion-v1',now())`, userID, completed); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, userID, wishlistOnly); err != nil {
		t.Fatal(err)
	}
	profileID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,publication_state) VALUES($1,$2,1,'failed')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	for _, gameID := range []uuid.UUID{feedbackOnly, completed} {
		recommendationID := uuid.New()
		if _, err := database.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score) VALUES($1,$2,$3,$4,'test',1)`, recommendationID, userID, gameID, profileID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key) VALUES($1,$2,$3,'dislike',$4)`, uuid.New(), recommendationID, userID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}

	repository, err := NewSQLRepository(database, "embed")
	if err != nil {
		t.Fatal(err)
	}
	signals, err := repository.LoadActivitySignals(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 4 {
		t.Fatalf("signals = %#v, want four activity games", signals)
	}
	seen := map[uuid.UUID]Signal{}
	for _, signal := range signals {
		seen[signal.GameID] = signal
	}
	for _, id := range []uuid.UUID{played, recent, achieved, completed} {
		if _, ok := seen[id]; !ok {
			t.Fatalf("eligible game %s missing", id)
		}
	}
	for _, id := range []uuid.UUID{unplayed, wishlistOnly, feedbackOnly, ids[7]} {
		if _, ok := seen[id]; ok {
			t.Fatalf("ineligible game %s loaded", id)
		}
	}
	if seen[completed].CompletionWeight != 1.85 {
		t.Fatalf("disliked completion = %#v", seen[completed])
	}
}

func TestBuildCreatesBuildingDraftAndQueuesSummary(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID, gameID, workflowID := uuid.New(), uuid.New(), uuid.New()
	seedActivityGame(t, database, userID, gameID, true)
	completionAt := time.Date(2026, time.October, 17, 8, 30, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at) VALUES($1,$2,3,10,'test',now())`, userID, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,completion_at,basis,algorithm_version,updated_at) VALUES($1,$2,2,false,1.85,$3,'ending','completion-v1',now())`, userID, gameID, completionAt); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed")
	service, _ := NewService(repository)
	result, err := service.Build(ctx, BuildRequest{UserID: userID, SourceWorkflowID: workflowID, BuildKey: "activity-build"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Profile.PublicationState != PublicationBuilding || !result.SummaryRequired {
		t.Fatalf("result = %#v", result)
	}
	var evidence, positive, negative int
	if err := database.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_profile_evidence WHERE profile_id=$1),(SELECT count(*) FROM profile_embeddings WHERE profile_id=$1 AND polarity='positive'),(SELECT count(*) FROM profile_embeddings WHERE profile_id=$1 AND polarity='negative')`, result.Profile.ID).Scan(&evidence, &positive, &negative); err != nil {
		t.Fatal(err)
	}
	if evidence != 1 || positive != 1 || negative != 0 {
		t.Fatalf("evidence/positive/negative = %d/%d/%d", evidence, positive, negative)
	}
	var playtime, recent int64
	var achieved, total, endingCount int32
	var lastPlayedAt, storedCompletionAt time.Time
	var hundredPercent bool
	var completionWeight float64
	var basis, completionAlgorithm string
	if err := database.Pool.QueryRow(ctx, `SELECT playtime_minutes,recently_played_minutes,last_played_at,achieved,total,completion_ending_count,completion_hundred_percent_basis,completion_weight,completion_at,completion_basis,completion_algorithm_version FROM activity_profile_evidence WHERE profile_id=$1 AND game_id=$2`, result.Profile.ID, gameID).Scan(&playtime, &recent, &lastPlayedAt, &achieved, &total, &endingCount, &hundredPercent, &completionWeight, &storedCompletionAt, &basis, &completionAlgorithm); err != nil {
		t.Fatal(err)
	}
	if playtime != 120 || recent != 30 || lastPlayedAt.IsZero() || achieved != 3 || total != 10 || endingCount != 2 || hundredPercent || completionWeight != 1.85 || !storedCompletionAt.Equal(completionAt) || basis != "ending" || completionAlgorithm != CompletionAlgorithmVersion {
		t.Fatalf("evidence snapshot mismatch: play=%d recent=%d last=%s achievements=%d/%d completion=%d/%v/%v/%s/%s/%s", playtime, recent, lastPlayedAt, achieved, total, endingCount, hundredPercent, completionWeight, storedCompletionAt, basis, completionAlgorithm)
	}
	var metadata []byte
	if err := database.Pool.QueryRow(ctx, `SELECT algorithm_metadata FROM preference_profiles WHERE id=$1`, result.Profile.ID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatal(err)
	}
	if values["sourceWorkflowId"] != workflowID.String() || values["algorithmVersion"] != AlgorithmVersion {
		t.Fatalf("metadata = %s", metadata)
	}
	if strings.Contains(string(metadata), userID.String()) || strings.Contains(string(metadata), gameID.String()) {
		t.Fatalf("metadata leaks user/game identity: %s", metadata)
	}
	wantCounts := map[string]any{"playtime": float64(1), "recent": float64(1), "achievements": float64(1), "completion": float64(1)}
	if !reflect.DeepEqual(values["signalCounts"], wantCounts) {
		t.Fatalf("signal counts metadata = %#v, want %#v", values["signalCounts"], wantCounts)
	}
	replayed, err := service.Build(ctx, BuildRequest{UserID: userID, SourceWorkflowID: workflowID, BuildKey: "activity-build"})
	if err != nil || replayed.Profile.ID != result.Profile.ID || replayed.Profile.Version != result.Profile.Version {
		t.Fatalf("replay = %#v err=%v", replayed, err)
	}
}

func TestActivityWithoutEmbeddingCreatesBuildingDraftWithoutProfileVector(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID, gameID := uuid.New(), uuid.New()
	seedActivityGame(t, database, userID, gameID, false)
	repository, _ := NewSQLRepository(database, "embed")
	service, _ := NewService(repository)
	result, err := service.Build(ctx, BuildRequest{UserID: userID, SourceWorkflowID: uuid.New(), BuildKey: "missing-vector"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Profile.PublicationState != PublicationBuilding || !result.SummaryRequired || result.Profile.EvidenceCount != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertSnapshotCounts(t, database, result.Profile.ID, 1, 0)
}

func TestBuildCreatesInsufficientWithoutEmbeddingOrSummaryJob(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed")
	service, _ := NewService(repository)
	result, err := service.Build(ctx, BuildRequest{UserID: userID, SourceWorkflowID: uuid.New(), BuildKey: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Profile.PublicationState != PublicationInsufficient || result.SummaryRequired {
		t.Fatalf("result = %#v", result)
	}
	assertSnapshotCounts(t, database, result.Profile.ID, 0, 0)
}

func TestBuildDraftIsDeletionFenced(t *testing.T) {
	database := openProfileStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO deleted_user_tombstones(user_id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed")
	service, _ := NewService(repository)
	if _, err := service.Build(ctx, BuildRequest{UserID: userID, SourceWorkflowID: uuid.New(), BuildKey: "deleted"}); err == nil {
		t.Fatal("deleted user profile build succeeded")
	}
	var profiles int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM preference_profiles WHERE user_id=$1`, userID).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if profiles != 0 {
		t.Fatalf("deleted user profiles = %d, want 0", profiles)
	}
}

func assertSnapshotCounts(t *testing.T, database *store.Store, profileID uuid.UUID, wantEvidence, wantEmbeddings int) {
	t.Helper()
	var evidence, embeddings int
	if err := database.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM activity_profile_evidence WHERE profile_id=$1),(SELECT count(*) FROM profile_embeddings WHERE profile_id=$1)`, profileID).Scan(&evidence, &embeddings); err != nil {
		t.Fatal(err)
	}
	if evidence != wantEvidence || embeddings != wantEmbeddings {
		t.Fatalf("evidence/embeddings = %d/%d, want %d/%d", evidence, embeddings, wantEvidence, wantEmbeddings)
	}
}

func seedActivityGame(t *testing.T, database *store.Store, userID, gameID uuid.UUID, embedding bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,4242,'Activity','test',now())`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,source,last_successful_fetch_at) VALUES($1,$2,120,30,$3,'test',now())`, userID, gameID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if embedding {
		if err := database.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: "activity", Vector: unitVector(0)}); err != nil {
			t.Fatal(err)
		}
	}
}
