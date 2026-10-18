package profile

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCalculateSignalWeights(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		signal Signal
		want   SignalWeights
		absent bool
	}{
		{name: "recent playtime", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), PlaytimeMinutes: 100, LastPlayedAt: asOf}, want: SignalWeights{Positive: 1, Playtime: 1}},
		{name: "old playtime", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), PlaytimeMinutes: 100, LastPlayedAt: asOf.Add(-10 * 365 * 24 * time.Hour)}, want: SignalWeights{Positive: 1.0 / 32, Playtime: 1.0 / 32}},
		{name: "missing recency", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), PlaytimeMinutes: 100}, want: SignalWeights{Positive: .25, Playtime: .25}},
		{name: "recent activity", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), RecentMinutes: 50}, want: SignalWeights{Positive: 1.5, Recent: 1.5}},
		{name: "wishlist", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), Wishlisted: true}, want: SignalWeights{Positive: 2, Wishlist: 2}},
		{name: "like", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), Likes: 2}, want: SignalWeights{Positive: 4, Feedback: 4}},
		{name: "dislike", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), Dislikes: 2}, want: SignalWeights{Negative: 4}},
		{name: "achievement", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), AchievementRatio: .5, LastPlayedAt: asOf}, want: SignalWeights{Positive: .25, Achievement: .25}},
		{name: "completion does not decay", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), CompletionWeight: 1.5, LastPlayedAt: asOf.Add(-20 * 365 * 24 * time.Hour)}, want: SignalWeights{Positive: 1.5, Completion: 1.5}},
		{name: "completion replaces achievement ratio", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), CompletionWeight: 1.85, AchievementRatio: 1, LastPlayedAt: asOf}, want: SignalWeights{Positive: 1.85, Completion: 1.85}},
		{name: "completion remains additive with independent signals", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), PlaytimeMinutes: 100, RecentMinutes: 50, CompletionWeight: 2.05, Likes: 1, LastPlayedAt: asOf}, want: SignalWeights{Positive: 6.55, Playtime: 1, Recent: 1.5, Completion: 2.05, Feedback: 2}},
		{name: "completion is clamped", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), CompletionWeight: 99}, want: SignalWeights{Positive: 2.25, Completion: 2.25}},
		{name: "completion override preserves achievement and negative history", signal: Signal{GameID: uuid.New(), Vector: unitVector(0), CompletionWeight: 1.5, CompletionOverridden: true, AchievementRatio: 1, Dislikes: 2, LastPlayedAt: asOf}, want: SignalWeights{Positive: .5, Negative: 4, Achievement: .5}},
		{name: "invalid vector", signal: Signal{GameID: uuid.New(), Vector: []float32{1, 2}, Likes: 1}, absent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := CalculateSignalWeights([]Signal{test.signal}, asOf)[test.signal.GameID]
			if test.absent {
				if ok {
					t.Fatalf("invalid signal weight = %#v, want absent", got)
				}
				return
			}
			if !ok || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("weight = %#v, present=%v, want %#v", got, ok, test.want)
			}
		})
	}
}

func TestCalculateSignalWeightsMatchesBuildVectors(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	signals := []Signal{
		{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: unitVector(0), PlaytimeMinutes: 1200, RecentMinutes: 60, AchievementRatio: .6, LastPlayedAt: asOf.Add(-365 * 24 * time.Hour), Likes: 1},
		{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: unitVector(1), PlaytimeMinutes: 600, Wishlisted: true, Dislikes: 1},
	}
	weights := CalculateSignalWeights(signals, asOf)
	wantPositive, wantNegative := make([]float32, Dimensions), make([]float32, Dimensions)
	var positiveTotal, negativeTotal float64
	for _, signal := range signals {
		weight := weights[signal.GameID]
		addWeighted(wantPositive, signal.Vector, weight.Positive)
		addWeighted(wantNegative, signal.Vector, weight.Negative)
		positiveTotal += weight.Positive
		negativeTotal += weight.Negative
	}
	divide(wantPositive, positiveTotal)
	divide(wantNegative, negativeTotal)
	got := BuildVectors(signals, asOf)
	if !reflect.DeepEqual(got.Positive, wantPositive) || !reflect.DeepEqual(got.Negative, wantNegative) {
		t.Fatalf("BuildVectors centroids do not match calculated weights")
	}
}

