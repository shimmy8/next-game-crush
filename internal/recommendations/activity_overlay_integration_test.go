package recommendations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/store"
)

type feedbackGapRepository struct {
	*SQLRepository
	insert func() error
	done   bool
}

func (r *feedbackGapRepository) ResolveRankingInputs(ctx context.Context, request RankingRequest, limit int) (RankingInputs, error) {
	if !r.done {
		r.done = true
		if err := r.insert(); err != nil {
			return RankingInputs{}, err
		}
	}
	return r.SQLRepository.ResolveRankingInputs(ctx, request, limit)
}

func TestRetrievalUnionIncludesPositiveOverlayNeighborsOnly(t *testing.T) {
	database := openRankingStore(t)
	ctx := context.Background()
	userID, profileID, overlayID := uuid.New(), uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash,algorithm_metadata) VALUES($1,$2,1,$3,'union-overlay','union-hash','{}')`, overlayID, userID, "preference-overlay-v1"); err != nil {
		t.Fatal(err)
	}
	activity, positive, negative := make([]float32, profile.Dimensions), make([]float32, profile.Dimensions), make([]float32, profile.Dimensions)
	activity[0], positive[1], negative[2] = 1, 1, 1
	for polarity, vector := range map[string][]float32{"positive": positive, "negative": negative} {
		if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_overlay_embeddings(overlay_id,polarity,model,input_content_hash,embedding,source) VALUES($1,$2,'embed','union-hash',$3,'deterministic')`, overlayID, polarity, pgvector.NewVector(vector)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_overlay_embeddings(overlay_id,polarity,model,input_content_hash,embedding,source) VALUES($1,'positive','other-model','union-hash',$2,'deterministic')`, overlayID, pgvector.NewVector(activity)); err != nil {
		t.Fatal(err)
	}
	activityID := insertRankingGame(t, database, 12000, "Activity Match")
	positiveID := insertRankingGame(t, database, 12001, "Positive Overlay Match")
	negativeID := insertRankingGame(t, database, 12002, "Negative Only Match")
	ids := []uuid.UUID{activityID, positiveID, negativeID}
	for index := 0; index < ActivityNeighborLimit; index++ {
		ids = append(ids, insertRankingGame(t, database, int64(12100+index), fmt.Sprintf("Activity Filler %03d", index)))
	}
	for _, gameID := range ids {
		if _, err := database.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, gameID); err != nil {
			t.Fatal(err)
		}
		vector := activity
		if gameID == positiveID {
			vector = positive
		} else if gameID == negativeID {
			vector = negative
		}
		if err := database.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: gameID.String(), Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := NewSQLRepository(database, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	input, err := repository.RankingInputs(ctx, userID, profile.Profile{ID: profileID, UserID: userID, Version: 1, Positive: activity, PositiveStrength: 1}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Candidates) != len(ids) {
		t.Fatalf("union was not deduplicated: candidates=%d ids=%d", len(input.Candidates), len(ids))
	}
	byID := make(map[uuid.UUID]Candidate, len(input.Candidates))
	for _, candidate := range input.Candidates {
		byID[candidate.GameID] = candidate
	}
	activityLeg := 0
	for _, candidate := range input.Candidates {
		if candidate.retrievalRank <= ActivityNeighborLimit {
			activityLeg++
		}
	}
	if activityLeg != ActivityNeighborLimit || byID[positiveID].retrievalRank <= ActivityNeighborLimit || byID[positiveID].retrievalRank > ActivityNeighborLimit+PreferenceNeighborLimit {
		t.Fatalf("activity/positive legs have wrong size/rank: %d/%d", activityLeg, byID[positiveID].retrievalRank)
	}
	if byID[negativeID].retrievalRank <= ActivityNeighborLimit+PreferenceNeighborLimit {
		t.Fatalf("negative vector unexpectedly retrieved candidate: rank=%d", byID[negativeID].retrievalRank)
	}
	if byID[positiveID].Factors.PositivePreferenceSimilarity != 1 || byID[activityID].Factors.PositivePreferenceSimilarity != 0 {
		t.Fatalf("configured-model overlay factors positive=%v activity=%v", byID[positiveID].Factors.PositivePreferenceSimilarity, byID[activityID].Factors.PositivePreferenceSimilarity)
	}
}

func TestResolveSourceUsesExactBuildKeyElseAuthoritativeNewestState(t *testing.T) {
	database := openRankingStore(t)
	ctx := context.Background()
	userID, oldID, newestID, overlayID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	metadata := `{"algorithmVersion":"actual-profile-v9","confidence":0.7}`
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,publication_state,algorithm_metadata,build_key,input_content_hash) VALUES($1,$3,1,'building',$4,'old-build','old-profile-input'),($2,$3,2,'insufficient',$4,'new-build','new-profile-input')`, oldID, newestID, userID, metadata); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, profile.Dimensions)
	vector[0] = 1
	if _, err := database.Pool.Exec(ctx, `INSERT INTO profile_embeddings(profile_id,polarity,model,input_content_hash,embedding,source) VALUES($1,'positive','embed','old-input',$2,'test')`, oldID, pgvector.NewVector(vector)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash,algorithm_metadata) VALUES($1,$2,1,'preference-overlay-v1','overlay-build','overlay-input','{}')`, overlayID, userID); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed", "text")
	latest, err := repository.ResolveRankingInputs(ctx, RankingRequest{UserID: userID}, 10)
	if err != nil || latest.Source.Activity.ID != newestID || latest.Source.Activity.PublicationState != profile.PublicationInsufficient || len(latest.Source.Activity.Positive) != 0 {
		t.Fatalf("authoritative newest=%#v err=%v", latest.Source, err)
	}
	exact, err := repository.ResolveRankingInputs(ctx, RankingRequest{UserID: userID, ProfileBuildKey: "old-build"}, 10)
	if err != nil || exact.Source.Activity.ID != oldID || exact.Source.ProfileAlgorithmVersion != "actual-profile-v9" || exact.Source.ProfileBuildKey != "old-build" || exact.Source.ProfileInputContentHash != "old-input" {
		t.Fatalf("exact source=%#v err=%v", exact.Source, err)
	}
}

func TestSaveRankingRejectsNullableNewProvenance(t *testing.T) {
	database := openRankingStore(t)
	ctx := context.Background()
	userID, otherUser, profileID, overlayID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1),($2)`, userID, otherUser); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata,build_key) VALUES($1,$2,1,'{"algorithmVersion":"activity-real"}','profile-build')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash,algorithm_metadata) VALUES($1,$2,1,'overlay-real','overlay-build','overlay-input','{}')`, overlayID, userID); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed", "text")
	if _, _, _, err := repository.SaveRanking(ctx, userID, RankingSource{Activity: profile.Profile{ID: uuid.New()}}, nil, "invalid"); err == nil || !strings.Contains(err.Error(), "exact activity profile and preference overlay") {
		t.Fatalf("nullable provenance write err=%v", err)
	}
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	source, err := repository.persistedRankingSource(ctx, tx, userID, profileID, overlayID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	garbage := "pair-v2:" + strings.Repeat("0", 64) + ":" + AlgorithmVersion + ":" + strings.Repeat("f", 64)
	if _, _, _, err := repository.SaveRanking(ctx, userID, source, nil, garbage); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("garbage pair key err=%v", err)
	}
	fabricated := source
	fabricated.ProfileBuildKey = "fabricated"
	if _, _, _, err := repository.SaveRanking(ctx, userID, fabricated, nil, RankingBuildKey("fabricated", AlgorithmVersion, fabricated)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("fabricated metadata err=%v", err)
	}
	if _, _, _, err := repository.SaveRanking(ctx, otherUser, source, nil, RankingBuildKey("cross-user", AlgorithmVersion, source)); err == nil || !strings.Contains(err.Error(), "validate ranking source") {
		t.Fatalf("cross-user source err=%v", err)
	}
}

func TestActivityIdentityAndRankingKeyIgnoreMutableSummaryPublication(t *testing.T) {
	database := openRankingStore(t)
	ctx := context.Background()
	userID, profileID, overlayID := uuid.New(), uuid.New(), uuid.New()
	if _, err := database.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,publication_state,algorithm_metadata,build_key,input_content_hash) VALUES($1,$2,1,'building','{"algorithmVersion":"activity-stable"}','profile-build','mutable-summary-before')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, profile.Dimensions)
	vector[0] = 1
	if _, err := database.Pool.Exec(ctx, `INSERT INTO profile_embeddings(profile_id,polarity,model,input_content_hash,embedding,source) VALUES($1,'positive','embed','immutable-activity-input',$2,'deterministic')`, profileID, pgvector.NewVector(vector)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash,algorithm_metadata) VALUES($1,$2,1,'preference-overlay-v1','overlay-build','overlay-input','{}')`, overlayID, userID); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewSQLRepository(database, "embed", "text")
	before, err := repository.ResolveRankingInputs(ctx, RankingRequest{UserID: userID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	beforeKey := RankingBuildKey("same-request", AlgorithmVersion, before.Source)
	_, beforeVersion, _, err := repository.SaveRanking(ctx, userID, before.Source, nil, beforeKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE preference_profiles SET publication_state='published',summary='published summary',summary_source='deterministic',published_at=now(),input_content_hash='mutable-summary-after' WHERE id=$1`, profileID); err != nil {
		t.Fatal(err)
	}
	after, err := repository.ResolveRankingInputs(ctx, RankingRequest{UserID: userID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	afterKey := RankingBuildKey("same-request", AlgorithmVersion, after.Source)
	_, afterVersion, _, err := repository.SaveRanking(ctx, userID, after.Source, nil, afterKey)
	if err != nil {
		t.Fatal(err)
	}
	if before.Source.ProfileInputContentHash != "immutable-activity-input" || after.Source.ProfileInputContentHash != "immutable-activity-input" || beforeKey != afterKey || beforeVersion != afterVersion {
		t.Fatalf("summary publication changed activity identity before=%q/%q after=%q/%q", before.Source.ProfileInputContentHash, beforeKey, after.Source.ProfileInputContentHash, afterKey)
	}
}

func TestFeedbackCommitBetweenEnsureAndRankingSnapshotRetriesRealDatabasePair(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	base := service.repository.(*SQLRepository)
	ctx := context.Background()
	var gameID uuid.UUID
	if err := base.store.Pool.QueryRow(ctx, `SELECT game_id FROM recommendations WHERE id=$1`, old.id).Scan(&gameID); err != nil {
		t.Fatal(err)
	}
	wrapper := &feedbackGapRepository{SQLRepository: base}
	wrapper.insert = func() error {
		_, err := base.store.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,reasons,idempotency_key,created_at) VALUES($1,$2,$3,'like','{}',$4,now())`, uuid.New(), old.id, old.owner, uuid.NewString())
		return err
	}
	overlayRepository, err := preferences.NewSQLRepository(base.store)
	if err != nil {
		t.Fatal(err)
	}
	service.repository = wrapper
	service.overlays = preferenceRepositoryEnsurer{repository: overlayRepository}
	input, err := service.resolveCoherentInputs(ctx, old.owner, "feedback-gap")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := overlayRepository.LatestOverlay(ctx, old.owner)
	if err != nil {
		t.Fatal(err)
	}
	if input.Source.Overlay.ID != latest.ID || input.Source.Overlay.InputContentHash != latest.InputContentHash {
		t.Fatalf("stale overlay pair input=%s/%s latest=%s/%s", input.Source.Overlay.ID, input.Source.Overlay.InputContentHash, latest.ID, latest.InputContentHash)
	}
	found := false
	for _, candidate := range input.Candidates {
		if candidate.GameID == gameID {
			found = candidate.Factors.PositiveDirectFeedback == 1
		}
	}
	if !found {
		t.Fatalf("post-retry direct factor missing for %s", gameID)
	}
}

