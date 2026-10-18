package recommendations

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
	"next-game-crush/internal/ai"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/store"
)

func TestRebuildRetrievesSemanticAndWishlistCandidatesAndPersistsFactors(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	owned := insertRankingGame(t, db, 1, "Owned Signal")
	semantic := insertRankingGame(t, db, 2, "Semantic Match")
	wishlist := insertRankingGame(t, db, 3, "Wishlist Without Vector")
	released := insertRankingGame(t, db, 4, "Already Released")
	now := time.Now().UTC()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at) VALUES($1,$2,600,60,'test',$3)`, userID, owned, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at)VALUES($1,$2,5,10,'test',$3)`, userID, owned, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{owned, semantic} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_features(game_id,model,input_content_hash,features,source,last_successful_fetch_at)VALUES($1,'text',$2,'{"themes":["space"],"mechanics":["building"]}','test',$3)`, id, id.String(), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, wishlist, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{semantic, wishlist, released} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin)VALUES($1,'seed')`, id); err != nil {
			t.Fatal(err)
		}
	}
	future, past := now.AddDate(0, 1, 0), now.AddDate(0, -1, 0)
	for _, release := range []struct {
		id        uuid.UUID
		precision string
		date      any
		raw       string
	}{{semantic, "day", future, "future"}, {wishlist, "month", nil, "January 2027"}, {released, "day", past, "past"}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at)VALUES($1,$2,$3,$4,$5,$6,'test',$6)`, uuid.New(), release.id, release.precision, release.date, release.raw, now); err != nil {
			t.Fatal(err)
		}
	}
	vector := make([]float32, profile.Dimensions)
	vector[0] = 1
	for _, id := range []uuid.UUID{owned, semantic, released} {
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: id, Model: "embed", ContentHash: id.String(), Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}
	profileRepo, _ := profile.NewSQLRepository(db, "embed")
	profiles, _ := profile.NewService(profileRepo)
	built, err := profiles.Rebuild(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if built.Version != 1 {
		t.Fatalf("profile version=%d", built.Version)
	}
	retryOne, err := profileRepo.SaveProfile(ctx, userID, profile.Vectors{Positive: built.Positive, Negative: built.Negative, PositiveStrength: built.PositiveStrength, Confidence: built.Confidence, Evidence: built.EvidenceCount}, profile.AlgorithmVersion, "same-job")
	if err != nil {
		t.Fatal(err)
	}
	retryTwo, err := profileRepo.SaveProfile(ctx, userID, profile.Vectors{Positive: built.Positive, Negative: built.Negative, PositiveStrength: built.PositiveStrength, Confidence: built.Confidence, Evidence: built.EvidenceCount}, profile.AlgorithmVersion, "same-job")
	if err != nil || retryOne.ID != retryTwo.ID || retryOne.Version != retryTwo.Version {
		t.Fatalf("retry profiles=%#v %#v err=%v", retryOne, retryTwo, err)
	}
	repo, _ := NewSQLRepository(db, "embed", "text")
	service, _ := NewService(repo, profiles, fakeExplanationClient{err: ai.ErrRefusal})
	items, err := service.Rebuild(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("recommendations=%d (%#v), want semantic and wishlist", len(items), items)
	}
	for _, item := range items {
		if item.AlgorithmVersion != AlgorithmVersion || item.Explanation == "" || item.GeneratedExplanation {
			t.Fatalf("persisted recommendation=%#v", item)
		}
	}
	if items[0].Factors.TagAffinity == 0 && items[1].Factors.TagAffinity == 0 {
		t.Fatal("structured feature affinity was not computed")
	}
	var factors int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_factors`).Scan(&factors); err != nil || factors != 2 {
		t.Fatalf("factor rows=%d err=%v", factors, err)
	}
	calendar := BuildCalendar(items)
	if len(calendar.ExactDates) != 1 || len(calendar.Month) != 1 {
		t.Fatalf("calendar=%#v", calendar)
	}
	second, err := service.Rebuild(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].RankingVersion != 2 {
		t.Fatalf("second ranking=%#v", second)
	}
	latest, err := service.Latest(ctx, userID)
	if err != nil || len(latest) != 2 || latest[0].RankingVersion != 2 || len(latest[0].Evidence) == 0 || latest[0].Evidence[0].Text == "" {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
}

