package completion

import (
	"math"
	"time"
)

const ProjectionAlgorithmVersion = "completion-v1"

type ProjectionInput struct {
	EverHundredPercent bool
	HundredPercentAt   time.Time
	EndingUnlocks      []time.Time
}

type Projection struct {
	EndingCount         int
	HundredPercentBasis bool
	Weight              float64
	CompletionAt        time.Time
	Basis               string
}

func CalculateProjection(input ProjectionInput) (Projection, bool) {
	endingCount := len(input.EndingUnlocks)
	if !input.EverHundredPercent && endingCount == 0 {
		return Projection{}, false
	}
	endingWeight := 0.0
	switch endingCount {
	case 0:
	case 1:
		endingWeight = 1.50
	case 2:
		endingWeight = 1.85
	case 3:
		endingWeight = 2.05
	default:
		endingWeight = math.Min(2.25, 2.05+0.10*float64(endingCount-3))
	}
	hundredPercentWeight := 0.0
	if input.EverHundredPercent {
		hundredPercentWeight = 1.50
	}
	projection := Projection{
		EndingCount: endingCount, HundredPercentBasis: input.EverHundredPercent,
		Weight: math.Max(endingWeight, hundredPercentWeight),
	}
	switch {
	case input.EverHundredPercent && endingCount > 0:
		projection.Basis = "both"
	case endingCount > 0:
		projection.Basis = "ending"
	default:
		projection.Basis = "hundred_percent"
	}
	if endingCount > 0 {
		for _, unlockedAt := range input.EndingUnlocks {
			if unlockedAt.IsZero() {
				continue
			}
			if projection.CompletionAt.IsZero() || unlockedAt.Before(projection.CompletionAt) {
				projection.CompletionAt = unlockedAt.UTC()
			}
		}
	} else if !input.HundredPercentAt.IsZero() {
		projection.CompletionAt = input.HundredPercentAt.UTC()
	}
	return projection, true
}
