package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/profile"
)

func TestRankingV2PublishesWithoutMutatingV1(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	ctx := context.Background()
	items, version, _, err := service.rebuild(ctx, old.owner, "same-workflow")
	if err != nil || len(items) != 1 || version != 2 {
		t.Fatalf("v2 rebuild: items=%#v version=%d err=%v", items, version, err)
	}
	if items[0].AlgorithmVersion != AlgorithmVersion || items[0].Weights.Wishlist != 0 || items[0].Factors.Wishlist != 1 || items[0].Score != 0 {
		t.Fatalf("v2 snapshot=%#v", items[0])
	}
	repo := service.repository.(*SQLRepository)
	var key string
	if err := repo.store.Pool.QueryRow(ctx, `SELECT build_key FROM recommendation_runs WHERE user_id=$1 AND ranking_version=2 AND published`, old.owner).Scan(&key); err != nil || key != RankingBuildKey("same-workflow", AlgorithmVersion, ActiveAdjustmentsDigest(nil)) {
		t.Fatalf("v2 key=%q err=%v", key, err)
	}
	latest, err := service.Latest(ctx, old.owner)
	if err != nil || len(latest) != 1 || latest[0].ID != items[0].ID {
		t.Fatalf("published latest=%#v err=%v", latest, err)
	}
	assertHistoricalRanking(t, service, old)
	if _, replayVersion, _, err := service.rebuild(ctx, old.owner, "same-workflow"); err != nil || replayVersion != version {
		t.Fatalf("retry version=%d err=%v", replayVersion, err)
	}
}

func TestFailedV2BuildKeepsPreviousPublishedRun(t *testing.T) {
	service, old := historicalRankingFixture(t, true)
	items, version, _, err := service.rebuild(context.Background(), old.owner, "same-workflow")
	if err == nil || len(items) != 0 || version != 0 {
		t.Fatalf("failed build leaked partial result: items=%#v version=%d err=%v", items, version, err)
	}
	latest, err := service.Latest(context.Background(), old.owner)
	if err != nil || len(latest) != 1 || latest[0].ID != old.id || latest[0].AlgorithmVersion != "ranking-v1" {
		t.Fatalf("previous published ranking=%#v err=%v", latest, err)
	}
	repo := service.repository.(failingExplanationRepository).SQLRepository
	var unpublished int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM recommendation_runs WHERE user_id=$1 AND ranking_version=2 AND NOT published`, old.owner).Scan(&unpublished); err != nil || unpublished != 1 {
		t.Fatalf("unpublished v2 runs=%d err=%v", unpublished, err)
	}
	assertHistoricalRanking(t, service, old)
}

func TestLatestSuppressesDislikedGameWhileReplacementBuildFails(t *testing.T) {
	service, old := historicalRankingFixture(t, true)
	repo := service.repository.(failingExplanationRepository).SQLRepository
	ctx := context.Background()
	var dislikedGameID uuid.UUID
	if err := repo.store.Pool.QueryRow(ctx, `SELECT game_id FROM recommendations WHERE id=$1`, old.id).Scan(&dislikedGameID); err != nil {
		t.Fatal(err)
	}
	similarGameID := insertRankingGame(t, repo.store, 9101, "Similar Game")
	similarRecommendationID := uuid.New()
	factors, _ := json.Marshal(Factors{Confidence: 1})
	weights, _ := json.Marshal(DefaultWeights())
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, []any{similarGameID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) SELECT $1,user_id,$2,profile_id,algorithm_version,ranking_version,20 FROM recommendations WHERE id=$3`, []any{similarRecommendationID, similarGameID, old.id}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, []any{similarRecommendationID, factors, weights}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,reason,reasons,idempotency_key) VALUES($1,$2,$3,'dislike','genre',ARRAY['genre'],$4)`, []any{uuid.New(), old.id, old.owner, uuid.NewString()}},
	} {
		if _, err := repo.store.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, _, err := service.rebuild(ctx, old.owner, "disliked-workflow"); err == nil {
		t.Fatal("replacement build unexpectedly succeeded")
	}
	latest, err := service.Latest(ctx, old.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 || latest[0].GameID != similarGameID {
		t.Fatalf("latest did not suppress only the disliked game: %#v", latest)
	}
	overview := BuildOverview(latest, WishlistSourceState{}, catalog.PresentationStatus{})
	if overview.Discoveries.Total != 1 || overview.Wishlist.Total != 0 || overview.Discoveries.Featured == nil || overview.Discoveries.Featured.GameID != similarGameID {
		t.Fatalf("overview did not suppress only the disliked game: %#v", overview)
	}

	insertDirect := func(id uuid.UUID, action string, createdAt time.Time) {
		t.Helper()
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,reasons,idempotency_key,created_at) VALUES($1,$2,$3,$4,'{}',$5,$6)`, id, old.id, old.owner, action, id.String(), createdAt); err != nil {
			t.Fatal(err)
		}
	}
	assertVisibleGames := func(want ...uuid.UUID) {
		t.Helper()
		got, err := service.Latest(ctx, old.owner)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("visible games=%#v, want=%v", got, want)
		}
		for index := range want {
			if got[index].GameID != want[index] {
				t.Fatalf("visible games=%#v, want=%v", got, want)
			}
		}
	}

	insertDirect(uuid.New(), "remind_me", time.Now().UTC().Add(time.Second))
	assertVisibleGames(similarGameID)
	insertDirect(uuid.New(), "like", time.Now().UTC().Add(2*time.Second))
	assertVisibleGames(dislikedGameID, similarGameID)
	insertDirect(uuid.New(), "exclude", time.Now().UTC().Add(3*time.Second))
	assertVisibleGames(similarGameID)
	insertDirect(uuid.New(), "like", time.Now().UTC().Add(4*time.Second))
	assertVisibleGames(dislikedGameID, similarGameID)
}