func TestRankingSnapshotBindsCoherentProfileOverlayPairDuringRace(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repository := service.repository.(*SQLRepository)
	ctx := context.Background()
	if _, _, _, err := service.rebuild(ctx, old.owner, "coherent-pair"); err != nil {
		t.Fatal(err)
	}
	var runProfile, runOverlay, rowProfile, rowOverlay uuid.UUID
	var buildKey string
	if err := repository.store.Pool.QueryRow(ctx, `SELECT profile_id,preference_overlay_id,build_key FROM recommendation_runs WHERE user_id=$1 AND ranking_version=2`, old.owner).Scan(&runProfile, &runOverlay, &buildKey); err != nil {
		t.Fatal(err)
	}
	if err := repository.store.Pool.QueryRow(ctx, `SELECT profile_id,preference_overlay_id FROM recommendations WHERE user_id=$1 AND ranking_version=2 LIMIT 1`, old.owner).Scan(&rowProfile, &rowOverlay); err != nil {
		t.Fatal(err)
	}
	if runProfile == uuid.Nil || runOverlay == uuid.Nil || runProfile != rowProfile || runOverlay != rowOverlay || !strings.HasPrefix(buildKey, "pair-v2:") {
		t.Fatalf("incoherent provenance run=%s/%s row=%s/%s key=%q", runProfile, runOverlay, rowProfile, rowOverlay, buildKey)
	}
	tx, err := repository.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	first, err := repository.resolveSource(ctx, tx, RankingRequest{UserID: old.owner}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	newProfile, newOverlay := uuid.New(), uuid.New()
	if _, err := repository.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,publication_state,algorithm_metadata,build_key) SELECT $1,$2,max(version)+1,'insufficient','{"algorithmVersion":"race-profile"}',$3 FROM preference_profiles WHERE user_id=$2`, newProfile, old.owner, "race-profile-build"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.store.Pool.Exec(ctx, `INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash,algorithm_metadata) SELECT $1,$2,max(version)+1,'preference-overlay-v1',$3,$4,'{}' FROM preference_overlays WHERE user_id=$2`, newOverlay, old.owner, "race-overlay-build", "race-overlay-input"); err != nil {
		t.Fatal(err)
	}
	stillFirst, err := repository.resolveSource(ctx, tx, RankingRequest{UserID: old.owner}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if stillFirst.Activity.ID != first.Activity.ID || stillFirst.Overlay.ID != first.Overlay.ID {
		t.Fatalf("repeatable-read mixed source pair first=%s/%s second=%s/%s", first.Activity.ID, first.Overlay.ID, stillFirst.Activity.ID, stillFirst.Overlay.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	newest, err := repository.ResolveRankingInputs(ctx, RankingRequest{UserID: old.owner}, 10)
	if err != nil || newest.Source.Activity.ID != newProfile || newest.Source.Overlay.ID != newOverlay {
		t.Fatalf("post-commit source=%s/%s err=%v", newest.Source.Activity.ID, newest.Source.Overlay.ID, err)
	}
	changed := newest.Source
	changedKey := RankingBuildKey("coherent-pair", AlgorithmVersion, changed)
	if _, _, _, err := repository.SaveRanking(ctx, old.owner, changed, nil, changedKey); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("same request accepted different source pair: %v", err)
	}
}
