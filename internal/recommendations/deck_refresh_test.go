package recommendations

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/jobs"
)

type publicationDeckPlanner struct {
	t      *testing.T
	repo   *SQLRepository
	owner  uuid.UUID
	queue  *jobs.Queue
	failAt int
	calls  int
}

func (p *publicationDeckPlanner) EnqueueDeckRefresh(ctx context.Context, target catalog.DeckRefreshTarget) error {
	p.t.Helper()
	var published bool
	if err := p.repo.store.Pool.QueryRow(ctx, `SELECT published FROM recommendation_runs WHERE user_id=$1 AND ranking_version=$2`, p.owner, target.RankingVersion).Scan(&published); err != nil || !published {
		p.t.Fatalf("Deck enqueue before publication: published=%v err=%v", published, err)
	}
	p.calls++
	if p.calls == p.failAt {
		return errors.New("injected enqueue failure")
	}
	_, err := p.queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneCatalog, Kind: catalog.DeckRefreshJobKind, IdempotencyKey: fmt.Sprintf("catalog.deck_refresh:v1:%s:ranking:%d", target.GameID, target.RankingVersion)})
	return err
}

func TestPublishedRankingEnqueuesDeckRefreshOnlyForSelectedGames(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	// A catalog/wishlist game excluded by ownership must never become a target.
	unranked := insertRankingGame(t, repo.store, 9101, "Already Owned")
	if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, unranked); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`,
		`INSERT INTO user_games(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`,
	} {
		if _, err := repo.store.Pool.Exec(ctx, query, old.owner, unranked); err != nil {
			t.Fatal(err)
		}
	}
	planner := &publicationDeckPlanner{t: t, repo: repo, owner: old.owner, queue: jobs.NewQueue(repo.store)}
	service.SetDeckRefreshPlanner(planner)
	for _, key := range []string{"deck-selected", "deck-selected", "deck-next-version"} {
		if _, _, _, err := service.rebuild(ctx, old.owner, key); err != nil {
			t.Fatal(err)
		}
	}
	var jobsCount, versions int
	if err := repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind=$1 AND lane='catalog'`, catalog.DeckRefreshJobKind).Scan(&jobsCount); err != nil {
		t.Fatal(err)
	}
	if err := repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_runs WHERE user_id=$1 AND published AND algorithm_version=$2`, old.owner, AlgorithmVersion).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if jobsCount != 2 || versions != 2 {
		t.Fatalf("Deck jobs=%d published new versions=%d", jobsCount, versions)
	}
	for version := int64(2); version <= 3; version++ {
		targets, err := repo.PublishedDeckRefreshTargets(ctx, old.owner, version)
		if err != nil || len(targets) != 1 || targets[0].SteamAppID != 9100 || targets[0].RankingVersion != version {
			t.Fatalf("targets=%+v err=%v", targets, err)
		}
	}
}

func TestPublishedDeckRefreshReplayFillsPartialEnqueue(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	second := insertRankingGame(t, repo.store, 9101, "Second Selected")
	if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',now())`, old.owner, second); err != nil {
		t.Fatal(err)
	}
	planner := &publicationDeckPlanner{t: t, repo: repo, owner: old.owner, queue: jobs.NewQueue(repo.store), failAt: 2}
	service.SetDeckRefreshPlanner(planner)
	if _, _, _, err := service.rebuild(ctx, old.owner, "partial-deck"); err == nil {
		t.Fatal("missing enqueue failure")
	}
	var published bool
	var before, after int
	if err := repo.store.Pool.QueryRow(ctx, `SELECT published FROM recommendation_runs WHERE user_id=$1 AND ranking_version=2`, old.owner).Scan(&published); err != nil || !published {
		t.Fatalf("published=%v err=%v", published, err)
	}
	if err := repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Change current inputs: replay must still enumerate the immutable published rows.
	if _, err := repo.store.Pool.Exec(ctx, `DELETE FROM wishlist_entries WHERE user_id=$1 AND game_id=$2`, old.owner, second); err != nil {
		t.Fatal(err)
	}
	if _, version, _, err := service.rebuild(ctx, old.owner, "partial-deck"); err != nil || version != 2 {
		t.Fatalf("replay version=%d err=%v", version, err)
	}
	if err := repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != 1 || after != 2 || planner.calls != 4 {
		t.Fatalf("before=%d after=%d calls=%d", before, after, planner.calls)
	}
}

func TestFailedRankingPublicationNeverEnqueuesDeck(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	service.repository = deckPublicationFailure{repo}
	planner := &publicationDeckPlanner{t: t, repo: repo, owner: old.owner, queue: jobs.NewQueue(repo.store)}
	service.SetDeckRefreshPlanner(planner)
	if _, _, _, err := service.rebuild(context.Background(), old.owner, "failed-publish"); err == nil {
		t.Fatal("missing publication failure")
	}
	if planner.calls != 0 {
		t.Fatalf("unpublished ranking made %d Deck enqueue calls", planner.calls)
	}
	targets, err := repo.PublishedDeckRefreshTargets(context.Background(), old.owner, 2)
	if err != nil || len(targets) != 0 {
		t.Fatalf("unpublished targets=%+v err=%v", targets, err)
	}
}

type deckPublicationFailure struct{ *SQLRepository }

func (deckPublicationFailure) PublishRanking(context.Context, uuid.UUID, int64, string) error {
	return errors.New("injected publication failure")
}

type emptyDeckTargetRepository struct {
	*SQLRepository
	t     *testing.T
	calls int
}

func (r *emptyDeckTargetRepository) PublishedDeckRefreshTargets(ctx context.Context, userID uuid.UUID, version int64) ([]catalog.DeckRefreshTarget, error) {
	r.calls++
	var published bool
	if err := r.store.Pool.QueryRow(ctx, `SELECT published FROM recommendation_runs WHERE user_id=$1 AND ranking_version=$2`, userID, version).Scan(&published); err != nil || !published {
		r.t.Fatalf("empty target enumeration preceded publication: %v %v", published, err)
	}
	targets, err := r.SQLRepository.PublishedDeckRefreshTargets(ctx, userID, version)
	if err != nil || len(targets) != 0 {
		r.t.Fatalf("empty publication targets=%v err=%v", targets, err)
	}
	return targets, err
}

func TestEmptyPublishedRankingEnumeratesImmutableDeckTargets(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	if _, err := repo.store.Pool.Exec(ctx, `DELETE FROM wishlist_entries WHERE user_id=$1`, old.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(ctx, `DELETE FROM catalog_entries`); err != nil {
		t.Fatal(err)
	}
	tracking := &emptyDeckTargetRepository{SQLRepository: repo, t: t}
	service.repository = tracking
	planner := &publicationDeckPlanner{t: t, repo: repo, owner: old.owner, queue: jobs.NewQueue(repo.store)}
	service.SetDeckRefreshPlanner(planner)
	for range 2 {
		items, version, _, err := service.rebuild(ctx, old.owner, "empty-deck")
		if err != nil || version != 2 || len(items) != 0 {
			t.Fatalf("empty rebuild items=%d version=%d err=%v", len(items), version, err)
		}
	}
	if tracking.calls != 2 || planner.calls != 0 {
		t.Fatalf("enumerations=%d per-target planner calls=%d", tracking.calls, planner.calls)
	}
}
