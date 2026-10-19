package profile

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBuildVectorsUsesOnlyPlayedActivity(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	playtime := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: unitVector(0), PlaytimeMinutes: 1200, LastPlayedAt: asOf}
	recent := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: unitVector(1), RecentMinutes: 60}
	achieved := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), Vector: unitVector(2), AchievementRatio: .5, LastPlayedAt: asOf}
	completed := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000004"), Vector: unitVector(3), CompletionWeight: 1.5}

	got := BuildVectors([]Signal{completed, achieved, recent, playtime}, asOf)
	if got.Evidence != 4 {
		t.Fatalf("evidence = %d, want 4", got.Evidence)
	}
	if !reflect.DeepEqual(got.SignalCounts, map[string]int{"playtime": 1, "recent": 1, "achievements": 1, "completion": 1}) {
		t.Fatalf("signal counts = %#v", got.SignalCounts)
	}
	wantConfidence := .65 + .35*math.Log(5)/math.Log(11)
	if math.Abs(got.Confidence-wantConfidence) > 1e-12 {
		t.Fatalf("confidence = %v, want %v", got.Confidence, wantConfidence)
	}
	if len(got.Positive) != Dimensions {
		t.Fatalf("positive dimensions = %d", len(got.Positive))
	}
}

func TestEachActivityClassQualifiesIndependently(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		kind   string
		signal Signal
	}{
		{name: "playtime", kind: "playtime", signal: Signal{PlaytimeMinutes: 10, LastPlayedAt: asOf}},
		{name: "recent", kind: "recent", signal: Signal{RecentMinutes: 10}},
		{name: "achievements", kind: "achievements", signal: Signal{AchievementRatio: .5, LastPlayedAt: asOf}},
		{name: "completion", kind: "completion", signal: Signal{CompletionWeight: 1.5}},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.signal.GameID = uuid.New()
			test.signal.Vector = unitVector(i)
			got := BuildVectors([]Signal{test.signal}, asOf)
			if got.Evidence != 1 || got.SignalCounts[test.kind] != 1 || got.Confidence <= 0 {
				t.Fatalf("vectors = %#v", got)
			}
		})
	}
}

func TestBuildVectorsWritesNoNegativeActivityVector(t *testing.T) {
	got := BuildVectors([]Signal{{GameID: uuid.New(), Vector: unitVector(0), CompletionWeight: 2.25}}, time.Now())
	if got.Negative != nil {
		t.Fatalf("negative activity vector = %#v, want nil", got.Negative)
	}
}

func TestBuildVectorsRejectsVectorOnlyIneligibleSignal(t *testing.T) {
	got := BuildVectors([]Signal{{GameID: uuid.New(), Vector: unitVector(0)}}, time.Now())
	if got.Evidence != 0 || validVector(got.Positive) || got.Confidence != 0 || got.PositiveStrength != 0 || len(got.SignalCounts) != 0 {
		t.Fatalf("ineligible vector contributed to profile: %#v", got)
	}
}

func TestBuildVectorsCountsActivityWithoutEmbeddingButExcludesItFromSemanticMetrics(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	got := BuildVectors([]Signal{
		{GameID: uuid.New(), PlaytimeMinutes: 60, LastPlayedAt: asOf},
		{GameID: uuid.New(), Vector: unitVector(0), RecentMinutes: 30},
	}, asOf)
	if got.Evidence != 1 {
		t.Fatalf("semantic evidence = %d, want 1", got.Evidence)
	}
	if !reflect.DeepEqual(got.SignalCounts, map[string]int{"playtime": 1, "recent": 1}) {
		t.Fatalf("activity counts = %#v", got.SignalCounts)
	}
	want := .65*(1.0/4.0) + .35*(math.Log(2)/math.Log(11))
	if math.Abs(got.Confidence-want) > 1e-12 {
		t.Fatalf("confidence = %v, want %v", got.Confidence, want)
	}
}

func TestBuildVectorsIsDeterministicAcrossSignalOrder(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	a := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: unitVector(0), PlaytimeMinutes: 10}
	b := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: unitVector(1), PlaytimeMinutes: 10}
	first, second := BuildVectors([]Signal{a, b}, asOf), BuildVectors([]Signal{b, a}, asOf)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("vectors differ by input order")
	}
}

func TestPlaytimeRecencyWeightDecaysWithTwoYearHalfLife(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	if got := HistoricalRecencyWeight(asOf.Add(-2*365*24*time.Hour), asOf); math.Abs(got-.5) > 1e-9 {
		t.Fatalf("two year recency = %v, want .5", got)
	}
	if got := HistoricalRecencyWeight(asOf.Add(-10*365*24*time.Hour), asOf); math.Abs(got-1.0/32.0) > 1e-9 {
		t.Fatalf("ten year recency = %v, want 1/32", got)
	}
	if got := HistoricalRecencyWeight(time.Time{}, asOf); got != .25 {
		t.Fatalf("unknown recency = %v, want .25", got)
	}
}

func TestActivityWeightsPreserveCompletionAndRecencyFormulas(t *testing.T) {
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	completion := Signal{GameID: uuid.New(), Vector: unitVector(0), PlaytimeMinutes: 100, RecentMinutes: 50, AchievementRatio: 1, CompletionWeight: 1.85, LastPlayedAt: asOf}
	weight := CalculateSignalWeights([]Signal{completion}, asOf)[completion.GameID]
	if weight.Playtime != 1 || weight.Recent != 1.5 || weight.Completion != 1.85 || weight.Achievement != 0 || math.Abs(weight.Positive-4.35) > 1e-12 {
		t.Fatalf("combined weights = %#v", weight)
	}
	clamped := Signal{GameID: uuid.New(), Vector: unitVector(0), CompletionWeight: 99}
	if got := CalculateSignalWeights([]Signal{clamped}, asOf)[clamped.GameID]; got.Completion != 2.25 || got.Positive != 2.25 {
		t.Fatalf("clamped completion = %#v", got)
	}
	fresh := BuildVectors([]Signal{{GameID: uuid.New(), Vector: unitVector(0), PlaytimeMinutes: 10_000, LastPlayedAt: asOf}}, asOf)
	old := BuildVectors([]Signal{{GameID: uuid.New(), Vector: unitVector(0), PlaytimeMinutes: 10_000, LastPlayedAt: asOf.Add(-10 * 365 * 24 * time.Hour)}}, asOf)
	if fresh.Positive[0] != old.Positive[0] || math.Abs(fresh.PositiveStrength-1) > 1e-12 || math.Abs(old.PositiveStrength-1.0/32.0) > 1e-12 {
		t.Fatalf("fresh/old vector or strength = %v/%v, strength=%v/%v", fresh.Positive[0], old.Positive[0], fresh.PositiveStrength, old.PositiveStrength)
	}
}

func unitVector(index int) []float32 {
	vector := make([]float32, Dimensions)
	vector[index] = 1
	return vector
}
