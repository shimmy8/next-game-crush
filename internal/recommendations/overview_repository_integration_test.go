package recommendations

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/profile"
)

func TestWishlistSourceStateScopesAndPreservesTimestamp(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	owner, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, other} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	repo, _ := NewSQLRepository(db, "embed", "text")
	state, err := repo.WishlistSourceState(ctx, owner)
	if err != nil || state.Availability != "unknown" || state.LastSuccessfulFetchAt != nil {
		t.Fatalf("missing=%#v err=%v", state, err)
	}
	success := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		user                      uuid.UUID
		source, key, availability string
	}{{owner, "wishlist", "", "available"}, {owner, "wishlist", "wrong", "request_failed"}, {owner, "owned_games", "", "unavailable"}, {other, "wishlist", "", "unavailable"}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO user_source_states(user_id,source,resource_key,availability,last_attempt_at,last_successful_fetch_at,error)VALUES($1,$2,$3,$4,$5,$5,'private diagnostic')`, row.user, row.source, row.key, row.availability, success); err != nil {
			t.Fatal(err)
		}
	}
	for _, availability := range []string{"available", "unavailable", "request_failed"} {
		if _, err := db.Pool.Exec(ctx, `UPDATE user_source_states SET availability=$2,last_attempt_at=$3 WHERE user_id=$1 AND source='wishlist' AND resource_key=''`, owner, availability, success.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		state, err = repo.WishlistSourceState(ctx, owner)
		if err != nil || state.Availability != availability || state.LastSuccessfulFetchAt == nil || !state.LastSuccessfulFetchAt.Equal(success) {
			t.Fatalf("state=%#v err=%v", state, err)
		}
	}
	state, err = repo.WishlistSourceState(ctx, other)
	if err != nil || state.Availability != "unavailable" {
		t.Fatalf("other=%#v err=%v", state, err)
	}
}

func TestOverviewReadsOnePublishedRunDuringRebuild(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	owner := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	profiles, _ := profile.NewSQLRepository(db, "embed")
	p, err := profiles.SaveProfile(ctx, owner, profile.Vectors{Positive: make([]float32, profile.Dimensions), Negative: make([]float32, profile.Dimensions)}, profile.AlgorithmVersion, "overview-profile")
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := NewSQLRepository(db, "embed", "text")
	service := &Service{repository: repo}
	service.SetOverviewSources(repo, nil)
	first := insertRankingGame(t, db, 7101, "Published discovery")
	second := insertRankingGame(t, db, 7102, "Next wishlist")
	firstSaved, v1, _, err := repo.SaveRanking(ctx, owner, p, []Recommendation{{GameID: first, AlgorithmVersion: AlgorithmVersion, Weights: DefaultWeights()}}, "overview-v1")
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.SaveExplanation(ctx, owner, v1, "overview-v1", firstSaved[0].ID, firstSaved[0].Name, ExplanationRecord{Text: neutralExplanation, Source: ExplanationSourceNone, Supplied: []GroundingEvidence{}}); err != nil {
		t.Fatal(err)
	}
	if err = repo.PublishRanking(ctx, owner, v1, "overview-v1"); err != nil {
		t.Fatal(err)
	}
	secondSaved, v2, _, err := repo.SaveRanking(ctx, owner, p, []Recommendation{{GameID: second, AlgorithmVersion: AlgorithmVersion, Weights: DefaultWeights(), Factors: Factors{Wishlist: 1}}}, "overview-v2")
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.Overview(ctx, owner)
	if err != nil || before.Discoveries.Featured == nil || before.Discoveries.Featured.GameID != first || before.Discoveries.Featured.RankingVersion != v1 || before.Wishlist.Total != 0 {
		t.Fatalf("before=%#v err=%v", before, err)
	}
	if err = repo.SaveExplanation(ctx, owner, v2, "overview-v2", secondSaved[0].ID, secondSaved[0].Name, ExplanationRecord{Text: neutralExplanation, Source: ExplanationSourceNone, Supplied: []GroundingEvidence{}}); err != nil {
		t.Fatal(err)
	}
	if err = repo.PublishRanking(ctx, owner, v2, "overview-v2"); err != nil {
		t.Fatal(err)
	}
	after, err := service.Overview(ctx, owner)
	if err != nil || after.Discoveries.Featured != nil || after.Wishlist.Total != 1 || after.Wishlist.Items[0].GameID != second || after.Wishlist.Items[0].RankingVersion != v2 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
}