func TestCandidateFactorsDecayOldImplicitHistoryEndToEnd(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	candidate := insertRankingGame(t, db, 9000, "Matching Future Game")
	freshSignal := insertRankingGame(t, db, 9001, "Fresh Signal")
	oldSignal := insertRankingGame(t, db, 9002, "Old Signal")
	for _, id := range []uuid.UUID{candidate, freshSignal, oldSignal} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_features(game_id,model,input_content_hash,features,source,last_successful_fetch_at) VALUES($1,'text',$2,'{"themes":["strategy"],"mechanics":["teamplay"]}','test',$3)`, id, id.String(), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'day',$3,'future',$4,'test',$4)`, uuid.New(), candidate, now.AddDate(0, 1, 0), now); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, profile.Dimensions)
	vector[0] = 1
	for _, id := range []uuid.UUID{candidate, freshSignal, oldSignal} {
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: id, Model: "embed", ContentHash: id.String(), Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}

	buildFor := func(signalGame uuid.UUID, lastPlayed time.Time) Candidate {
		userID := uuid.New()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,source,last_successful_fetch_at) VALUES($1,$2,10000,0,$3,'test',$4)`, userID, signalGame, lastPlayed, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at) VALUES($1,$2,10,10,'test',$3)`, userID, signalGame, now); err != nil {
			t.Fatal(err)
		}
		profileRepo, err := profile.NewSQLRepository(db, "embed")
		if err != nil {
			t.Fatal(err)
		}
		profiles, err := profile.NewService(profileRepo)
		if err != nil {
			t.Fatal(err)
		}
		built, err := profiles.Rebuild(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := NewSQLRepository(db, "embed", "text")
		if err != nil {
			t.Fatal(err)
		}
		repository.now = func() time.Time { return now }
		candidates, err := repository.Candidates(ctx, userID, built, 10)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("candidates = %#v, err=%v", candidates, err)
		}
		return candidates[0]
	}

	fresh := buildFor(freshSignal, now)
	old := buildFor(oldSignal, now.Add(-10*365*24*time.Hour))
	if fresh.Factors.PositiveSimilarity <= old.Factors.PositiveSimilarity*30 {
		t.Fatalf("semantic similarity fresh/old = %v/%v", fresh.Factors.PositiveSimilarity, old.Factors.PositiveSimilarity)
	}
	for name, values := range map[string][2]float64{
		"tag":         {fresh.Factors.TagAffinity, old.Factors.TagAffinity},
		"playtime":    {fresh.Factors.PlaytimeAffinity, old.Factors.PlaytimeAffinity},
		"achievement": {fresh.Factors.AchievementAffinity, old.Factors.AchievementAffinity},
	} {
		if values[0] <= values[1]*30 {
			t.Fatalf("%s affinity fresh/old = %v/%v", name, values[0], values[1])
		}
	}
	if freshScore, oldScore := Score(fresh.Factors, DefaultWeights()).Score, Score(old.Factors, DefaultWeights()).Score; freshScore <= oldScore+40 {
		t.Fatalf("scores fresh/old = %v/%v, want material decay", freshScore, oldScore)
	}
}

func TestCompletionAffinityIsPersistentAndDoesNotDoubleCountAchievement(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	owner, profileID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, profileID, owner); err != nil {
		t.Fatal(err)
	}
	candidate := insertRankingGame(t, db, 9050, "Completion Candidate")
	completed := insertRankingGame(t, db, 9051, "Completed Anchor")
	ordinary := insertRankingGame(t, db, 9052, "Achievement Anchor")
	overridden := insertRankingGame(t, db, 9053, "Overridden Completion")
	oldCompleted := insertRankingGame(t, db, 9054, "Old Completion")
	for _, row := range []struct {
		id       uuid.UUID
		features string
	}{
		{candidate, `{"themes":["shared"],"mechanics":["candidate-only"]}`},
		{completed, `{"themes":["shared"]}`},
		{ordinary, `{"themes":["shared"],"mechanics":["candidate-only"]}`},
		{overridden, `{"themes":["shared"],"mechanics":["candidate-only"]}`},
		{oldCompleted, `{"themes":["shared"]}`},
	} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_features(game_id,model,input_content_hash,features,source,last_successful_fetch_at) VALUES($1,'text',$2,$3,'test',$4)`, row.id, row.id.String(), row.features, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		id   uuid.UUID
		last time.Time
	}{{completed, now}, {ordinary, now}, {overridden, now}, {oldCompleted, now.Add(-10 * 365 * 24 * time.Hour)}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,source,last_successful_fetch_at) VALUES($1,$2,0,0,$3,'test',$4)`, owner, row.id, row.last, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at) VALUES($1,$2,5,10,'test',$5),($1,$3,10,10,'test',$5),($1,$4,10,10,'test',$5)`, owner, ordinary, completed, overridden, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{completed, overridden, oldCompleted} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO user_game_completion_projections(user_id,game_id,ending_count,hundred_percent_basis,weight,basis,algorithm_version,updated_at) VALUES($1,$2,1,false,1.5,'ending','completion-v1',$3)`, owner, id, now); err != nil {
			t.Fatal(err)
		}
	}
	recommendationID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score) VALUES($1,$2,$3,$4,$5,0)`, recommendationID, owner, overridden, profileID, AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,reasons,idempotency_key,created_at) VALUES($1,$2,$3,'dislike','{}',$4,$5)`, uuid.New(), recommendationID, owner, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.affinities(ctx, tx, owner, candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Completion != .5 {
		t.Fatalf("completion affinity=%v, want .5 with fresh and ten-year-old anchors weighted equally", got.Completion)
	}
	if got.Achievement != 1 {
		t.Fatalf("achievement affinity=%v, want only ordinary progress anchor", got.Achievement)
	}
}