func TestWishlistWatchlistExplanationPolicyAcrossRebuildLatestAndGet(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	client := &countingExplanationClient{output: ai.Explanation{Text: "Эта игра подходит вашим интересам.", EvidenceKeys: []string{"wishlist"}}}
	service.explainer = client
	ctx := context.Background()
	items, _, _, err := service.rebuild(ctx, old.owner, "watchlist-explanation")
	if err != nil || len(items) != 1 || client.calls != 0 {
		t.Fatalf("membership-only rebuild=%#v calls=%d err=%v", items, client.calls, err)
	}
	assertWatchlist := func(item Recommendation) {
		t.Helper()
		if item.GeneratedExplanation || item.ExplanationSource != ExplanationSourceDeterministic || item.Explanation != "Вы уже добавили эту игру в желаемое — следим за датой выхода." || !hasEvidenceID(item.Evidence, "wishlist-context-1") {
			t.Fatalf("watchlist explanation=%#v", item)
		}
	}
	assertWatchlist(items[0])
	repo := service.repository.(*SQLRepository)
	// Old saved generated copy must be checked by both read paths as well.
	if _, err := repo.store.Pool.Exec(ctx, `UPDATE recommendations SET explanation='Эта игра подходит вашим интересам.',explanation_generated=true,explanation_evidence='[{"id":"wishlist","text":"Игра есть в вашем списке желаемого"}]' WHERE id=$1`, items[0].ID); err != nil {
		t.Fatal(err)
	}
	latest, err := service.Latest(ctx, old.owner)
	if err != nil || len(latest) != 1 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	if latest[0].ExplanationSource != ExplanationSourceNone || latest[0].Explanation != neutralExplanation || len(latest[0].Evidence) != 0 {
		t.Fatalf("unsafe legacy latest did not degrade: %#v", latest[0])
	}
	got, err := service.Get(ctx, old.owner, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExplanationSource != ExplanationSourceNone || got.Explanation != neutralExplanation || len(got.Evidence) != 0 {
		t.Fatalf("unsafe legacy get did not degrade: %#v", got)
	}
}

type historicalRanking struct {
	owner, id uuid.UUID
	weights   Weights
}

func historicalRankingFixture(t *testing.T, failExplanation bool) (*Service, historicalRanking) {
	t.Helper()
	db := openRankingStore(t)
	ctx := context.Background()
	old := historicalRanking{owner: uuid.New(), id: uuid.New(), weights: DefaultWeights()}
	old.weights.Wishlist = 30
	profileID := uuid.New()
	gameID := insertRankingGame(t, db, 9100, "Watched Game")
	factors, err := json.Marshal(Factors{Wishlist: 1})
	if err != nil {
		t.Fatal(err)
	}
	weights, err := json.Marshal(old.weights)
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{old.owner}},
		{`INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, []any{profileID, old.owner}},
		{`INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, []any{gameID}},
		{`INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, []any{old.owner, gameID}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,'ranking-v1','same-workflow',true)`, []any{uuid.New(), old.owner, profileID}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) VALUES($1,$2,$3,$4,'ranking-v1',1,30)`, []any{old.id, old.owner, gameID, profileID}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, []any{old.id, factors, weights}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
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
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	var rankingRepo Repository = repo
	if failExplanation {
		rankingRepo = failingExplanationRepository{repo}
	}
	service, err := NewService(rankingRepo, profiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, old
}

func assertHistoricalRanking(t *testing.T, service *Service, old historicalRanking) {
	t.Helper()
	item, err := service.Get(context.Background(), old.owner, old.id)
	if err != nil || item.AlgorithmVersion != "ranking-v1" || item.RankingVersion != 1 || item.Score != 30 || item.Weights != old.weights || item.Factors.Wishlist != 1 {
		t.Fatalf("historical snapshot changed: %#v err=%v", item, err)
	}
}

type failingExplanationRepository struct{ *SQLRepository }

func (f failingExplanationRepository) SaveExplanation(context.Context, uuid.UUID, int64, string, uuid.UUID, string, ExplanationRecord) error {
	return errors.New("explanation storage unavailable")
}
