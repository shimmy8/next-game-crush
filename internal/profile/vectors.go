package profile

import (
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

const Dimensions = 1536

type Signal struct {
	GameID               uuid.UUID
	Vector               []float32
	PlaytimeMinutes      int64
	RecentMinutes        int64
	LastPlayedAt         time.Time
	AchievementRatio     float64
	CompletionWeight     float64
	CompletionOverridden bool
	Wishlisted           bool
	Likes                int
	Dislikes             int
}

type Vectors struct {
	Positive         []float32
	Negative         []float32
	PositiveStrength float64
	Confidence       float64
	Evidence         int
}

type SignalWeights struct {
	Positive    float64
	Negative    float64
	Playtime    float64
	Recent      float64
	Achievement float64
	Completion  float64
	Wishlist    float64
	Feedback    float64
}

func CalculateSignalWeights(input []Signal, asOf time.Time) map[uuid.UUID]SignalWeights {
	maxPlaytime, maxRecent := signalMaxima(input)
	result := make(map[uuid.UUID]SignalWeights, len(input))
	for _, signal := range input {
		weights, ok := calculateSignalWeight(signal, maxPlaytime, maxRecent, asOf)
		if !ok {
			continue
		}
		result[signal.GameID] = weights
	}
	return result
}

func BuildVectors(input []Signal, asOf time.Time) Vectors {
	signals := append([]Signal(nil), input...)
	sort.SliceStable(signals, func(i, j int) bool { return signals[i].GameID.String() < signals[j].GameID.String() })
	weightsByGame := CalculateSignalWeights(signals, asOf)
	maxPlaytime, maxRecent := signalMaxima(signals)
	gameIDCounts := make(map[uuid.UUID]int, len(signals))
	for _, signal := range signals {
		gameIDCounts[signal.GameID]++
	}
	positive, negative := make([]float32, Dimensions), make([]float32, Dimensions)
	var positiveWeight, positivePotential, negativeWeight float64
	valid, signalKinds := 0, map[string]bool{}
	for _, signal := range signals {
		weights, ok := weightsByGame[signal.GameID]
		if gameIDCounts[signal.GameID] > 1 {
			weights, ok = calculateSignalWeight(signal, maxPlaytime, maxRecent, asOf)
		}
		if !ok {
			continue
		}
		valid++
		if weights.Playtime > 0 {
			signalKinds["playtime"] = true
			positivePotential += math.Log1p(float64(signal.PlaytimeMinutes)) / math.Log1p(float64(maxPlaytime))
		}
		if weights.Recent > 0 {
			signalKinds["recent"] = true
			positivePotential += 1.5 * float64(signal.RecentMinutes) / float64(maxRecent)
		}
		if weights.Achievement > 0 {
			signalKinds["achievements"] = true
			positivePotential += .5 * clamp(signal.AchievementRatio)
		}
		if weights.Completion > 0 {
			signalKinds["completion"] = true
			positivePotential += weights.Completion
		}
		if weights.Wishlist > 0 {
			signalKinds["wishlist"] = true
			positivePotential += weights.Wishlist
		}
		if weights.Feedback > 0 {
			signalKinds["feedback"] = true
			positivePotential += weights.Feedback
		}
		if weights.Positive > 0 {
			addWeighted(positive, signal.Vector, weights.Positive)
			positiveWeight += weights.Positive
		}
		if weights.Negative > 0 {
			addWeighted(negative, signal.Vector, weights.Negative)
			negativeWeight += weights.Negative
			signalKinds["feedback"] = true
		}
	}
	divide(positive, positiveWeight)
	divide(negative, negativeWeight)
	confidence := 0.0
	if valid > 0 && (positiveWeight > 0 || negativeWeight > 0) {
		coverage := float64(len(signalKinds)) / 6
		volume := math.Min(1, math.Log1p(float64(valid))/math.Log(11))
		confidence = clamp(.65*coverage + .35*volume)
	}
	positiveStrength := 0.0
	if positivePotential > 0 {
		positiveStrength = clamp(positiveWeight / positivePotential)
	}
	return Vectors{Positive: positive, Negative: negative, PositiveStrength: positiveStrength, Confidence: confidence, Evidence: valid}
}

func calculateSignalWeight(signal Signal, maxPlaytime, maxRecent int64, asOf time.Time) (SignalWeights, bool) {
	if !validVector(signal.Vector) {
		return SignalWeights{}, false
	}
	weights := SignalWeights{}
	recencyWeight := HistoricalRecencyWeight(signal.LastPlayedAt, asOf)
	if signal.PlaytimeMinutes > 0 && maxPlaytime > 0 {
		weights.Playtime = math.Log1p(float64(signal.PlaytimeMinutes)) / math.Log1p(float64(maxPlaytime)) * recencyWeight
		weights.Positive += weights.Playtime
	}
	if signal.RecentMinutes > 0 && maxRecent > 0 {
		weights.Recent = 1.5 * float64(signal.RecentMinutes) / float64(maxRecent)
		weights.Positive += weights.Recent
	}
	completion := clampCompletion(signal.CompletionWeight)
	if completion > 0 && !signal.CompletionOverridden {
		weights.Completion = completion
		weights.Positive += weights.Completion
	} else if ratio := clamp(signal.AchievementRatio); ratio > 0 {
		weights.Achievement = .5 * ratio * recencyWeight
		weights.Positive += weights.Achievement
	}
	if signal.Wishlisted {
		weights.Wishlist = 2
		weights.Positive += weights.Wishlist
	}
	if signal.Likes > 0 {
		weights.Feedback = 2 * float64(signal.Likes)
		weights.Positive += weights.Feedback
	}
	if signal.Dislikes > 0 {
		weights.Negative = 2 * float64(signal.Dislikes)
	}
	return weights, true
}

func clampCompletion(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 2.25 {
		return 2.25
	}
	return value
}

func signalMaxima(input []Signal) (int64, int64) {
	maxPlaytime, maxRecent := int64(0), int64(0)
	for _, signal := range input {
		if signal.PlaytimeMinutes > maxPlaytime {
			maxPlaytime = signal.PlaytimeMinutes
		}
		if signal.RecentMinutes > maxRecent {
			maxRecent = signal.RecentMinutes
		}
	}
	return maxPlaytime, maxRecent
}

func HistoricalRecencyWeight(lastPlayedAt, asOf time.Time) float64 {
	if lastPlayedAt.IsZero() {
		return .25
	}
	age := asOf.Sub(lastPlayedAt)
	if age <= 0 {
		return 1
	}
	const halfLife = 2 * 365 * 24 * time.Hour
	return math.Exp(-math.Ln2 * float64(age) / float64(halfLife))
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
