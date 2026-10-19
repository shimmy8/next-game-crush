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
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/steam"
)

type inertSteamClient struct{}

func (inertSteamClient) PlayerSummary(context.Context, string) (steam.PlayerSummary, error) {
	return steam.PlayerSummary{}, steam.ErrUnavailable
}
func (inertSteamClient) ProbeOwnedGames(context.Context, string) ([]steam.OwnedGame, steam.Availability, error) {
	return nil, steam.Unavailable, nil
}
func (inertSteamClient) OwnedGames(context.Context, string) ([]steam.OwnedGame, steam.Availability, error) {
	return nil, steam.Unavailable, nil
}
func (inertSteamClient) RecentlyPlayed(context.Context, string) ([]steam.RecentGame, steam.Availability, error) {
	return nil, steam.Unavailable, nil
}
func (inertSteamClient) Wishlist(context.Context, string) ([]steam.WishlistItem, steam.Availability, error) {
	return nil, steam.Unavailable, nil
}
func (inertSteamClient) Achievements(context.Context, string, int64) (steam.AchievementSummary, steam.Availability, error) {
	return steam.AchievementSummary{}, steam.Unavailable, nil
}
func (inertSteamClient) AchievementSchema(context.Context, int64) (steam.AchievementSchema, steam.Availability, error) {
	return steam.AchievementSchema{}, steam.Unavailable, nil
}
func (inertSteamClient) GameDetails(context.Context, int64) (steam.GameDetails, error) {
	return steam.GameDetails{}, steam.ErrUnavailable
}
func (inertSteamClient) DeckCompatibility(context.Context, int64) (steam.DeckCompatibility, steam.Availability, error) {
	return steam.DeckUnknown, steam.Unavailable, nil
}

type rebuildPublicationObserver struct {
	t     *testing.T
	repo  *SQLRepository
	calls []bool
	keys  []string
}

func (o *rebuildPublicationObserver) CatalogRebuildFinished(ctx context.Context, userID uuid.UUID, key string, succeeded bool) error {
	o.calls = append(o.calls, succeeded)
	o.keys = append(o.keys, key)
	if succeeded {
		var published bool
		if err := o.repo.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recommendation_runs WHERE user_id=$1 AND published AND ranking_version>1)`, userID).Scan(&published); err != nil || !published {
			o.t.Fatalf("completion preceded publication: published=%v err=%v", published, err)
		}
	}
	return nil
}

func TestCatalogRebuildObserverRunsAfterPublishFailureAndReplay(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	observer := &rebuildPublicationObserver{t: t, repo: repo}
	service.SetCatalogRebuildObserver(observer)
	key := "catalog:recommendations.rebuild:v2:" + old.owner.String() + ":1:retry:0"
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, UserID: old.owner})
	lease := jobs.Lease{ID: uuid.New(), UserID: old.owner, IdempotencyKey: key, Payload: payload}
	if err := service.handleJob(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if err := service.handleJob(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if len(observer.calls) != 2 || !observer.calls[0] || !observer.calls[1] || observer.keys[1] != key {
		t.Fatalf("success/replay observer=%v keys=%v", observer.calls, observer.keys)
	}

	failing, failedOld := historicalRankingFixture(t, true)
	failingRepo := failing.repository.(failingExplanationRepository).SQLRepository
	failedObserver := &rebuildPublicationObserver{t: t, repo: failingRepo}
	failing.SetCatalogRebuildObserver(failedObserver)
	failedKey := "catalog:recommendations.rebuild:v2:" + failedOld.owner.String() + ":1:retry:0"
	failedPayload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, UserID: failedOld.owner})
	if err := failing.handleJob(context.Background(), jobs.Lease{ID: uuid.New(), UserID: failedOld.owner, IdempotencyKey: failedKey, Payload: failedPayload}); err == nil {
		t.Fatal("missing injected rebuild failure")
	}
	if len(failedObserver.calls) != 1 || failedObserver.calls[0] {
		t.Fatalf("failure observer=%v", failedObserver.calls)
	}
	latest, err := failing.Latest(context.Background(), failedOld.owner)
	if err != nil || len(latest) != 1 || latest[0].ID != failedOld.id || latest[0].RankingVersion != 1 {
		t.Fatalf("failure replaced the last published ranking: latest=%#v err=%v", latest, err)
	}
}

func TestCatalogRetryGenerationReplaysPublishedRankingAndRepairsDeckFanout(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	second := insertRankingGame(t, repo.store, 9101, "Second Selected")
	if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, old.owner, second); err != nil {
		t.Fatal(err)
	}
	planner := &publicationDeckPlanner{t: t, repo: repo, owner: old.owner, queue: jobs.NewQueue(repo.store), failAt: 2}
	observer := &rebuildPublicationObserver{t: t, repo: repo}
	service.SetDeckRefreshPlanner(planner)
	service.SetCatalogRebuildObserver(observer)
	payload, _ := json.Marshal(JobPayload{Version: JobPayloadVersion, UserID: old.owner})
	root := "catalog:recommendations.rebuild:v2:" + old.owner.String() + ":41"
	gen0 := jobs.Lease{ID: uuid.New(), UserID: old.owner, IdempotencyKey: root + ":retry:0", Payload: payload}
	gen1 := jobs.Lease{ID: uuid.New(), UserID: old.owner, IdempotencyKey: root + ":retry:1", Payload: payload}
	if err := service.handleJob(ctx, gen0); err == nil {
		t.Fatal("missing injected generation-zero Deck failure")
	}
	if err := service.handleJob(ctx, gen1); err != nil {
		t.Fatalf("generation-one replay: %v", err)
	}
	var publishedRuns, rankingVersion, deckJobs int
	var buildKey string
	if err := repo.store.Pool.QueryRow(ctx, `SELECT count(*),max(ranking_version),max(build_key) FROM recommendation_runs
		WHERE user_id=$1 AND algorithm_version=$2 AND published`, old.owner, AlgorithmVersion).Scan(&publishedRuns, &rankingVersion, &buildKey); err != nil {
		t.Fatal(err)
	}
	if err := repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1`, catalog.DeckRefreshJobKind).Scan(&deckJobs); err != nil {
		t.Fatal(err)
	}
	wantBuildKey := RankingBuildKey(root, AlgorithmVersion, ActiveAdjustmentsDigest(nil))
	if publishedRuns != 1 || rankingVersion != 2 || buildKey != wantBuildKey || deckJobs != 2 || planner.calls != 4 {
		t.Fatalf("runs=%d version=%d build=%q want=%q deckJobs=%d calls=%d", publishedRuns, rankingVersion, buildKey, wantBuildKey, deckJobs, planner.calls)
	}
	if len(observer.calls) != 2 || observer.calls[0] || !observer.calls[1] || observer.keys[0] != gen0.IdempotencyKey || observer.keys[1] != gen1.IdempotencyKey {
		t.Fatalf("observer calls=%v keys=%v", observer.calls, observer.keys)
	}
}