func TestExplanationCorpusLoadsConfiguredCandidateDataAndProfileSignals(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	owner, other := uuid.New(), uuid.New()
	for _, userID := range []uuid.UUID{owner, other} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
			t.Fatal(err)
		}
	}

	candidate := insertRankingGame(t, db, 9200, "Configured Candidate")
	missingEmbedding := insertRankingGame(t, db, 9201, "Missing Configured Embedding")
	recent := insertRankingGame(t, db, 9202, "Recent Anchor")
	old := insertRankingGame(t, db, 9203, "Old Anchor")
	unknownRecency := insertRankingGame(t, db, 9204, "Unknown Recency Anchor")
	wishlisted := insertRankingGame(t, db, 9205, "Wishlist Anchor")
	liked := insertRankingGame(t, db, 9206, "Liked Anchor")
	disliked := insertRankingGame(t, db, 9207, "Disliked Anchor")
	otherOnly := insertRankingGame(t, db, 9208, "Other User Anchor")
	missingFeatures := insertRankingGame(t, db, 9209, "Missing Configured Features")

	configuredVector := make([]float32, profile.Dimensions)
	configuredVector[0] = 1
	otherVector := make([]float32, profile.Dimensions)
	otherVector[1] = 1
	for _, gameID := range []uuid.UUID{candidate, recent, old, unknownRecency, wishlisted, liked, disliked, otherOnly, missingFeatures} {
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: gameID.String(), Vector: configuredVector}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: missingEmbedding, Model: "wrong-embed", ContentHash: missingEmbedding.String(), Vector: otherVector}); err != nil {
		t.Fatal(err)
	}

	featureRows := []struct {
		gameID uuid.UUID
		model  string
		value  string
	}{
		{candidate, "text", `{"themes":["horror"],"mechanics":["exploration"],"modes":["single-player"],"tone":["tense"],"evidence_keys":["private"]}`},
		{candidate, "wrong-text", `{"themes":["wrong"]}`},
		{missingEmbedding, "text", `{"themes":["configured-without-embedding"]}`},
		{missingFeatures, "wrong-text", `{"themes":["wrong-only"]}`},
		{recent, "text", `{"themes":["horror"]}`},
		{old, "text", `{"mechanics":["exploration"]}`},
		{unknownRecency, "text", `{"tone":["tense"]}`},
		{wishlisted, "text", `{"modes":["single-player"]}`},
		{liked, "text", `{"themes":["horror"]}`},
		{disliked, "text", `{"themes":["horror"]}`},
		{otherOnly, "text", `{"themes":["private"]}`},
	}
	for _, row := range featureRows {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_features(game_id,model,input_content_hash,features,source,last_successful_fetch_at) VALUES($1,$2,$3,$4,'test',$5)`, row.gameID, row.model, row.gameID.String()+row.model, row.value, now); err != nil {
			t.Fatal(err)
		}
	}

	for _, row := range []struct {
		gameID     uuid.UUID
		playtime   int64
		recent     int64
		lastPlayed any
	}{
		{recent, 600, 45, now.Add(-24 * time.Hour)},
		{old, 1200, 0, now.Add(-10 * 365 * 24 * time.Hour)},
		{unknownRecency, 300, 0, nil},
	} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,source,last_successful_fetch_at) VALUES($1,$2,$3,$4,$5,'test',$6)`, owner, row.gameID, row.playtime, row.recent, row.lastPlayed, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO achievement_summaries(user_id,game_id,achieved,total,source,last_successful_fetch_at) VALUES($1,$2,3,4,'test',$3)`, owner, recent, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, owner, wishlisted, now); err != nil {
		t.Fatal(err)
	}
	insertExplanationCorpusFeedback(t, db, owner, liked, []string{"like", "like"}, true)
	insertExplanationCorpusFeedback(t, db, owner, disliked, []string{"dislike"}, false)
	insertExplanationCorpusFeedback(t, db, other, otherOnly, []string{"like"}, true)

	if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at) VALUES($1,$2,99999,99999,'test',$3),($1,$4,88888,88888,'test',$3)`, other, recent, now, otherOnly); err != nil {
		t.Fatal(err)
	}

	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := repo.ExplanationCorpus(ctx, owner, []uuid.UUID{candidate, missingEmbedding, missingFeatures, candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(corpus.Candidates) != 3 {
		t.Fatalf("candidate count=%d: %#v", len(corpus.Candidates), corpus.Candidates)
	}
	complete := corpus.Candidates[candidate]
	if complete.GameID != candidate || len(complete.Vector) != profile.Dimensions || complete.Vector[0] != 1 || complete.Features.Themes[0] != "horror" || len(complete.Features.Mechanics) != 1 || len(complete.Features.Modes) != 1 || len(complete.Features.Tone) != 1 {
		t.Fatalf("configured candidate=%#v", complete)
	}
	withoutEmbedding := corpus.Candidates[missingEmbedding]
	if withoutEmbedding.GameID != missingEmbedding || len(withoutEmbedding.Vector) != 0 || len(withoutEmbedding.Features.Themes) != 1 || withoutEmbedding.Features.Themes[0] != "configured-without-embedding" {
		t.Fatalf("missing configured embedding=%#v", withoutEmbedding)
	}
	withoutFeatures := corpus.Candidates[missingFeatures]
	if withoutFeatures.GameID != missingFeatures || len(withoutFeatures.Vector) != profile.Dimensions || len(withoutFeatures.Features.Themes)+len(withoutFeatures.Features.Mechanics)+len(withoutFeatures.Features.Modes)+len(withoutFeatures.Features.Tone) != 0 {
		t.Fatalf("missing configured features=%#v", withoutFeatures)
	}

	anchors := make(map[uuid.UUID]EvidenceAnchor, len(corpus.Anchors))
	for _, anchor := range corpus.Anchors {
		anchors[anchor.Signal.GameID] = anchor
	}
	if len(anchors) != 6 {
		t.Fatalf("anchor count=%d: %#v", len(anchors), corpus.Anchors)
	}
	if _, ok := anchors[otherOnly]; ok {
		t.Fatal("another user's game leaked into explanation anchors")
	}
	if got := anchors[recent]; got.Name != "Recent Anchor" || got.Signal.PlaytimeMinutes != 600 || got.Signal.RecentMinutes != 45 || !got.Signal.LastPlayedAt.Equal(now.Add(-24*time.Hour)) || got.Signal.AchievementRatio != .75 || len(got.Features.Themes) != 1 || len(got.Signal.Vector) != profile.Dimensions {
		t.Fatalf("recent anchor=%#v", got)
	}
	if got := anchors[old]; !got.Signal.LastPlayedAt.Equal(now.Add(-10 * 365 * 24 * time.Hour)) {
		t.Fatalf("old anchor last played=%v", got.Signal.LastPlayedAt)
	}
	if got := anchors[unknownRecency]; !got.Signal.LastPlayedAt.IsZero() {
		t.Fatalf("unknown recency=%v", got.Signal.LastPlayedAt)
	}
	if got := anchors[wishlisted]; !got.Signal.Wishlisted {
		t.Fatalf("wishlist anchor=%#v", got)
	}
	if got := anchors[liked]; got.Signal.Likes != 2 || got.Signal.Dislikes != 0 {
		t.Fatalf("aggregated positive feedback=%#v", got.Signal)
	}
	if got := anchors[disliked]; got.Signal.Likes != 0 || got.Signal.Dislikes != 1 {
		t.Fatalf("negative feedback=%#v", got.Signal)
	}
	profileRepo, err := profile.NewSQLRepository(db, "embed")
	if err != nil {
		t.Fatal(err)
	}
	profileSignals, err := profileRepo.LoadSignals(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(profileSignals) != len(anchors) {
		t.Fatalf("profile/corpus signal counts=%d/%d", len(profileSignals), len(anchors))
	}
	for _, signal := range profileSignals {
		anchor, ok := anchors[signal.GameID]
		if !ok || !reflect.DeepEqual(anchor.Signal, signal) {
			t.Fatalf("corpus signal differs from profile signal for %s: corpus=%#v profile=%#v", signal.GameID, anchor.Signal, signal)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'day',$3,'future',$4,'test',$4)`, uuid.New(), candidate, now.AddDate(0, 1, 0), now); err != nil {
		t.Fatal(err)
	}
	repo.now = func() time.Time { return now }
	inputs, err := repo.RankingInputs(ctx, owner, profile.Profile{Positive: configuredVector, Confidence: 1}, 10)
	if err != nil || len(inputs.Candidates) == 0 {
		t.Fatalf("ranking snapshot candidates=%#v err=%v", inputs.Candidates, err)
	}
	snapshotCandidate := inputs.Corpus.Candidates[candidate]
	if snapshotCandidate.Features.Themes[0] != "horror" || len(inputs.Corpus.Anchors) != len(corpus.Anchors) {
		t.Fatalf("ranking snapshot corpus=%#v", inputs.Corpus)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE game_features SET features='{"themes":["mutated-after-snapshot"]}' WHERE game_id=$1 AND model='text'`, candidate); err != nil {
		t.Fatal(err)
	}
	if inputs.Corpus.Candidates[candidate].Features.Themes[0] != "horror" {
		t.Fatalf("captured corpus changed after its ranking snapshot: %#v", inputs.Corpus.Candidates[candidate])
	}

}

func TestExplanationCorpusEmptyInputDoesNotQueryPersonalRows(t *testing.T) {
	db := openRankingStore(t)
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	corpus, err := repo.ExplanationCorpus(ctx, uuid.New(), nil)
	if err != nil || len(corpus.Candidates) != 0 || len(corpus.Anchors) != 0 {
		t.Fatalf("empty corpus=%#v err=%v", corpus, err)
	}
}

func insertExplanationCorpusFeedback(t *testing.T, db *store.Store, userID, gameID uuid.UUID, actions []string, classifyFirstPositive bool) {
	t.Helper()
	ctx := context.Background()
	profileID, recommendationID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,(SELECT COALESCE(max(version),0)+1 FROM preference_profiles WHERE user_id=$2),'{}')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) VALUES($1,$2,$3,$4,'corpus-test',1,1)`, recommendationID, userID, gameID, profileID); err != nil {
		t.Fatal(err)
	}
	for index, action := range actions {
		feedbackID := uuid.New()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key) VALUES($1,$2,$3,$4,$5)`, feedbackID, recommendationID, userID, action, feedbackID.String()); err != nil {
			t.Fatal(err)
		}
		if index == 0 && classifyFirstPositive {
			if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash) VALUES($1,'genre','positive',1,true,'test',$2)`, feedbackID, feedbackID.String()); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestLatestAndGetExposeSteamPresentation(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	userID, profileID, gameID, recommendationID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	unknownGameID, unknownRecommendationID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	factors, _ := json.Marshal(Factors{Wishlist: 1, TagAffinity: 0.5, Confidence: 0.75})
	weights, _ := json.Marshal(DefaultWeights())
	legacyEvidence := []byte(`[{"id":"score","text":"deterministic score 75.000"},{"id":"factors","text":"{\"wishlist\":1}"},{"id":"release-window","text":"coming_soon:To be announced"}]`)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,620,'Portal 2','steam_store',$2,$3)`, []any{gameID, now, []byte(`{"HeaderImage":"https://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg"}`)}},
		{`INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, []any{profileID, userID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,$4,'presentation-test',true)`, []any{runID, userID, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score,explanation,explanation_evidence,explanation_generated) VALUES($1,$2,$3,$4,$5,1,75,$6,$7,false)`, []any{recommendationID, userID, gameID, profileID, AlgorithmVersion, `Recommended because of: deterministic score 75.000, {"wishlist":1}, coming_soon:To be announced.`, legacyEvidence}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, []any{recommendationID, factors, weights}},
		{`INSERT INTO game_compatibility(game_id,windows,mac,linux,platforms_last_successful_fetch_at,deck_status,deck_availability,deck_last_successful_fetch_at) VALUES($1,true,false,true,$2,'verified','available',$2)`, []any{gameID, now}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,621,'Unknown Compatibility','test',$2)`, []any{unknownGameID, now}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) VALUES($1,$2,$3,$4,$5,1,50)`, []any{unknownRecommendationID, userID, unknownGameID, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, []any{unknownRecommendationID, factors, weights}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Latest(ctx, userID)
	if err != nil || len(latest) != 2 {
		t.Fatalf("Latest = %#v, %v", latest, err)
	}
	assertSteamPresentation(t, latest[0])
	assertCompatibilityPresentation(t, latest[0], true)
	assertCompatibilityPresentation(t, latest[1], false)
	got, err := repo.Get(ctx, userID, recommendationID)
	if err != nil {
		t.Fatal(err)
	}
	assertSteamPresentation(t, got)
	assertCompatibilityPresentation(t, got, true)
	unknown, err := repo.Get(ctx, userID, unknownRecommendationID)
	if err != nil {
		t.Fatal(err)
	}
	assertCompatibilityPresentation(t, unknown, false)

	profileRepo, err := profile.NewSQLRepository(db, "embed")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewService(profileRepo)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, profiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	safeLatest, err := service.Latest(ctx, userID)
	if err != nil || len(safeLatest) != 2 {
		t.Fatalf("safe Latest = %#v, %v", safeLatest, err)
	}
	assertSafeLegacyPresentation(t, safeLatest[0])
	assertCompatibilityPresentation(t, safeLatest[0], true)
	assertCompatibilityPresentation(t, safeLatest[1], false)
	safeGet, err := service.Get(ctx, userID, recommendationID)
	if err != nil {
		t.Fatal(err)
	}
	assertSafeLegacyPresentation(t, safeGet)
	assertCompatibilityPresentation(t, safeGet, true)
	safeUnknown, err := service.Get(ctx, userID, unknownRecommendationID)
	if err != nil {
		t.Fatal(err)
	}
	assertCompatibilityPresentation(t, safeUnknown, false)
}

func TestLatestAndGetExposeLatestDirectFeedbackAcrossRankingVersions(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	userID, profileID, gameID := uuid.New(), uuid.New(), uuid.New()
	oldRecommendationID, currentRecommendationID := uuid.New(), uuid.New()
	dislikeID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	likeID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	now := time.Now().UTC()
	factors, _ := json.Marshal(Factors{Confidence: 1})
	weights, _ := json.Marshal(DefaultWeights())
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,700,'Reaction Game','test',$2)`, []any{gameID, now}},
		{`INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, []any{profileID, userID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,$4,'reaction-v1',true),($5,$2,$3,2,$4,'reaction-v2',true)`, []any{uuid.New(), userID, profileID, AlgorithmVersion, uuid.New()}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) VALUES($1,$2,$3,$4,$5,1,50),($6,$2,$3,$4,$5,2,60)`, []any{oldRecommendationID, userID, gameID, profileID, AlgorithmVersion, currentRecommendationID}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$3,$4),($2,$3,$4)`, []any{oldRecommendationID, currentRecommendationID, factors, weights}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,reason,reasons,idempotency_key,created_at) VALUES($1,$3,$4,'dislike','length',ARRAY['length'],'reaction-dislike',$5),($2,$3,$4,'like','genre',ARRAY['genre','setting'],'reaction-like',$5),($6,$3,$4,'remind_me',NULL,'{}','reaction-remind',$7)`, []any{dislikeID, likeID, oldRecommendationID, userID, now, uuid.New(), now.Add(time.Second)}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Latest(ctx, userID)
	if err != nil || len(latest) != 1 || latest[0].LatestFeedbackAction == nil || *latest[0].LatestFeedbackAction != "like" {
		t.Fatalf("latest feedback = %#v, %v", latest, err)
	}
	if !reflect.DeepEqual(latest[0].LatestFeedbackReasons, []string{"genre", "setting"}) {
		t.Fatalf("latest feedback reasons = %v", latest[0].LatestFeedbackReasons)
	}
	got, err := repo.Get(ctx, userID, currentRecommendationID)
	if err != nil || got.LatestFeedbackAction == nil || *got.LatestFeedbackAction != "like" {
		t.Fatalf("get feedback = %#v, %v", got.LatestFeedbackAction, err)
	}
	if !reflect.DeepEqual(got.LatestFeedbackReasons, []string{"genre", "setting"}) {
		t.Fatalf("get feedback reasons = %v", got.LatestFeedbackReasons)
	}
}

func TestLatestSuppressesPublishedRecommendationWhileFeedbackRebuildIsPending(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	userID, profileID, gameID, recommendationID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	factors, _ := json.Marshal(Factors{Confidence: 1})
	weights, _ := json.Marshal(DefaultWeights())
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,701,'Published Game','test',$2)`, []any{gameID, now}},
		{`INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, []any{profileID, userID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,$4,'published-before-feedback',true)`, []any{uuid.New(), userID, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) VALUES($1,$2,$3,$4,$5,1,50)`, []any{recommendationID, userID, gameID, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, []any{recommendationID, factors, weights}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,reason,reasons,idempotency_key,created_at) VALUES($1,$2,$3,'dislike','length',ARRAY['length'],'pending-dislike',$4)`, []any{uuid.New(), recommendationID, userID, now.Add(time.Second)}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Latest(ctx, userID)
	if err != nil || len(latest) != 0 {
		t.Fatalf("published latest retained a currently disliked game: %#v, %v", latest, err)
	}
}

