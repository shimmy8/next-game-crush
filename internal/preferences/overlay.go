package preferences

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

const (
	OverlayAlgorithmVersion = "preference-overlay-v1"
	OverlayJobKind          = "preferences.rebuild"
	OverlayDimensions       = 1536
)

type DirectReaction struct {
	FeedbackID, GameID uuid.UUID
	Action             string
	CreatedAt          time.Time
	EmbeddingModel     string
	EmbeddingHash      string
	Vector             []float32
}

type OverlaySnapshot struct {
	Reactions   []DirectReaction
	Corrections []Adjustment
}

type OverlayVectors struct {
	Positive, Negative                               []float32
	PositiveSources, NegativeSources, MissingVectors int
	EffectiveCorrections                             int
	InputContentHash                                 string
}

type OverlayMetadata struct {
	PositiveSources      int `json:"positiveSources"`
	NegativeSources      int `json:"negativeSources"`
	MissingVectors       int `json:"missingVectors"`
	EffectiveCorrections int `json:"effectiveCorrections"`
}

type Overlay struct {
	ID, UserID                                   uuid.UUID
	Version                                      int64
	AlgorithmVersion, BuildKey, InputContentHash string
	Positive, Negative                           []float32
	Metadata                                     OverlayMetadata
	CreatedAt                                    time.Time
}

type canonicalReaction struct {
	FeedbackID     string `json:"feedbackId"`
	GameID         string `json:"gameId"`
	Action         string `json:"action"`
	EmbeddingModel string `json:"embeddingModel"`
	EmbeddingHash  string `json:"embeddingHash"`
}

type canonicalCorrection struct {
	Dimension string  `json:"dimension"`
	Target    string  `json:"target"`
	Direction string  `json:"direction"`
	Effect    float64 `json:"effect"`
}

type canonicalOverlay struct {
	AlgorithmVersion string                `json:"algorithmVersion"`
	Reactions        []canonicalReaction   `json:"reactions"`
	Corrections      []canonicalCorrection `json:"corrections"`
}

func BuildOverlay(snapshot OverlaySnapshot) OverlayVectors {
	reactions := append([]DirectReaction(nil), snapshot.Reactions...)
	sort.Slice(reactions, func(i, j int) bool {
		if reactions[i].GameID != reactions[j].GameID {
			return reactions[i].GameID.String() < reactions[j].GameID.String()
		}
		return reactions[i].FeedbackID.String() < reactions[j].FeedbackID.String()
	})
	positiveSum, negativeSum := make([]float64, OverlayDimensions), make([]float64, OverlayDimensions)
	canonicalReactions := make([]canonicalReaction, 0, len(reactions))
	result := OverlayVectors{}
	for _, reaction := range reactions {
		if reaction.Action != "like" && reaction.Action != "dislike" && reaction.Action != "exclude" {
			continue
		}
		canonicalReactions = append(canonicalReactions, canonicalReaction{
			FeedbackID: reaction.FeedbackID.String(), GameID: reaction.GameID.String(), Action: reaction.Action, EmbeddingModel: reaction.EmbeddingModel, EmbeddingHash: reaction.EmbeddingHash,
		})
		if !validOverlayVector(reaction.Vector) {
			result.MissingVectors++
			continue
		}
		if reaction.Action == "like" {
			addOverlayVector(positiveSum, reaction.Vector)
			result.PositiveSources++
		} else {
			addOverlayVector(negativeSum, reaction.Vector)
			result.NegativeSources++
		}
	}
	result.Positive = overlayCentroid(positiveSum, result.PositiveSources)
	result.Negative = overlayCentroid(negativeSum, result.NegativeSources)

	correctionEffects := map[string]canonicalCorrection{}
	for _, correction := range snapshot.Corrections {
		if !correction.Active || correction.Dimension == "" || correction.Target == "" || correction.Direction == "" {
			continue
		}
		effect := correction.Confidence * correction.Strength
		if math.IsNaN(effect) || math.IsInf(effect, 0) || effect <= 0 {
			continue
		}
		if effect > 1 {
			effect = 1
		}
		key := correction.Dimension + "\x00" + string(correction.Target) + "\x00" + correction.Direction
		current, exists := correctionEffects[key]
		if !exists || effect > current.Effect {
			correctionEffects[key] = canonicalCorrection{Dimension: correction.Dimension, Target: string(correction.Target), Direction: correction.Direction, Effect: effect}
		}
	}
	canonicalCorrections := make([]canonicalCorrection, 0, len(correctionEffects))
	for _, correction := range correctionEffects {
		canonicalCorrections = append(canonicalCorrections, correction)
	}
	sort.Slice(canonicalCorrections, func(i, j int) bool {
		if canonicalCorrections[i].Dimension != canonicalCorrections[j].Dimension {
			return canonicalCorrections[i].Dimension < canonicalCorrections[j].Dimension
		}
		if canonicalCorrections[i].Target != canonicalCorrections[j].Target {
			return canonicalCorrections[i].Target < canonicalCorrections[j].Target
		}
		return canonicalCorrections[i].Direction < canonicalCorrections[j].Direction
	})
	result.EffectiveCorrections = len(canonicalCorrections)
	encoded, _ := json.Marshal(canonicalOverlay{AlgorithmVersion: OverlayAlgorithmVersion, Reactions: canonicalReactions, Corrections: canonicalCorrections})
	digest := sha256.Sum256(encoded)
	result.InputContentHash = hex.EncodeToString(digest[:])
	return result
}

func validOverlayVector(vector []float32) bool {
	if len(vector) != OverlayDimensions {
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

func addOverlayVector(sum []float64, vector []float32) {
	for index, value := range vector {
		sum[index] += float64(value)
	}
}

func overlayCentroid(sum []float64, sources int) []float32 {
	if sources == 0 {
		return nil
	}
	result := make([]float32, len(sum))
	nonzero := false
	for index, value := range sum {
		result[index] = float32(value / float64(sources))
		nonzero = nonzero || result[index] != 0
	}
	if !nonzero {
		return nil
	}
	return result
}
