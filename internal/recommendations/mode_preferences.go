package recommendations

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"
	"next-game-crush/internal/preferences"
)

type RankingInputs struct {
	Source               RankingSource
	Candidates           []Candidate
	EffectiveAdjustments []preferences.Adjustment
	Corpus               EvidenceCorpus
	// ActiveAdjustments is retained for intermediate callers; new ranking code
	// uses EffectiveAdjustments from the same repeatable-read snapshot.
	ActiveAdjustments []preferences.Adjustment
}

func modeStrength(candidateID uuid.UUID, modes []preferences.Mode, a preferences.Adjustment) float64 {
	if !a.Active || a.SourceGameID == candidateID || a.Dimension != "mode" || a.Direction != "avoid" || !preferences.MatchesMode(a.Target, modes) || a.Confidence <= 0 || a.Strength <= 0 || math.IsNaN(a.Confidence) || math.IsInf(a.Confidence, 0) || math.IsNaN(a.Strength) || math.IsInf(a.Strength, 0) {
		return 0
	}
	return clamp(a.Confidence * a.Strength)
}
func ModePreferenceFactor(candidateID uuid.UUID, modes []preferences.Mode, active []preferences.Adjustment) float64 {
	var maximum float64
	for _, a := range active {
		if value := modeStrength(candidateID, modes, a); value > maximum {
			maximum = value
		}
	}
	return maximum
}
func ModePreferenceEvidence(candidateID uuid.UUID, modes []preferences.Mode, active []preferences.Adjustment) *Evidence {
	var winner *preferences.Adjustment
	var maximum float64
	for _, a := range active {
		value := modeStrength(candidateID, modes, a)
		if value > 0 && (winner == nil || value > maximum || value == maximum && (a.Target < winner.Target || a.Target == winner.Target && a.ID.String() < winner.ID.String())) {
			copy := a
			winner = &copy
			maximum = value
		}
	}
	if winner == nil {
		return nil
	}
	return &Evidence{ID: "negative-mode-preference:" + string(winner.Target), Kind: EvidenceNegativeModePreference, Text: preferences.ModeEvidence(winner.Target)}
}
func ActiveAdjustmentsDigest(active []preferences.Adjustment) string {
	type tuple struct {
		ID, Feedback, Game, Dimension, Target, Direction string
		Confidence, Strength                             float64
	}
	values := make([]tuple, 0, len(active))
	for _, a := range active {
		if a.Active {
			values = append(values, tuple{a.ID.String(), a.SourceFeedbackID.String(), a.SourceGameID.String(), a.Dimension, string(a.Target), a.Direction, a.Confidence, a.Strength})
		}
	}
	sort.Slice(values, func(i, j int) bool {
		a, _ := json.Marshal(values[i])
		b, _ := json.Marshal(values[j])
		return string(a) < string(b)
	})
	encoded, _ := json.Marshal(values)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}
func RankingBuildKey(requestKey, algorithmVersion string, source RankingSource) string {
	digest := rankingSourceDigest(source)
	return fmt.Sprintf("pair-v2:%x:%s:%s", sha256.Sum256([]byte(requestKey)), algorithmVersion, digest)
}

func rankingSourceDigest(source RankingSource) string {
	payload, _ := json.Marshal(struct {
		ProfileID, ProfileAlgorithm, ProfileBuildKey, ProfileInputHash string
		ProfileVersion                                                 int64
		OverlayID, OverlayAlgorithm, OverlayBuildKey, OverlayInputHash string
		OverlayVersion                                                 int64
	}{source.Activity.ID.String(), source.ProfileAlgorithmVersion, source.ProfileBuildKey, source.ProfileInputContentHash, source.Activity.Version,
		source.Overlay.ID.String(), source.Overlay.AlgorithmVersion, source.Overlay.BuildKey, source.Overlay.InputContentHash, source.Overlay.Version})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func noVectorProfileIdentity(id uuid.UUID, version int64, algorithmVersion, buildKey string) string {
	payload, _ := json.Marshal(struct {
		ID, AlgorithmVersion, BuildKey string
		Version                        int64
	}{id.String(), algorithmVersion, buildKey, version})
	return "no-vector-v1:" + fmt.Sprintf("%x", sha256.Sum256(payload))
}
func rankingRequestPrefix(key string) string {
	fields := strings.Split(key, ":")
	if len(fields) == 4 && strings.HasPrefix(fields[0], "pair-v") && len(fields[1]) == 64 {
		return strings.Join(fields[:3], ":") + ":"
	}
	return ""
}

func modeEvidence(r Recommendation) *Evidence {
	if clamp(r.Factors.NegativeModePreference) == 0 {
		return nil
	}
	for _, item := range r.Evidence {
		target := preferences.Mode(strings.TrimPrefix(item.ID, "negative-mode-preference:"))
		if strings.HasPrefix(item.ID, "negative-mode-preference:") && preferences.ValidMode(target) && item.Text == preferences.ModeEvidence(target) {
			copy := item
			return &copy
		}
	}
	return &Evidence{ID: "negative-mode-preference", Kind: EvidenceNegativeModePreference, Text: "Учитываем ваши пожелания реже предлагать игры такого режима"}
}

func validModeEvidence(id, text string) bool {
	if id == "negative-mode-preference" {
		return text == "Учитываем ваши пожелания реже предлагать игры такого режима"
	}
	if !strings.HasPrefix(id, "negative-mode-preference:") {
		return false
	}
	target := preferences.Mode(strings.TrimPrefix(id, "negative-mode-preference:"))
	return preferences.ValidMode(target) && text == preferences.ModeEvidence(target)
}
