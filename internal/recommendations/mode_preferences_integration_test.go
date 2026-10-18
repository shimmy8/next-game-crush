package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
)

func TestModePreferenceRankingInputsAndPersistedHumanEvidence(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	source := insertRankingGame(t, repo.store, 9200, "Co-op Source")
	target := insertRankingGame(t, repo.store, 9201, "Another Co-op")
	unknown := insertRankingGame(t, repo.store, 9202, "Co-op In Name Only")
	p, err := service.profiles.Latest(ctx, old.owner)
	if err != nil {
		t.Fatal(err)
	}
	sourceRec, event, adjustment := uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score)VALUES($1,$2,$3,$4,'fixture',1,15)`, []any{sourceRec, old.owner, source, p.ID}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'dislike',$4)`, []any{event, sourceRec, old.owner, event.String()}},
		{`INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)VALUES($1,$2,$3,$4,'mode','co-op','avoid',.9,1)`, []any{adjustment, old.owner, event, source}},
	} {
		if _, err := repo.store.Pool.Exec(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{source, target, unknown} {
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin)VALUES($1,'seed')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at)VALUES($1,$2,'day',$3,'future',now(),'test',now())`, uuid.New(), id, time.Now().AddDate(0, 0, 10)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{source, target} {
		if _, err := repo.store.Pool.Exec(ctx, `INSERT INTO game_modes(game_id,mode,source,source_content_hash,last_successful_fetch_at)VALUES($1,'co-op','steam_category','fixture',now())`, id); err != nil {
			t.Fatal(err)
		}
	}
	input, err := repo.RankingInputs(ctx, old.owner, p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.ActiveAdjustments) != 1 {
		t.Fatalf("active=%d", len(input.ActiveAdjustments))
	}
	seen := map[uuid.UUID]Candidate{}
	for _, c := range input.Candidates {
		seen[c.GameID] = c
	}
	if seen[target].Factors.NegativeModePreference != .9 || seen[source].Factors.NegativeModePreference != 0 || seen[unknown].Factors.NegativeModePreference != 0 {
		t.Fatalf("mode factors=%v/%v/%v", seen[target].Factors.NegativeModePreference, seen[source].Factors.NegativeModePreference, seen[unknown].Factors.NegativeModePreference)
	}
	items, _, _, err := service.rebuild(ctx, old.owner, "mode-persist")
	if err != nil {
		t.Fatal(err)
	}
	var persisted Recommendation
	for _, item := range items {
		if item.GameID == target {
			persisted = item
		}
	}
	if persisted.Factors.NegativeModePreference != .9 || persisted.Weights.NegativeModePreferenceWeight != -5 {
		t.Fatalf("persisted factors=%#v", persisted)
	}
	preferenceRepo, _ := preferences.NewSQLRepository(repo.store)
	if _, err := preferenceRepo.DisableAndEnqueue(ctx, old.owner, adjustment); err != nil {
		t.Fatal(err)
	}
	item, err := service.Get(ctx, old.owner, persisted.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := preferences.ModeEvidence(preferences.ModeCoop)
	found := false
	for _, e := range item.Evidence {
		if e.ID == "negative-mode-preference:co-op" && e.Text == expected {
			found = true
		}
	}
	if !found {
		t.Fatalf("persisted human evidence lost after disabling: %#v", item.Evidence)
	}
	var raw []byte
	if err := repo.store.Pool.QueryRow(ctx, `SELECT weights FROM recommendation_factors WHERE recommendation_id=$1`, item.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var weights map[string]float64
	if err := json.Unmarshal(raw, &weights); err != nil || weights["negativeModePreferenceWeight"] != -5 {
		t.Fatalf("weights=%s err=%v", raw, err)
	}
}

func TestRankingBuildUnpublishedReplacementAndPublishedReplay(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	p, err := service.profiles.Latest(ctx, old.owner)
	if err != nil {
		t.Fatal(err)
	}
	var game uuid.UUID
	if err := repo.store.Pool.QueryRow(ctx, `SELECT game_id FROM recommendations WHERE id=$1`, old.id).Scan(&game); err != nil {
		t.Fatal(err)
	}
	item := Recommendation{GameID: game, Name: "Watched Game", AlgorithmVersion: AlgorithmVersion, Weights: DefaultWeights(), Factors: Factors{NegativeModePreference: 1}}
	initial, v, err := repo.SaveRanking(ctx, old.owner, p, []Recommendation{item}, RankingBuildKey("replacement-request", AlgorithmVersion, "initial"))
	if err != nil || v != 2 {
		t.Fatalf("initial=%d err=%v", v, err)
	}
	item.Factors.NegativeModePreference = .4
	replaced, v, err := repo.SaveRanking(ctx, old.owner, p, []Recommendation{item}, RankingBuildKey("replacement-request", AlgorithmVersion, "changed"))
	if err != nil || v != 2 || len(replaced) != 1 || replaced[0].ID == initial[0].ID {
		t.Fatalf("replacement=%#v version=%d err=%v", replaced, v, err)
	}
	var key string
	if err := repo.store.Pool.QueryRow(ctx, `SELECT build_key FROM recommendation_runs WHERE user_id=$1 AND ranking_version=2`, old.owner).Scan(&key); err != nil || key != RankingBuildKey("replacement-request", AlgorithmVersion, "changed") {
		t.Fatalf("replacement key=%q err=%v", key, err)
	}
	changedKey := RankingBuildKey("replacement-request", AlgorithmVersion, "changed")
	for _, recommendation := range replaced {
		if err := repo.SaveExplanation(ctx, old.owner, 2, changedKey, recommendation.ID, recommendation.Name, ExplanationRecord{Text: neutralExplanation, Source: ExplanationSourceNone, Supplied: []GroundingEvidence{}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.PublishRanking(ctx, old.owner, 2, changedKey); err != nil {
		t.Fatal(err)
	}
	item.Factors.NegativeModePreference = 0
	replay, v, err := repo.SaveRanking(ctx, old.owner, p, []Recommendation{item}, RankingBuildKey("replacement-request", AlgorithmVersion, "later"))
	if err != nil || v != 2 || len(replay) != 0 {
		t.Fatalf("published replay=%#v version=%d err=%v", replay, v, err)
	}
	persisted, err := repo.Get(ctx, old.owner, replaced[0].ID)
	if err != nil || persisted.Factors.NegativeModePreference != .4 {
		t.Fatalf("published mutation=%#v err=%v", persisted, err)
	}
	_, next, err := repo.SaveRanking(ctx, old.owner, p, nil, RankingBuildKey("replacement-request", "different-algorithm", "changed"))
	if err != nil || next != 3 {
		t.Fatalf("new algorithm=%d err=%v", next, err)
	}
}

func TestRankingBuildConcurrentDigestsReplayOneVersion(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	p, err := service.profiles.Latest(ctx, old.owner)
	if err != nil {
		t.Fatal(err)
	}
	versions := make(chan int64, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, digest := range []string{"one", "two"} {
		wg.Add(1)
		go func(digest string) {
			defer wg.Done()
			_, v, err := repo.SaveRanking(ctx, old.owner, p, nil, RankingBuildKey("concurrent-request", AlgorithmVersion, digest))
			versions <- v
			errs <- err
		}(digest)
	}
	wg.Wait()
	close(versions)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for v := range versions {
		if v != 2 {
			t.Fatalf("version=%d", v)
		}
	}
	var currentBuildKey string
	if err := repo.store.Pool.QueryRow(ctx, `SELECT build_key FROM recommendation_runs WHERE user_id=$1 AND ranking_version=2`, old.owner).Scan(&currentBuildKey); err != nil {
		t.Fatal(err)
	}
	if err := repo.PublishRanking(ctx, old.owner, 2, currentBuildKey); err != nil {
		t.Fatal(err)
	}
	_, replayed, err := repo.SaveRanking(ctx, old.owner, p, nil, RankingBuildKey("concurrent-request", AlgorithmVersion, "three"))
	if err != nil || replayed != 2 {
		t.Fatalf("replay=%d err=%v", replayed, err)
	}
	var count int
	if err := repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_runs WHERE user_id=$1`, old.owner).Scan(&count); err != nil || count != 2 {
		t.Fatalf("runs=%d err=%v", count, err)
	}
	_, next, err := repo.SaveRanking(ctx, old.owner, profile.Profile{ID: p.ID}, nil, RankingBuildKey("disable-independent", AlgorithmVersion, "three"))
	if err != nil || next != 3 {
		t.Fatalf("independent=%d err=%v", next, err)
	}
}