func TestLatestLegacyExcludeIsNeutralToReminderAndReversedByLaterLike(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	userID, profileID, gameID, recommendationID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	factors, _ := json.Marshal(Factors{Confidence: 1})
	weights, _ := json.Marshal(DefaultWeights())
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,702,'Legacy Excluded Game','test',$2)`, []any{gameID, now}},
		{`INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, []any{profileID, userID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,$4,'published-before-legacy-exclude',true)`, []any{uuid.New(), userID, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) VALUES($1,$2,$3,$4,$5,1,50)`, []any{recommendationID, userID, gameID, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, []any{recommendationID, factors, weights}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,reasons,idempotency_key,created_at) VALUES($1,$2,$3,'exclude','{}',$4,$5),($6,$2,$3,'remind_me','{}',$7,$8)`, []any{uuid.New(), recommendationID, userID, uuid.NewString(), now, uuid.New(), uuid.NewString(), now.Add(time.Second)}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Latest(ctx, userID)
	if err != nil || len(latest) != 0 {
		t.Fatalf("reminder replaced legacy exclusion: latest=%#v err=%v", latest, err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,reasons,idempotency_key,created_at) VALUES($1,$2,$3,'like','{}',$4,$5)`, uuid.New(), recommendationID, userID, uuid.NewString(), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	latest, err = repo.Latest(ctx, userID)
	if err != nil || len(latest) != 1 || latest[0].GameID != gameID || latest[0].LatestFeedbackAction == nil || *latest[0].LatestFeedbackAction != "like" {
		t.Fatalf("later like did not restore legacy-excluded game: latest=%#v err=%v", latest, err)
	}
}