func TestBuildVectorsPreservesDuplicateGameIDBehavior(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	gameID := uuid.New()
	result := BuildVectors([]Signal{
		{GameID: gameID, Vector: unitVector(0), PlaytimeMinutes: 100},
		{GameID: gameID, Vector: unitVector(1), Wishlisted: true},
	}, asOf)
	if result.Evidence != 2 {
		t.Fatalf("evidence = %d, want 2", result.Evidence)
	}
	if math.Abs(float64(result.Positive[0])-1.0/9) > 1e-7 || math.Abs(float64(result.Positive[1])-8.0/9) > 1e-7 {
		t.Fatalf("positive centroid = [%v,%v], want [1/9,8/9]", result.Positive[0], result.Positive[1])
	}
	if math.Abs(result.PositiveStrength-.75) > 1e-12 {
		t.Fatalf("positive strength = %v, want .75", result.PositiveStrength)
	}

	result = BuildVectors([]Signal{
		{GameID: gameID, Vector: unitVector(0), Wishlisted: true},
		{GameID: gameID, Vector: []float32{1, 2}, Likes: 1},
	}, asOf)
	if result.Evidence != 1 || result.Positive[0] != 1 {
		t.Fatalf("valid/invalid duplicate result = %#v, want one valid wishlist signal", result)
	}
}

func unitVector(index int) []float32 {
	vector := make([]float32, Dimensions)
	vector[index] = 1
	return vector
}

func TestBuildVectorsUsesWeightedPositiveAndNegativeCentroids(t *testing.T) {
	a := make([]float32, Dimensions)
	b := make([]float32, Dimensions)
	a[0], b[1] = 1, 1
	result := BuildVectors([]Signal{
		{GameID: uuid.New(), Vector: a, PlaytimeMinutes: 1200, RecentMinutes: 120, AchievementRatio: .5, Wishlisted: true, Likes: 1},
		{GameID: uuid.New(), Vector: b, Dislikes: 2},
	}, time.Now())
	if result.Positive[0] != 1 || result.Positive[1] != 0 {
		t.Fatalf("positive centroid = [%v,%v], want [1,0]", result.Positive[0], result.Positive[1])
	}
	if result.Negative[0] != 0 || result.Negative[1] != 1 {
		t.Fatalf("negative centroid = [%v,%v], want [0,1]", result.Negative[0], result.Negative[1])
	}
	if result.Confidence <= 0 || result.Confidence > 1 {
		t.Fatalf("confidence = %v, want (0,1]", result.Confidence)
	}
	wantConfidence := .65*5.0/6.0 + .35*math.Log(3)/math.Log(11)
	if math.Abs(result.Confidence-wantConfidence) > 1e-12 {
		t.Fatalf("confidence = %v, want %v", result.Confidence, wantConfidence)
	}
	if math.Abs(result.PositiveStrength-31.0/36) > 1e-12 {
		t.Fatalf("positive strength = %v, want %v", result.PositiveStrength, 31.0/36)
	}
	if result.Evidence != 2 {
		t.Fatalf("evidence = %d, want 2", result.Evidence)
	}
}

func TestBuildVectorsHandlesSparseAndInvalidVectors(t *testing.T) {
	result := BuildVectors([]Signal{{Vector: nil, Wishlisted: true}, {Vector: []float32{1, 2}, Likes: 1}}, time.Now())
	if len(result.Positive) != Dimensions || len(result.Negative) != Dimensions || result.Confidence != 0 {
		t.Fatalf("sparse result = %#v", result)
	}
	for _, vector := range [][]float32{result.Positive, result.Negative} {
		for _, value := range vector {
			if value != 0 || math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				t.Fatalf("invalid sparse vector value %v", value)
			}
		}
	}
	if got := Cosine(result.Positive, result.Negative); got != 0 {
		t.Fatalf("zero-vector cosine = %v, want 0", got)
	}
}

