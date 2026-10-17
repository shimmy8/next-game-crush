package profile

import (
	"math"
	"sort"

	"github.com/google/uuid"
)

const Dimensions = 1536

type Signal struct {
	GameID           uuid.UUID
	Vector           []float32
	PlaytimeMinutes  int64
	RecentMinutes    int64
	AchievementRatio float64
	Wishlisted       bool
	Likes            int
	Dislikes         int
}

type Vectors struct {
	Positive   []float32
	Negative   []float32
	Confidence float64
	Evidence   int
}

func BuildVectors(input []Signal) Vectors {
	signals := append([]Signal(nil), input...)
	sort.SliceStable(signals, func(i, j int) bool { return signals[i].GameID.String() < signals[j].GameID.String() })
	positive, negative := make([]float32, Dimensions), make([]float32, Dimensions)
	maxPlaytime, maxRecent := int64(0), int64(0)
	for _, signal := range signals {
		if signal.PlaytimeMinutes > maxPlaytime {
			maxPlaytime = signal.PlaytimeMinutes
		}
		if signal.RecentMinutes > maxRecent {
			maxRecent = signal.RecentMinutes
		}
	}
	var positiveWeight, negativeWeight float64
	valid, signalKinds := 0, map[string]bool{}
	for _, signal := range signals {
		if !validVector(signal.Vector) {
			continue
		}
		valid++
		weight := 0.0
		if signal.PlaytimeMinutes > 0 && maxPlaytime > 0 {
			weight += math.Log1p(float64(signal.PlaytimeMinutes)) / math.Log1p(float64(maxPlaytime))
			signalKinds["playtime"] = true
		}
		if signal.RecentMinutes > 0 && maxRecent > 0 {
			weight += 1.5 * float64(signal.RecentMinutes) / float64(maxRecent)
			signalKinds["recent"] = true
		}
		if ratio := clamp(signal.AchievementRatio); ratio > 0 {
			weight += .5 * ratio
			signalKinds["achievements"] = true
		}
		if signal.Wishlisted {
			weight += 2
			signalKinds["wishlist"] = true
		}
		if signal.Likes > 0 {
			weight += 2 * float64(signal.Likes)
			signalKinds["feedback"] = true
		}
		if weight > 0 {
			addWeighted(positive, signal.Vector, weight)
			positiveWeight += weight
		}
		if signal.Dislikes > 0 {
			weight = 2 * float64(signal.Dislikes)
			addWeighted(negative, signal.Vector, weight)
			negativeWeight += weight
			signalKinds["feedback"] = true
		}
	}
	divide(positive, positiveWeight)
	divide(negative, negativeWeight)
	confidence := 0.0
	if valid > 0 && (positiveWeight > 0 || negativeWeight > 0) {
		coverage := float64(len(signalKinds)) / 5
		volume := math.Min(1, math.Log1p(float64(valid))/math.Log(11))
		confidence = clamp(.65*coverage + .35*volume)
	}
	return Vectors{Positive: positive, Negative: negative, Confidence: confidence, Evidence: valid}
}

func Cosine(a, b []float32) float64 {
	if len(a) != Dimensions || len(b) != Dimensions {
		return 0
	}
	var dot, aa, bb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
			return 0
		}
		dot, aa, bb = dot+x*y, aa+x*x, bb+y*y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / math.Sqrt(aa*bb)
}

func validVector(vector []float32) bool {
	if len(vector) != Dimensions {
		return false
	}
	nonzero := false
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
		nonzero = nonzero || value != 0
	}
	return nonzero
}
func addWeighted(dst, src []float32, weight float64) {
	for i := range dst {
		dst[i] += float32(float64(src[i]) * weight)
	}
}
func divide(vector []float32, weight float64) {
	if weight > 0 {
		for i := range vector {
			vector[i] /= float32(weight)
		}
	}
}
func clamp(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