func TestCandidatesUseLatestDirectActionAcrossSources(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	userID, profileID := uuid.New(), uuid.New()
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, profileID, userID); err != nil {
		t.Fatal(err)
	}

	type fixture struct {
		gameID uuid.UUID
		recID  uuid.UUID
	}
	fixtures := map[string]fixture{}
	for appID, name := range map[int64]string{
		7201: "Catalog direct action",
		7202: "Wishlist direct action",
		7203: "Legacy exclude action",
		7204: "Equal timestamp action",
	} {
		gameID := insertRankingGame(t, db, appID, name)
		recID := uuid.New()
		fixtures[name] = fixture{gameID: gameID, recID: recID}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score) VALUES($1,$2,$3,$4,$5,0)`, recID, userID, gameID, profileID, AlgorithmVersion); err != nil {
			t.Fatal(err)
		}
		if name == "Wishlist direct action" {
			if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, gameID, now); err != nil {
				t.Fatal(err)
			}
		} else if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, gameID); err != nil {
			t.Fatal(err)
		}
	}
	vector := make([]float32, profile.Dimensions)
	vector[0] = 1
	for name, fixture := range fixtures {
		if name == "Wishlist direct action" {
			continue
		}
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: fixture.gameID, Model: "embed", ContentHash: fixture.gameID.String(), Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}

	insertAction := func(id uuid.UUID, name, action string, at time.Time) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,reasons,idempotency_key,created_at) VALUES($1,$2,$3,$4,'{}',$5,$6)`, id, fixtures[name].recID, userID, action, id.String(), at); err != nil {
			t.Fatal(err)
		}
	}
	insertAction(uuid.New(), "Catalog direct action", "dislike", now)
	insertAction(uuid.New(), "Catalog direct action", "remind_me", now.Add(time.Second))
	insertAction(uuid.New(), "Wishlist direct action", "dislike", now)
	insertAction(uuid.New(), "Legacy exclude action", "exclude", now)
	insertAction(uuid.MustParse("00000000-0000-0000-0000-000000000001"), "Equal timestamp action", "dislike", now)
	insertAction(uuid.MustParse("00000000-0000-0000-0000-000000000002"), "Equal timestamp action", "like", now)

	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	load := func() map[string]Candidate {
		t.Helper()
		candidates, err := repo.Candidates(ctx, userID, profile.Profile{Positive: vector, PositiveStrength: 1}, 20)
		if err != nil {
			t.Fatal(err)
		}
		byName := make(map[string]Candidate, len(candidates))
		for _, candidate := range candidates {
			byName[candidate.Name] = candidate
		}
		return byName
	}

	initial := load()
	for _, hidden := range []string{"Catalog direct action", "Wishlist direct action", "Legacy exclude action"} {
		if _, ok := initial[hidden]; ok {
			t.Fatalf("%q remained eligible after current negative direct action", hidden)
		}
	}
	if _, ok := initial["Equal timestamp action"]; !ok {
		t.Fatal("higher-ID like at the same timestamp did not restore eligibility")
	}

	for _, name := range []string{"Catalog direct action", "Wishlist direct action", "Legacy exclude action"} {
		insertAction(uuid.New(), name, "like", now.Add(2*time.Second))
	}
	restored := load()
	for _, name := range []string{"Catalog direct action", "Wishlist direct action", "Legacy exclude action"} {
		candidate, ok := restored[name]
		if !ok {
			t.Fatalf("%q did not become eligible after later like", name)
		}
		if candidate.Dislikes == 0 || candidate.Factors.NegativeFeedback == 0 {
			t.Fatalf("%q lost historical negative evidence: %#v", name, candidate)
		}
	}
}

