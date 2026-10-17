package ai

import "errors"

const EmbeddingDimensions = 1536

var (
	ErrCapabilityDisabled error = CapabilityDisabledError{}
	ErrInvalidInput             = errors.New("invalid AI input")
	ErrInvalidEvidence          = errors.New("AI output cites unknown evidence")
	ErrInvalidOutput            = errors.New("invalid structured AI output")
	ErrInvalidEmbedding         = errors.New("invalid embedding response")
	ErrRefusal                  = errors.New("OpenAI refused the request")
	ErrIncomplete               = errors.New("OpenAI response was incomplete")
	ErrHTTP                     = errors.New("OpenAI HTTP request failed")
)

type CapabilityDisabledError struct{}

func (CapabilityDisabledError) Error() string { return "OpenAI capability is disabled" }
func (CapabilityDisabledError) Is(target error) bool {
	_, ok := target.(CapabilityDisabledError)
	return ok
}

// EvidenceItem is an already-normalized fact paired with an opaque, local key.
// The key may be returned by the model to ground an output; it must not contain
// a SteamID, database identifier, email address, or credential.
type EvidenceItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type GameEvidence struct {
	EvidenceKey string         `json:"evidence_key"`
	Name        string         `json:"name"`
	Evidence    []EvidenceItem `json:"evidence"`
}

type GameFeatures struct {
	Themes       []string `json:"themes"`
	Mechanics    []string `json:"mechanics"`
	Modes        []string `json:"modes"`
	Tone         []string `json:"tone"`
	EvidenceKeys []string `json:"evidence_keys"`
}

type ProfileEvidence struct {
	EvidenceKey string         `json:"evidence_key"`
	Evidence    []EvidenceItem `json:"evidence"`
}

type ProfileSummary struct {
	Summary      string   `json:"summary"`
	Preferences  []string `json:"preferences"`
	Avoidances   []string `json:"avoidances"`
	Uncertainty  string   `json:"uncertainty"`
	EvidenceKeys []string `json:"evidence_keys"`
}

type RecommendationEvidence struct {
	EvidenceKey string         `json:"evidence_key"`
	GameName    string         `json:"game_name"`
	Evidence    []EvidenceItem `json:"evidence"`
}

type Explanation struct {
	Text         string   `json:"text"`
	Facts        []string `json:"facts"`
	Inferences   []string `json:"inferences"`
	Uncertainty  string   `json:"uncertainty"`
	EvidenceKeys []string `json:"evidence_keys"`
	Generated    bool     `json:"-"`
}

type FeedbackText struct {
	EvidenceKey string `json:"evidence_key"`
	Text        string `json:"text"`
}

type FeedbackClassification struct {
	Taxonomy     string   `json:"taxonomy"`
	Polarity     string   `json:"polarity"`
	Confidence   float64  `json:"confidence"`
	Actionable   bool     `json:"actionable"`
	EvidenceKeys []string `json:"evidence_keys"`
}
