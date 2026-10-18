package completion

import (
	"testing"
	"time"
)

func TestCalculateProjectionWeights(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	unknown := time.Time{}
	tests := []struct {
		name       string
		input      ProjectionInput
		ok         bool
		weight     float64
		count      int
		basis      string
		completion time.Time
	}{
		{name: "none"},
		{name: "hundred percent only", input: ProjectionInput{EverHundredPercent: true, HundredPercentAt: t1}, ok: true, weight: 1.50, basis: "hundred_percent", completion: t1},
		{name: "one ending", input: ProjectionInput{EndingUnlocks: []time.Time{t1}}, ok: true, weight: 1.50, count: 1, basis: "ending", completion: t1},
		{name: "two endings", input: ProjectionInput{EndingUnlocks: []time.Time{t1, t0}}, ok: true, weight: 1.85, count: 2, basis: "ending", completion: t0},
		{name: "three endings", input: ProjectionInput{EndingUnlocks: []time.Time{t1, t0, unknown}}, ok: true, weight: 2.05, count: 3, basis: "ending", completion: t0},
		{name: "four endings", input: ProjectionInput{EndingUnlocks: []time.Time{t1, t0, unknown, unknown}}, ok: true, weight: 2.15, count: 4, basis: "ending", completion: t0},
		{name: "five endings", input: ProjectionInput{EndingUnlocks: make([]time.Time, 5)}, ok: true, weight: 2.25, count: 5, basis: "ending"},
		{name: "fifty endings capped", input: ProjectionInput{EndingUnlocks: make([]time.Time, 50)}, ok: true, weight: 2.25, count: 50, basis: "ending"},
		{name: "both one", input: ProjectionInput{EverHundredPercent: true, HundredPercentAt: t0, EndingUnlocks: []time.Time{t1}}, ok: true, weight: 1.50, count: 1, basis: "both", completion: t1},
		{name: "both two", input: ProjectionInput{EverHundredPercent: true, HundredPercentAt: t0, EndingUnlocks: []time.Time{t1, unknown}}, ok: true, weight: 1.85, count: 2, basis: "both", completion: t1},
		{name: "unknown endings do not borrow hundred percent date", input: ProjectionInput{EverHundredPercent: true, HundredPercentAt: t0, EndingUnlocks: []time.Time{unknown}}, ok: true, weight: 1.50, count: 1, basis: "both"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := CalculateProjection(test.input)
			if ok != test.ok || got.Weight != test.weight || got.EndingCount != test.count || got.Basis != test.basis || !got.CompletionAt.Equal(test.completion) {
				t.Fatalf("projection=%+v ok=%v", got, ok)
			}
		})
	}
}
