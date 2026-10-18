package recommendations

import (
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
)

const AlgorithmVersion = "ranking-v4-achievement-completion"

type Factors struct {
	NegativeModePreference float64 `json:"negativeModePreference"`
	PositiveSimilarity     float64 `json:"positiveSimilarity"`
	NegativeSimilarity     float64 `json:"negativeSimilarity"`
	Wishlist               float64 `json:"wishlist"`
	TagAffinity            float64 `json:"tagAffinity"`
	PlaytimeAffinity       float64 `json:"playtimeAffinity"`
	AchievementAffinity    float64 `json:"achievementAffinity"`
	RecentActivityAffinity float64 `json:"recentActivityAffinity"`
	CompletionAffinity     float64 `json:"completionAffinity"`
	PositiveFeedback       float64 `json:"positiveFeedback"`
	Novelty                float64 `json:"novelty"`
	NegativeFeedback       float64 `json:"negativeFeedback"`
	DateUncertainty        float64 `json:"dateUncertainty"`
	Confidence             float64 `json:"confidence"`
}

type Weights struct {
	NegativeModePreferenceWeight float64 `json:"negativeModePreferenceWeight"`
	SemanticSimilarity           float64 `json:"semanticSimilarity"`
	Wishlist                     float64 `json:"wishlist"`
	TagAffinity                  float64 `json:"tagAffinity"`
	PlaytimeAffinity             float64 `json:"playtimeAffinity"`
	AchievementAffinity          float64 `json:"achievementAffinity"`
	RecentActivityAffinity       float64 `json:"recentActivityAffinity"`
	CompletionAffinity           float64 `json:"completionAffinity"`
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
	Modes                             []preferences.Mode
	ModePreferenceEvidence            *Evidence
	GameID                            uuid.UUID
	Name                              string
	Owned, Released, ExplicitlyHidden bool
	ExcludedByLatestDirectAction      bool
	Dislikes                          int
	Factors                           Factors
	Release                           ReleaseWindow
	retrievalRank                     int
}

type EvidenceKind string

const (
	EvidenceFeatureOverlap         EvidenceKind = "feature_overlap"
	EvidenceSimilarGame            EvidenceKind = "similar_game"
	EvidencePlaytimeAnchor         EvidenceKind = "playtime_anchor"
	EvidenceRecentActivityAnchor   EvidenceKind = "recent_activity_anchor"
	EvidenceAchievementAnchor      EvidenceKind = "achievement_anchor"
	EvidenceCompletionAnchor       EvidenceKind = "completion_anchor"
	EvidencePositiveFeedback       EvidenceKind = "positive_feedback"
	EvidenceWishlistContext        EvidenceKind = "wishlist_context"
	EvidenceNegativeModePreference EvidenceKind = "negative_mode_preference"
)

type Evidence struct {
	ID   string       `json:"id"`
	Kind EvidenceKind `json:"kind"`
	Text string       `json:"text"`
}

type FeatureValues struct {
	Themes    []string `json:"themes"`
	Mechanics []string `json:"mechanics"`
	Modes     []string `json:"modes"`
	Tone      []string `json:"tone"`
}

type CandidateEvidence struct {
	GameID   uuid.UUID
	Vector   []float32
	Features FeatureValues
}

type EvidenceAnchor struct {
	Name     string
	Signal   profile.Signal
	Features FeatureValues
}

type EvidenceCorpus struct {
	Candidates map[uuid.UUID]CandidateEvidence
	Anchors    []EvidenceAnchor
}

type GroundingEvidence struct {
	ID            string       `json:"id"`
	Kind          EvidenceKind `json:"kind"`
	PublicText    string       `json:"publicText"`
	Strength      float64      `json:"strength"`
	SourceGameIDs []uuid.UUID  `json:"sourceGameIds,omitempty"`
}

type StoredEvidence struct {
	Version  int                 `json:"version"`
	Items    []GroundingEvidence `json:"items"`
	CitedIDs []string            `json:"citedIds,omitempty"`
}

