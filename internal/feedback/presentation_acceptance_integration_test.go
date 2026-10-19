package feedback

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
	"next-game-crush/internal/store"
)

type allowAllFeedbackEligibility struct{}

func (allowAllFeedbackEligibility) Eligible(context.Context, uuid.UUID, time.Time) (bool, error) {
	return true, nil
}
func (allowAllFeedbackEligibility) FilterEligibleGameIDs(_ context.Context, ids []uuid.UUID, _ time.Time) (map[uuid.UUID]bool, error) {
	result := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func feedbackTestClock() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

func TestFeedbackReranksCollectionsAndExcludesOnlyCurrentDislike(t *testing.T) {
	fixture := newPresentationAcceptanceFixture(t)
	ctx := context.Background()
	before, err := fixture.ranking.Overview(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	beforeMembership := overviewMembership(before)

	queue := jobs.NewQueue(fixture.db)
	classifier := fakeClassifier{output: ai.FeedbackClassification{
		Taxonomy: "other", Polarity: "neutral", Confidence: .2, Actionable: false,
		EvidenceKeys: []string{"feedback-text"},
	}}
	feedbackRepo, err := NewSQLRepository(fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	feedbackService, err := NewService(feedbackRepo, classifier, "feedback-acceptance", queue)
	if err != nil {
		t.Fatal(err)
	}

	commands := []Command{
		{UserID: fixture.userID, RecommendationID: fixture.recommendationID(t, fixture.discoveryIDs[1]), Action: ActionLike, Reason: "genre", FreeText: "нравится атмосфера", IdempotencyKey: "presentation-discovery-like"},
		{UserID: fixture.userID, RecommendationID: fixture.recommendationID(t, fixture.wishlistIDs[1]), Action: ActionDislike, Reason: "length", FreeText: "слишком длинная", IdempotencyKey: "presentation-wishlist-dislike"},
	}
	expectedSnapshotGames := []uuid.UUID{fixture.discoveryIDs[1], fixture.wishlistIDs[1]}
	accepted := make([]Accepted, 0, len(commands))
	for _, command := range commands {
		value, err := feedbackService.Record(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, value)
		if err := feedbackService.ProcessClassification(ctx, value.EventID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.profiles.Rebuild(ctx, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.ranking.Rebuild(ctx, fixture.userID); err != nil {
		t.Fatal(err)
	}

	after, err := fixture.ranking.Overview(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	gotMembership := overviewMembership(after)
	if !reflect.DeepEqual(gotMembership["discovery"], beforeMembership["discovery"]) || len(gotMembership["wishlist"]) != len(beforeMembership["wishlist"])-1 || overviewSection(after, fixture.wishlistIDs[1]) != "" || overviewSection(after, fixture.wishlistIDs[0]) != "wishlist" {
		t.Fatalf("feedback did not exclude only the currently disliked game: before=%v after=%v", beforeMembership, gotMembership)
	}
	if after.Discoveries.Total != 2 || after.Wishlist.Total != 1 || calendarSize(after.Calendar) != 3 {
		t.Fatalf("incomplete overview after rerank: discoveries=%d wishlist=%d calendar=%d", after.Discoveries.Total, after.Wishlist.Total, calendarSize(after.Calendar))
	}

	latest, err := fixture.ranking.Latest(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	byGame := make(map[uuid.UUID]recommendations.Recommendation, len(latest))
	for _, item := range latest {
		byGame[item.GameID] = item
		if item.AlgorithmVersion != recommendations.AlgorithmVersion || item.Weights.Wishlist != 0 || item.Weights.PositiveDirectFeedback != 20 || item.Weights.NegativeDirectFeedback != -30 {
			t.Fatalf("unexpected persisted ranking-v2 snapshots for %s: algorithm=%q weights=%#v", item.GameID, item.AlgorithmVersion, item.Weights)
		}
	}
	if byGame[fixture.discoveryIDs[1]].Factors.PositiveDirectFeedback != 1 {
		t.Fatalf("discovery feedback factor=%v, want 1", byGame[fixture.discoveryIDs[1]].Factors.PositiveDirectFeedback)
	}
	if _, present := byGame[fixture.wishlistIDs[1]]; present {
		t.Fatal("currently disliked wishlist game remained in the published ranking")
	}
	for index, command := range commands {
		var raw json.RawMessage
		var snapshot struct {
			ID             uuid.UUID               `json:"id"`
			GameID         uuid.UUID               `json:"gameId"`
			RankingVersion int64                   `json:"rankingVersion"`
			Factors        recommendations.Factors `json:"factors"`
		}
		if err := fixture.db.Pool.QueryRow(ctx, `SELECT recommendation_snapshot FROM feedback_events WHERE id=$1`, accepted[index].EventID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.ID != command.RecommendationID || snapshot.GameID != expectedSnapshotGames[index] || snapshot.RankingVersion != 1 {
			t.Fatalf("feedback snapshot changed after rerank: %#v", snapshot)
		}
	}
}

func TestWishlistMembershipChangeReclassifiesOnlyAfterPublishedRebuild(t *testing.T) {
	fixture := newPresentationAcceptanceFixture(t)
	ctx := context.Background()
	transitioning := fixture.discoveryIDs[0]

	before, err := fixture.ranking.Overview(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	if overviewSection(before, transitioning) != "discovery" {
		t.Fatal("fixture game did not start as a discovery")
	}
	if _, err := fixture.db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, fixture.userID, transitioning); err != nil {
		t.Fatal(err)
	}
	stale, err := fixture.ranking.Overview(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	if overviewSection(stale, transitioning) != "discovery" {
		t.Fatal("unpublished wishlist change reclassified the visible collection")
	}

	inputs, err := fixture.rankingRepo.ResolveRankingInputs(ctx, recommendations.RankingRequest{UserID: fixture.userID, RequestKey: "wishlist-membership-publish-boundary"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	weights := recommendations.DefaultWeights()
	prepared := make([]recommendations.Recommendation, 0, len(inputs.Candidates))
	for _, candidate := range inputs.Candidates {
		if !recommendations.Eligible(candidate) {
			continue
		}
		prepared = append(prepared, recommendations.Recommendation{
			GameID: candidate.GameID, Name: candidate.Name, ProfileVersion: inputs.Source.Activity.Version,
			AlgorithmVersion: recommendations.AlgorithmVersion, Score: recommendations.Score(candidate.Factors, weights).Score,
			Factors: candidate.Factors, Weights: weights, Wishlisted: candidate.Factors.Wishlist > 0, Release: candidate.Release,
		})
	}
	prepared = recommendations.Sort(prepared)
	buildKey := recommendations.RankingBuildKey("wishlist-membership-publish-boundary", recommendations.AlgorithmVersion, inputs.Source)
	saved, version, _, err := fixture.rankingRepo.SaveRanking(ctx, fixture.userID, inputs.Source, prepared, buildKey)
	if err != nil {
		t.Fatal(err)
	}
	whileUnpublished, err := fixture.ranking.Overview(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	if overviewSection(whileUnpublished, transitioning) != "discovery" {
		t.Fatal("unpublished rebuilt ranking became visible")
	}
	for _, item := range saved {
		if err := fixture.rankingRepo.SaveExplanation(ctx, fixture.userID, version, buildKey, item.ID, item.Name, recommendations.ExplanationRecord{Text: "Недостаточно данных для персонального объяснения.", Source: recommendations.ExplanationSourceNone, Supplied: []recommendations.GroundingEvidence{}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.rankingRepo.PublishRanking(ctx, fixture.userID, version, buildKey); err != nil {
		t.Fatal(err)
	}
	after, err := fixture.ranking.Overview(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	if overviewSection(after, transitioning) != "wishlist" || after.Discoveries.Total != 1 || after.Wishlist.Total != 3 || calendarSize(after.Calendar) != 4 {
		t.Fatalf("published wishlist transition not reflected exactly once: %#v", overviewMembership(after))
	}
}

type presentationAcceptanceFixture struct {
	db                        *store.Store
	userID                    uuid.UUID
	discoveryIDs, wishlistIDs []uuid.UUID
	profiles                  *profile.Service
	ranking                   *recommendations.Service
	rankingRepo               *recommendations.SQLRepository
}

func newPresentationAcceptanceFixture(t *testing.T) presentationAcceptanceFixture {
	t.Helper()
	db := openFeedbackStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	discoveryIDs := []uuid.UUID{insertFeedbackGame(t, db, 93001, "Alpha discovery"), insertFeedbackGame(t, db, 93002, "Beta discovery")}
	wishlistIDs := []uuid.UUID{insertFeedbackGame(t, db, 93003, "Gamma wishlist"), insertFeedbackGame(t, db, 93004, "Delta wishlist")}
	for _, gameID := range append(append([]uuid.UUID{}, discoveryIDs...), wishlistIDs...) {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, gameID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'coming_soon','Coming Soon',now(),'test',now())`, uuid.New(), gameID); err != nil {
			t.Fatal(err)
		}
	}
	for _, gameID := range wishlistIDs {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, userID, gameID); err != nil {
			t.Fatal(err)
		}
	}
	profileRepo, err := profile.NewSQLRepository(db, "embed")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewService(profileRepo)
	if err != nil {
		t.Fatal(err)
	}
	zero := make([]float32, profile.Dimensions)
	if _, err := profileRepo.SaveProfile(ctx, userID, profile.Vectors{Positive: zero, Negative: append([]float32(nil), zero...), Confidence: 1}, profile.AlgorithmVersion, "presentation-acceptance-profile"); err != nil {
		t.Fatal(err)
	}
	rankingRepo, err := recommendations.NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	ranking, err := recommendations.NewService(rankingRepo, profiles, nil, allowAllFeedbackEligibility{}, feedbackTestClock)
	if err != nil {
		t.Fatal(err)
	}
	ranking.SetOverviewSources(rankingRepo, nil)
	if _, err := ranking.Rebuild(ctx, userID); err != nil {
		t.Fatal(err)
	}
	return presentationAcceptanceFixture{db: db, userID: userID, discoveryIDs: discoveryIDs, wishlistIDs: wishlistIDs, profiles: profiles, ranking: ranking, rankingRepo: rankingRepo}
}

func (f presentationAcceptanceFixture) recommendationID(t *testing.T, gameID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT id FROM recommendations WHERE user_id=$1 AND game_id=$2 AND ranking_version=1`, f.userID, gameID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func overviewMembership(value recommendations.Overview) map[string][]uuid.UUID {
	result := map[string][]uuid.UUID{"discovery": {}, "wishlist": {}}
	if value.Discoveries.Featured != nil {
		result["discovery"] = append(result["discovery"], value.Discoveries.Featured.GameID)
	}
	for _, item := range value.Discoveries.Items {
		result["discovery"] = append(result["discovery"], item.GameID)
	}
	for _, item := range value.Wishlist.Items {
		result["wishlist"] = append(result["wishlist"], item.GameID)
	}
	for key := range result {
		sort.Slice(result[key], func(i, j int) bool { return result[key][i].String() < result[key][j].String() })
	}
	return result
}

func overviewSection(value recommendations.Overview, gameID uuid.UUID) string {
	for section, ids := range overviewMembership(value) {
		for _, id := range ids {
			if id == gameID {
				return section
			}
		}
	}
	return ""
}

func calendarSize(value recommendations.Calendar) int {
	return len(value.ExactDates) + len(value.Month) + len(value.Quarter) + len(value.Year) + len(value.ComingSoon) + len(value.TBA)
}