func TestBuildVectorsIsDeterministicAcrossSignalOrder(t *testing.T) {
	v1, v2 := make([]float32, Dimensions), make([]float32, Dimensions)
	v1[0], v2[1] = 1, 1
	a := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: v1, PlaytimeMinutes: 10}
	b := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: v2, PlaytimeMinutes: 10}
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	first, second := BuildVectors([]Signal{a, b}, asOf), BuildVectors([]Signal{b, a}, asOf)
	if first.Positive[0] != second.Positive[0] || first.Positive[1] != second.Positive[1] {
		t.Fatalf("centroids differ by input order: %v vs %v", first.Positive[:2], second.Positive[:2])
	}
}

func TestPlaytimeRecencyWeightDecaysWithTwoYearHalfLife(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		lastPlayed time.Time
		want       float64
	}{
		{name: "fresh", lastPlayed: asOf, want: 1},
		{name: "two years", lastPlayed: asOf.Add(-2 * 365 * 24 * time.Hour), want: .5},
		{name: "ten years", lastPlayed: asOf.Add(-10 * 365 * 24 * time.Hour), want: 1.0 / 32},
		{name: "unknown", want: .25},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := HistoricalRecencyWeight(test.lastPlayed, asOf)
			if math.Abs(got-test.want) > 1e-9 {
				t.Fatalf("playtimeRecencyWeight() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBuildVectorsMakesFreshPlaytimeMoreInfluentialThanOldPlaytime(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	fresh, old := make([]float32, Dimensions), make([]float32, Dimensions)
	fresh[0], old[1] = 1, 1
	result := BuildVectors([]Signal{
		{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: fresh, PlaytimeMinutes: 10_000, AchievementRatio: 1, LastPlayedAt: asOf},
		{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: old, PlaytimeMinutes: 10_000, AchievementRatio: 1, LastPlayedAt: asOf.Add(-10 * 365 * 24 * time.Hour)},
	}, asOf)
	if result.Positive[0] <= result.Positive[1]*30 {
		t.Fatalf("fresh/old centroid = %v/%v, want fresh playtime over 30x stronger", result.Positive[0], result.Positive[1])
	}
}

func TestBuildVectorsPreservesAbsoluteRecencyStrengthAfterNormalization(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	vector := make([]float32, Dimensions)
	vector[0] = 1
	fresh := BuildVectors([]Signal{{GameID: uuid.New(), Vector: vector, PlaytimeMinutes: 10_000, LastPlayedAt: asOf}}, asOf)
	old := BuildVectors([]Signal{{GameID: uuid.New(), Vector: vector, PlaytimeMinutes: 10_000, LastPlayedAt: asOf.Add(-10 * 365 * 24 * time.Hour)}}, asOf)
	if fresh.Positive[0] != old.Positive[0] {
		t.Fatalf("normalized directions differ: fresh=%v old=%v", fresh.Positive[0], old.Positive[0])
	}
	if fresh.PositiveStrength <= old.PositiveStrength*30 {
		t.Fatalf("fresh/old strength = %v/%v, want fresh over 30x stronger", fresh.PositiveStrength, old.PositiveStrength)
	}

	oldWishlisted := BuildVectors([]Signal{{GameID: uuid.New(), Vector: vector, PlaytimeMinutes: 10_000, LastPlayedAt: asOf.Add(-10 * 365 * 24 * time.Hour), Wishlisted: true}}, asOf)
	if oldWishlisted.PositiveStrength < .65 {
		t.Fatalf("wishlisted old-game strength = %v, want explicit current intent preserved", oldWishlisted.PositiveStrength)
	}
}