type ExplanationSource string

const (
	ExplanationSourceAI            ExplanationSource = "ai"
	ExplanationSourceDeterministic ExplanationSource = "deterministic"
	ExplanationSourceNone          ExplanationSource = "none"
)

type ExplanationFailureReason string

const (
	ExplanationFailureDisabled            ExplanationFailureReason = "disabled"
	ExplanationFailureRateLimited         ExplanationFailureReason = "rate_limited"
	ExplanationFailureTimeout             ExplanationFailureReason = "timeout"
	ExplanationFailureProviderUnavailable ExplanationFailureReason = "provider_unavailable"
	ExplanationFailureRefusal             ExplanationFailureReason = "refusal"
	ExplanationFailureInvalidOutput       ExplanationFailureReason = "invalid_output"
	ExplanationFailureInvalidCitation     ExplanationFailureReason = "invalid_citation"
	ExplanationFailureUnsafeText          ExplanationFailureReason = "unsafe_text"
	ExplanationFailureUnknown             ExplanationFailureReason = "unknown"
)

type ExplanationResult struct {
	Text          string
	Source        ExplanationSource
	FailureReason ExplanationFailureReason
	EvidenceKeys  []string
}

type ExplanationRecord struct {
	Text          string
	Source        ExplanationSource
	FailureReason ExplanationFailureReason
	Supplied      []GroundingEvidence
	CitedIDs      []string
}

type ExplanationStats struct {
	AIGenerated    int                              `json:"aiGenerated"`
	Deterministic  int                              `json:"deterministic"`
	None           int                              `json:"none"`
	FailureReasons map[ExplanationFailureReason]int `json:"failureReasons"`
}

type Platforms struct {
	Windows bool `json:"windows"`
	Mac     bool `json:"mac"`
	Linux   bool `json:"linux"`
}

type DeckCompatibility string

const (
	DeckVerified    DeckCompatibility = "verified"
	DeckPlayable    DeckCompatibility = "playable"
	DeckUnsupported DeckCompatibility = "unsupported"
	DeckUnknown     DeckCompatibility = "unknown"
)

type Recommendation struct {
	ID                    uuid.UUID         `json:"id"`
	GameID                uuid.UUID         `json:"gameId"`
	SteamAppID            int64             `json:"steamAppId"`
	Name                  string            `json:"name"`
	CoverImageURL         string            `json:"coverImageUrl,omitempty"`
	StoreURL              string            `json:"storeUrl"`
	Platforms             *Platforms        `json:"platforms,omitempty"`
	DeckCompatibility     DeckCompatibility `json:"deckCompatibility"`
	ProfileVersion        int64             `json:"profileVersion"`
	RankingVersion        int64             `json:"rankingVersion"`
	AlgorithmVersion      string            `json:"algorithmVersion"`
	Score                 float64           `json:"score"`
	Confidence            float64           `json:"confidence"`
	Factors               Factors           `json:"factors"`
	Weights               Weights           `json:"weights"`
	Wishlisted            bool              `json:"wishlisted"`
	Release               ReleaseWindow     `json:"release"`
	Explanation           string            `json:"explanation"`
	Evidence              []Evidence        `json:"evidence"`
	ExplanationSource     ExplanationSource `json:"explanationSource"`
	GeneratedExplanation  bool              `json:"generatedExplanation"`
	LatestFeedbackAction  *string           `json:"latestFeedbackAction"`
	LatestFeedbackReasons []string          `json:"latestFeedbackReasons"`
	CreatedAt             time.Time         `json:"createdAt"`
	explanationLoaded     bool
}

type Calendar struct {
	ExactDates []Recommendation `json:"exactDates"`
	Month      []Recommendation `json:"month"`
	Quarter    []Recommendation `json:"quarter"`
	Year       []Recommendation `json:"year"`
	ComingSoon []Recommendation `json:"comingSoon"`
	TBA        []Recommendation `json:"tba"`
}