func TestClockTransitionsChangeRealRankingBeforeRetryPublishes(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	var expiring uuid.UUID
	if err := repo.store.Pool.QueryRow(ctx, `SELECT game_id FROM wishlist_entries WHERE user_id=$1`, old.owner).Scan(&expiring); err != nil {
		t.Fatal(err)
	}
	entering := insertRankingGame(t, repo.store, 9101, "Entering Window")
	if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, old.owner, entering, now); err != nil {
		t.Fatal(err)
	}
	for _, game := range []struct {
		id         uuid.UUID
		appID      int64
		date       time.Time
		comingSoon bool
		active     bool
		band       string
	}{
		{expiring, 9100, now.AddDate(0, 0, -31), false, true, "positive"},
		{entering, 9101, now.AddDate(0, 0, 30), true, false, "unrated"},
	} {
		snapshot, _ := json.Marshal(steam.GameDetails{AppID: game.appID, Type: "game", Name: "Clock Candidate",
			ReleaseDate: steam.ReleaseDate{ComingSoon: game.comingSoon, Raw: game.date.Format("2 Jan, 2006")}})
		if _, err := repo.store.Pool.Exec(ctx, `UPDATE games SET source='steam_store',source_snapshot=$2,last_successful_fetch_at=$3 WHERE id=$1`, game.id, snapshot, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at)
			VALUES($1,$2,'day',$3,$4,$5,'steam_store',$5)`, uuid.New(), game.id, game.date, game.date.Format(time.DateOnly), now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO catalog_entry_sources(game_id,source,active,first_seen_at,last_seen_at)
			VALUES($1,'steam_discovery',$2,$3,$3) ON CONFLICT(game_id,source) DO UPDATE SET active=EXCLUDED.active,last_seen_at=EXCLUDED.last_seen_at`, game.id, game.active, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,release_text,precision,release_date,
			coming_soon,review_band,review_label,positive_percent,review_total,last_successful_appdetails_at,active,next_eligibility_at)
			VALUES($1,1,1,$2,'day',$3,$4,$5,'',90,100,$6,$7,$6)`, game.appID, game.date.Format("2 Jan, 2006"), game.date, game.comingSoon, game.band, now, game.active); err != nil {
			t.Fatal(err)
		}
	}
	service.eligibility = catalog.NewEligibilityRepository(repo.store)
	service.clock = func() time.Time { return now }
	cfg := catalog.DefaultDiscoveryConfig()
	cfg.Enabled = false
	queue := jobs.NewQueue(repo.store)
	discovery, err := catalog.NewDiscoveryService(catalog.NewDiscoveryRepository(repo.store), inertSteamClient{}, queue, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.SetCatalogRebuildObserver(discovery)
	planner := &publicationDeckPlanner{t: t, repo: repo, owner: old.owner, queue: queue, failAt: 1}
	service.SetDeckRefreshPlanner(planner)
	if err := catalog.NewSearchRepository(repo.store).ScheduleDueRebuilds(ctx, cfg, now); err != nil {
		t.Fatal(err)
	}
	loadLease := func(suffix string) jobs.Lease {
		t.Helper()
		var lease jobs.Lease
		if err := repo.store.Pool.QueryRow(ctx, `SELECT id,user_id,idempotency_key,payload FROM sync_jobs
			WHERE kind='recommendations.rebuild' AND idempotency_key LIKE $1 ORDER BY created_at DESC,id DESC LIMIT 1`, "%"+suffix).
			Scan(&lease.ID, &lease.UserID, &lease.IdempotencyKey, &lease.Payload); err != nil {
			t.Fatal(err)
		}
		return lease
	}
	gen0 := loadLease(":retry:0")
	if err := service.handleJob(ctx, gen0); err == nil {
		t.Fatal("missing post-publication Deck failure")
	}
	latest, err := service.Latest(ctx, old.owner)
	if err != nil || len(latest) != 1 || latest[0].GameID != entering || latest[0].RankingVersion != 2 {
		t.Fatalf("clock-transition ranking=%#v err=%v", latest, err)
	}
	if err := catalog.NewSearchRepository(repo.store).ScheduleDueRebuilds(ctx, cfg, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	gen1 := loadLease(":retry:1")
	if err := service.handleJob(ctx, gen1); err != nil {
		t.Fatal(err)
	}
	var runs, recommendations int
	var expiredCleared, enteringAdvanced bool
	if err := repo.store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM recommendation_runs WHERE user_id=$1 AND ranking_version=2 AND published),
		(SELECT count(*) FROM recommendations WHERE user_id=$1 AND ranking_version=2),
		(SELECT next_eligibility_at IS NULL FROM catalog_search_current WHERE steam_app_id=9100),
		(SELECT next_eligibility_at>$2 FROM catalog_search_current WHERE steam_app_id=9101)`, old.owner, now).
		Scan(&runs, &recommendations, &expiredCleared, &enteringAdvanced); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || recommendations != 1 || !expiredCleared || !enteringAdvanced || stableRebuildRequestKey(gen0.IdempotencyKey) != stableRebuildRequestKey(gen1.IdempotencyKey) {
		t.Fatalf("runs=%d recommendations=%d expiredCleared=%v enteringAdvanced=%v keys=%q/%q", runs, recommendations, expiredCleared, enteringAdvanced, gen0.IdempotencyKey, gen1.IdempotencyKey)
	}
}

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

func TestEmptyRebuildPublishesEmptyRanking(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	if _, err := repo.store.Pool.Exec(ctx, `DELETE FROM wishlist_entries WHERE user_id=$1`, old.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(ctx, `DELETE FROM catalog_entries`); err != nil {
		t.Fatal(err)
	}
	items, version, _, err := service.rebuild(ctx, old.owner, "empty-workflow")
	if err != nil || len(items) != 0 || version != 2 {
		t.Fatalf("empty rebuild: items=%#v version=%d err=%v", items, version, err)
	}
	var published bool
	if err := repo.store.Pool.QueryRow(ctx, `SELECT published FROM recommendation_runs WHERE user_id=$1 AND ranking_version=$2`, old.owner, version).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("empty ranking run was not published")
	}
	latest, err := service.Latest(ctx, old.owner)
	if err != nil || len(latest) != 0 {
		t.Fatalf("stale ranking remained visible: latest=%#v err=%v", latest, err)
	}
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
	service, err := NewService(rankingRepo, profiles, nil, allowAllCurrentEligibility{}, recommendationTestClock)
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