func TestRankingBuildFencesStaleExplanationAndPublication(t *testing.T) {
	service, old := historicalRankingFixture(t, false)
	repo := service.repository.(*SQLRepository)
	ctx := context.Background()
	p, err := service.profiles.Latest(ctx, old.owner)
	if err != nil {
		t.Fatal(err)
	}
	var gameID uuid.UUID
	if err := repo.store.Pool.QueryRow(ctx, `SELECT game_id FROM recommendations WHERE id=$1`, old.id).Scan(&gameID); err != nil {
		t.Fatal(err)
	}
	item := Recommendation{GameID: gameID, Name: "Watched Game", AlgorithmVersion: AlgorithmVersion, Weights: DefaultWeights()}
	staleKey := RankingBuildKey("fenced-request", AlgorithmVersion, "stale")
	stale, version, err := repo.SaveRanking(ctx, old.owner, p, []Recommendation{item}, staleKey)
	if err != nil || len(stale) != 1 {
		t.Fatalf("stale preparation=%#v version=%d err=%v", stale, version, err)
	}
	currentKey := RankingBuildKey("fenced-request", AlgorithmVersion, "current")
	current, currentVersion, err := repo.SaveRanking(ctx, old.owner, p, []Recommendation{item}, currentKey)
	if err != nil || currentVersion != version || len(current) != 1 {
		t.Fatalf("current preparation=%#v version=%d err=%v", current, currentVersion, err)
	}
	record := ExplanationRecord{Text: neutralExplanation, Source: ExplanationSourceNone, Supplied: []GroundingEvidence{}}
	if err := repo.SaveExplanation(ctx, old.owner, version, staleKey, stale[0].ID, stale[0].Name, record); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale explanation write err=%v", err)
	}
	if err := repo.PublishRanking(ctx, old.owner, version, staleKey); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale publication err=%v", err)
	}
	if err := repo.PublishRanking(ctx, old.owner, version, currentKey); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("incomplete publication err=%v", err)
	}
	if err := repo.SaveExplanation(ctx, old.owner, version, currentKey, current[0].ID, current[0].Name, record); err != nil {
		t.Fatal(err)
	}
	if err := repo.PublishRanking(ctx, old.owner, version, currentKey); err != nil {
		t.Fatal(err)
	}
}
