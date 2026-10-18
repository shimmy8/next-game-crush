package recommendations

import (
	"math"
	"sort"
	"strings"
)

func DefaultWeights() Weights {
	return Weights{SemanticSimilarity: 25, Wishlist: 0, TagAffinity: 15, PlaytimeAffinity: 10, AchievementAffinity: 5, RecentActivityAffinity: 5, CompletionAffinity: 5, PositiveFeedback: 20, Novelty: 5, NegativeFeedback: -30, DateUncertainty: -5, NegativeSimilarityMultiplier: .5, NegativeModePreferenceWeight: -5}
}

func Score(f Factors, w Weights) Result {
	affinity := clamp(f.PositiveSimilarity) - w.NegativeSimilarityMultiplier*clamp(f.NegativeSimilarity)
	if affinity < 0 {
		affinity = 0
	}
	raw := affinity*w.SemanticSimilarity + clamp(f.Wishlist)*w.Wishlist + clamp(f.TagAffinity)*w.TagAffinity + clamp(f.PlaytimeAffinity)*w.PlaytimeAffinity + clamp(f.AchievementAffinity)*w.AchievementAffinity + clamp(f.RecentActivityAffinity)*w.RecentActivityAffinity + clamp(f.CompletionAffinity)*w.CompletionAffinity + clamp(f.PositiveFeedback)*w.PositiveFeedback + clamp(f.Novelty)*w.Novelty + clamp(f.NegativeFeedback)*w.NegativeFeedback + clamp(f.DateUncertainty)*w.DateUncertainty
	raw += clamp(f.NegativeModePreference) * w.NegativeModePreferenceWeight
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		raw = 0
	}
	if raw < 0 {
		raw = 0
	}
	if raw > 100 {
		raw = 100
	}
	return Result{Score: math.Round(raw*1000) / 1000, SemanticAffinity: affinity}
}
func Eligible(candidate Candidate) bool {
	return candidate.GameID != uuidNil && !candidate.Owned && !candidate.Released && !candidate.ExplicitlyHidden && !candidate.ExcludedByLatestDirectAction
}
func Sort(input []Recommendation) []Recommendation {
	result := append([]Recommendation(nil), input...)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		left, right := strings.ToLower(result[i].Name), strings.ToLower(result[j].Name)
		if left != right {
			return left < right
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].GameID.String() < result[j].GameID.String()
	})
	return result
}
func BuildCalendar(items []Recommendation) Calendar {
	var result Calendar
	for _, item := range Sort(items) {
		item.Wishlisted = item.Wishlisted || clamp(item.Factors.Wishlist) > 0
		switch item.Release.Precision {
		case "day":
			if item.Release.ExactDate != "" {
				result.ExactDates = append(result.ExactDates, item)
			} else {
				result.TBA = append(result.TBA, item)
			}
		case "month":
			result.Month = append(result.Month, item)
		case "quarter":
			result.Quarter = append(result.Quarter, item)
		case "year":
			result.Year = append(result.Year, item)
		case "coming_soon":
			result.ComingSoon = append(result.ComingSoon, item)
		default:
			result.TBA = append(result.TBA, item)
		}
	}
	return result
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

var uuidNil = [16]byte{}