func assertCompatibilityPresentation(t *testing.T, got Recommendation, known bool) {
	t.Helper()
	if known {
		if got.Platforms == nil || *got.Platforms != (Platforms{Windows: true, Mac: false, Linux: true}) || got.DeckCompatibility != DeckVerified {
			t.Fatalf("known compatibility = %#v / %q", got.Platforms, got.DeckCompatibility)
		}
	} else if got.Platforms != nil || got.DeckCompatibility != DeckUnknown {
		t.Fatalf("unknown compatibility = %#v / %q", got.Platforms, got.DeckCompatibility)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["platforms"]; present != known {
		t.Fatalf("platforms JSON presence=%v, want %v", present, known)
	}
	if string(body["deckCompatibility"]) != `"`+string(got.DeckCompatibility)+`"` {
		t.Fatalf("deckCompatibility JSON=%s", body["deckCompatibility"])
	}
}

func assertSteamPresentation(t *testing.T, got Recommendation) {
	t.Helper()
	if got.SteamAppID != 620 || got.CoverImageURL != "https://cdn.cloudflare.steamstatic.com/steam/apps/620/header.jpg" || got.StoreURL != "https://store.steampowered.com/app/620/" {
		t.Fatalf("steam presentation = %#v", got)
	}
}

func assertSafeLegacyPresentation(t *testing.T, got Recommendation) {
	t.Helper()
	public := strings.ToLower(got.Explanation)
	for _, forbidden := range []string{"deterministic score", "{", "wishlist\"", "coming_soon", "to be announced"} {
		if strings.Contains(public, forbidden) {
			t.Fatalf("safe explanation leaks %q: %q", forbidden, got.Explanation)
		}
	}
	if len(got.Evidence) != 0 || got.GeneratedExplanation || got.ExplanationSource != ExplanationSourceNone || got.Explanation != neutralExplanation {
		t.Fatalf("safe evidence = %#v generated=%v", got.Evidence, got.GeneratedExplanation)
	}
	assertSteamPresentation(t, got)
}

func TestExplanationCacheIsUserScopedAndFencedAfterDeletion(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	firstUser, secondUser := uuid.New(), uuid.New()
	gameID := insertRankingGame(t, db, 9100, "Shared recommendation")
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1),($2)`, firstUser, secondUser); err != nil {
		t.Fatal(err)
	}
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	first := ai.Explanation{Text: "Первое личное объяснение.", EvidenceKeys: []string{"feature-overlap-1"}}
	second := ai.Explanation{Text: "Второе личное объяснение.", EvidenceKeys: []string{"wishlist-1"}}
	if err := repo.PutExplanationByInput(ctx, firstUser, gameID, "model", "hash", first); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutExplanationByInput(ctx, secondUser, gameID, "model", "hash", second); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		user uuid.UUID
		want ai.Explanation
	}{{firstUser, first}, {secondUser, second}} {
		got, ok, err := repo.GetExplanationByInput(ctx, test.user, gameID, "model", "hash")
		if err != nil || !ok || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("cache user=%s got=%#v ok=%v err=%v", test.user, got, ok, err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO deleted_user_tombstones(user_id) VALUES($1)`, firstUser); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, firstUser); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutExplanationByInput(ctx, firstUser, gameID, "model", "late", first); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late cache write error=%v, want pgx.ErrNoRows", err)
	}
	if _, ok, err := repo.GetExplanationByInput(ctx, firstUser, gameID, "model", "hash"); err != nil || ok {
		t.Fatalf("deleted user cache ok=%v err=%v", ok, err)
	}
	got, ok, err := repo.GetExplanationByInput(ctx, secondUser, gameID, "model", "hash")
	if err != nil || !ok || !reflect.DeepEqual(got, second) {
		t.Fatalf("surviving cache got=%#v ok=%v err=%v", got, ok, err)
	}
}

func insertRankingGame(t *testing.T, db *store.Store, appID int64, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,$2,$3,'test',now())`, id, appID, name); err != nil {
		t.Fatal(err)
	}
	return id
}

func openRankingStore(t *testing.T) *store.Store {
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
