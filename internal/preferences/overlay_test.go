package preferences

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDislikeThenLikeProducesOnlyPositiveOverlay(t *testing.T) {
	vector := overlayUnitVector(3)
	result := BuildOverlay(OverlaySnapshot{Reactions: []DirectReaction{{
		FeedbackID: uuid.New(), GameID: uuid.New(), Action: "like", CreatedAt: time.Now(), EmbeddingHash: "like", Vector: vector,
	}}})
	if result.PositiveSources != 1 || result.NegativeSources != 0 || result.MissingVectors != 0 {
		t.Fatalf("counts=%+v", result)
	}
	if result.Positive[3] != 1 || result.Negative != nil {
		t.Fatalf("vectors positive=%v negative=%v", result.Positive[3], result.Negative)
	}
}

func TestLikeThenDislikeProducesOnlyNegativeOverlay(t *testing.T) {
	vector := overlayUnitVector(7)
	result := BuildOverlay(OverlaySnapshot{Reactions: []DirectReaction{{
		FeedbackID: uuid.New(), GameID: uuid.New(), Action: "dislike", CreatedAt: time.Now(), EmbeddingHash: "dislike", Vector: vector,
	}}})
	if result.PositiveSources != 0 || result.NegativeSources != 1 || result.MissingVectors != 0 {
		t.Fatalf("counts=%+v", result)
	}
	if result.Positive != nil || result.Negative[7] != 1 {
		t.Fatalf("vectors positive=%v negative=%v", result.Positive, result.Negative[7])
	}
}

func TestMissingEmbeddingPreservesDirectEffectWithoutVector(t *testing.T) {
	result := BuildOverlay(OverlaySnapshot{Reactions: []DirectReaction{{FeedbackID: uuid.New(), GameID: uuid.New(), Action: "exclude"}}})
	if result.Positive != nil || result.Negative != nil || result.MissingVectors != 1 || result.NegativeSources != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestOverlayUsesCentroidsAndNoFabricatedZeroRows(t *testing.T) {
	gameA := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	gameB := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	result := BuildOverlay(OverlaySnapshot{Reactions: []DirectReaction{
		{FeedbackID: uuid.New(), GameID: gameA, Action: "like", EmbeddingHash: "a", Vector: overlayUnitVector(0)},
		{FeedbackID: uuid.New(), GameID: gameB, Action: "like", EmbeddingHash: "b", Vector: overlayUnitVector(1)},
	}})
	if len(result.Positive) != OverlayDimensions || result.Positive[0] != .5 || result.Positive[1] != .5 || result.Negative != nil {
		t.Fatalf("unexpected centroid: first=%v second=%v negative=%v", result.Positive[0], result.Positive[1], result.Negative)
	}
	reversed := BuildOverlay(OverlaySnapshot{Reactions: []DirectReaction{
		{FeedbackID: resultReactionID(2), GameID: gameB, Action: "like", EmbeddingHash: "b", Vector: overlayUnitVector(1)},
		{FeedbackID: resultReactionID(1), GameID: gameA, Action: "like", EmbeddingHash: "a", Vector: overlayUnitVector(0)},
	}})
	ordered := BuildOverlay(OverlaySnapshot{Reactions: []DirectReaction{
		{FeedbackID: resultReactionID(1), GameID: gameA, Action: "like", EmbeddingHash: "a", Vector: overlayUnitVector(0)},
		{FeedbackID: resultReactionID(2), GameID: gameB, Action: "like", EmbeddingHash: "b", Vector: overlayUnitVector(1)},
	}})
	if reversed.InputContentHash != ordered.InputContentHash {
		t.Fatalf("hash depends on input order: %s != %s", reversed.InputContentHash, ordered.InputContentHash)
	}
}

func TestSupersededAndDisabledCorrectionsAreNotEffective(t *testing.T) {
	snapshot := OverlaySnapshot{Corrections: []Adjustment{
		{ID: uuid.New(), Dimension: "mode", Target: ModeCoop, Direction: "avoid", Confidence: .9, Strength: .5, Active: true},
		{ID: uuid.New(), Dimension: "mode", Target: ModeCoop, Direction: "avoid", Confidence: .8, Strength: .8, Active: true},
		{ID: uuid.New(), Dimension: "mode", Target: ModeCompetitive, Direction: "avoid", Confidence: 1, Strength: 1, Active: false},
	}}
	result := BuildOverlay(snapshot)
	if result.EffectiveCorrections != 1 {
		t.Fatalf("effective corrections=%d", result.EffectiveCorrections)
	}
	withoutWeaker := BuildOverlay(OverlaySnapshot{Corrections: snapshot.Corrections[1:2]})
	if result.InputContentHash != withoutWeaker.InputContentHash {
		t.Fatal("weaker duplicate changed semantic overlay hash")
	}
}

func TestCanonicalEmptyOverlayHashIsStable(t *testing.T) {
	a, b := BuildOverlay(OverlaySnapshot{}), BuildOverlay(OverlaySnapshot{Reactions: []DirectReaction{}, Corrections: []Adjustment{}})
	if a.InputContentHash == "" || a.InputContentHash != b.InputContentHash || a.Positive != nil || a.Negative != nil {
		t.Fatalf("empty overlays differ: %+v %+v", a, b)
	}
}

func overlayUnitVector(index int) []float32 {
	result := make([]float32, OverlayDimensions)
	result[index] = 1
	return result
}

func resultReactionID(value int) uuid.UUID {
	return uuid.MustParse("00000000-0000-4000-8000-00000000000" + string(rune('0'+value)))
}
