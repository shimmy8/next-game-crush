package recommendations

import (
	"time"

	"github.com/google/uuid"
)

const AlgorithmVersion = "ranking-v1"

type Factors struct {
	PositiveSimilarity     float64 `json:"positiveSimilarity"`
	NegativeSimilarity     float64 `json:"negativeSimilarity"`
	Wishlist               float64 `json:"wishlist"`
	TagAffinity            float64 `json:"tagAffinity"`
	PlaytimeAffinity       float64 `json:"playtimeAffinity"`
	AchievementAffinity    float64 `json:"achievementAffinity"`
	RecentActivityAffinity float64 `json:"recentActivityAffinity"`
	PositiveFeedback       float64 `json:"positiveFeedback"`
	Novelty                float64 `json:"novelty"`
	NegativeFeedback       float64 `json:"negativeFeedback"`
	DateUncertainty        float64 `json:"dateUncertainty"`
	Confidence             float64 `json:"confidence"`
}

type Weights struct {
	SemanticSimilarity           float64 `json:"semanticSimilarity"`
	Wishlist                     float64 `json:"wishlist"`
	TagAffinity                  float64 `json:"tagAffinity"`
	PlaytimeAffinity             float64 `json:"playtimeAffinity"`
	AchievementAffinity          float64 `json:"achievementAffinity"`
	RecentActivityAffinity       float64 `json:"recentActivityAffinity"`
	PositiveFeedback             float64 `json:"positiveFeedback"`
	Novelty                      float64 `json:"novelty"`
	NegativeFeedback             float64 `json:"negativeFeedback"`
	DateUncertainty              float64 `json:"dateUncertainty"`
	NegativeSimilarityMultiplier float64 `json:"negativeSimilarityMultiplier"`
}

type Result struct {
	Score            float64 `json:"score"`
	SemanticAffinity float64 `json:"semanticAffinity"`
}

type ReleaseWindow struct {
	Precision string `json:"precision"`
	ExactDate string `json:"exactDate,omitempty"`
	Raw       string `json:"raw,omitempty"`
}

type Candidate struct {
	GameID                            uuid.UUID
	Name                              string
	Owned, Released, ExplicitlyHidden bool
	Dislikes                          int
	Factors                           Factors
	Release                           ReleaseWindow
	retrievalRank                     int
}

type Evidence struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Recommendation struct {
	ID                   uuid.UUID     `json:"id"`
	GameID               uuid.UUID     `json:"gameId"`
	Name                 string        `json:"name"`
	ProfileVersion       int64         `json:"profileVersion"`
	RankingVersion       int64         `json:"rankingVersion"`
	AlgorithmVersion     string        `json:"algorithmVersion"`
	Score                float64       `json:"score"`
	Confidence           float64       `json:"confidence"`
	Factors              Factors       `json:"factors"`
	Weights              Weights       `json:"weights"`
	Release              ReleaseWindow `json:"release"`
	Explanation          string        `json:"explanation"`
	Evidence             []Evidence    `json:"evidence"`
	GeneratedExplanation bool          `json:"generatedExplanation"`
	CreatedAt            time.Time     `json:"createdAt"`
}

type Calendar struct {
	ExactDates []Recommendation `json:"exactDates"`
	Month      []Recommendation `json:"month"`
	Quarter    []Recommendation `json:"quarter"`
	Year       []Recommendation `json:"year"`
	ComingSoon []Recommendation `json:"comingSoon"`
	TBA        []Recommendation `json:"tba"`
}
