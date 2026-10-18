package recommendations

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"next-game-crush/internal/profile"
)

const (
	StoredEvidenceVersion = 3
	maxEvidenceValueRunes = 120
	maxPublicTextRunes    = 240
	neutralExplanation    = "Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно."
)

var evidenceKindPriority = map[EvidenceKind]int{
	EvidencePositiveFeedback:     0,
	EvidenceFeatureOverlap:       1,
	EvidenceSimilarGame:          2,
	EvidenceRecentActivityAnchor: 3,
	EvidenceCompletionAnchor:     4,
	EvidencePlaytimeAnchor:       5,
	EvidenceAchievementAnchor:    6,
}

var privateEvidencePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`\b7656119[0-9]{10}\b`),
	regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b`),
	regexp.MustCompile(`(?i)\b[0-9a-f]{32,64}\b`),
	regexp.MustCompile(`\b[0-9]{16,}\b`),
	regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`),
	regexp.MustCompile(`(?i)\b(?:api[_ -]?key|session(?:[_ -]?id)?|cookie|authorization)\s*[:=]\s*[^\s,;]+`),
}

// BuildGroundingEvidence derives displayable facts from the same immutable
// signals that support ranking. It deliberately does not expose novelty: the
// current constant novelty factor is not evidence of a user preference.
func BuildGroundingEvidence(r Recommendation, candidate CandidateEvidence, anchors []EvidenceAnchor, asOf time.Time) []GroundingEvidence {
	weights := signalWeights(anchors, asOf)
	result := make([]GroundingEvidence, 0, 7)

	if values, sourceIDs := overlappingFeatures(candidate.GameID, candidate.Features, anchors, weights); len(values) > 0 {
		result = append(result, GroundingEvidence{
			ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap,
			PublicText: fmt.Sprintf("Совпадают темы и механики: %s.", strings.Join(values, ", ")),
			Strength:   factorContribution(r.Factors.TagAffinity, r.Weights.TagAffinity), SourceGameIDs: sourceIDs,
		})
	}
	if names, sourceIDs := similarAnchors(candidate.GameID, candidate.Vector, anchors, weights); len(names) > 0 {
		result = append(result, GroundingEvidence{
			ID: "similar-game-1", Kind: EvidenceSimilarGame,
			PublicText: fmt.Sprintf("Игры с похожим профилем: %s.", strings.Join(names, ", ")),
			Strength:   semanticContribution(r), SourceGameIDs: sourceIDs,
		})
	}
	if anchor, ok := bestComponentAnchor(candidate.GameID, candidate.Features, anchors, weights, func(w profile.SignalWeights) float64 { return w.Playtime }); ok {
		result = append(result, GroundingEvidence{ID: "playtime-anchor-1", Kind: EvidencePlaytimeAnchor, PublicText: fmt.Sprintf("Есть сходство с игрой %s, которой вы уделили много времени.", anchor.Name), Strength: factorContribution(r.Factors.PlaytimeAffinity, r.Weights.PlaytimeAffinity), SourceGameIDs: []uuid.UUID{anchor.Signal.GameID}})
	}
	if anchor, ok := bestComponentAnchor(candidate.GameID, candidate.Features, anchors, weights, func(w profile.SignalWeights) float64 { return w.Recent }); ok {
		result = append(result, GroundingEvidence{ID: "recent-activity-anchor-1", Kind: EvidenceRecentActivityAnchor, PublicText: fmt.Sprintf("Есть сходство с игрой %s, которую вы запускали недавно.", anchor.Name), Strength: factorContribution(r.Factors.RecentActivityAffinity, r.Weights.RecentActivityAffinity), SourceGameIDs: []uuid.UUID{anchor.Signal.GameID}})
	}
	if anchor, ok := bestCompletionAnchor(candidate, anchors, weights); ok {
		strength := factorContribution(r.Factors.CompletionAffinity, r.Weights.CompletionAffinity)
		if semantic := semanticContribution(r); semantic > strength {
			strength = semantic
		}
		result = append(result, GroundingEvidence{ID: "completion-anchor-1", Kind: EvidenceCompletionAnchor, PublicText: fmt.Sprintf("Есть сходство с пройденной вами игрой %s.", anchor.Name), Strength: strength, SourceGameIDs: []uuid.UUID{anchor.Signal.GameID}})
	}
	if anchor, ok := bestComponentAnchor(candidate.GameID, candidate.Features, anchors, weights, func(w profile.SignalWeights) float64 { return w.Achievement }); ok {
		result = append(result, GroundingEvidence{ID: "achievement-anchor-1", Kind: EvidenceAchievementAnchor, PublicText: fmt.Sprintf("Есть сходство с игрой %s, где у вас заметный прогресс достижений.", anchor.Name), Strength: factorContribution(r.Factors.AchievementAffinity, r.Weights.AchievementAffinity), SourceGameIDs: []uuid.UUID{anchor.Signal.GameID}})
	}
	if sourceIDs := positiveFeedbackSources(candidate.GameID, anchors, weights); len(sourceIDs) > 0 {
		result = append(result, GroundingEvidence{ID: "positive-feedback-1", Kind: EvidencePositiveFeedback, PublicText: "Вы уже отмечали эту игру как понравившуюся.", Strength: factorContribution(r.Factors.PositiveFeedback, r.Weights.PositiveFeedback), SourceGameIDs: sourceIDs})
	}
	if clamp(r.Factors.Wishlist) > 0 {
		result = append(result, GroundingEvidence{ID: "wishlist-context-1", Kind: EvidenceWishlistContext, PublicText: "Вы уже добавили эту игру в желаемое — следим за датой выхода.", Strength: factorContribution(r.Factors.Wishlist, r.Weights.Wishlist), SourceGameIDs: []uuid.UUID{candidate.GameID}})
	}
	if correction := modeEvidence(r); correction != nil {
		result = append(result, GroundingEvidence{ID: correction.ID, Kind: EvidenceNegativeModePreference, PublicText: correction.Text, Strength: clamp(r.Factors.NegativeModePreference)})
	}
	return result
}

func SelectGroundingEvidence(input []GroundingEvidence) []GroundingEvidence {
	valid := make([]GroundingEvidence, 0, len(input))
	var wishlist *GroundingEvidence
	var correction *GroundingEvidence
	strongest := 0.0
	seenIDs := map[string]bool{}
	for _, item := range input {
		if !validGroundingEvidence(item) || seenIDs[item.ID] {
			continue
		}
		seenIDs[item.ID] = true
		if item.Kind == EvidenceNegativeModePreference {
			if correction == nil || item.ID < correction.ID {
				copy := item
				correction = &copy
			}
			continue
		}
		if item.Kind == EvidenceWishlistContext {
			if wishlist == nil || evidenceLess(item, *wishlist) {
				copy := item
				wishlist = &copy
			}
			continue
		}
		if item.Strength >= .5 {
			valid = append(valid, item)
			if item.Strength > strongest {
				strongest = item.Strength
			}
		}
	}
	threshold := strongest * .05
	filtered := valid[:0]
	for _, item := range valid {
		if item.Strength >= threshold {
			filtered = append(filtered, item)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return evidenceLess(filtered[i], filtered[j]) })

	selected := make([]GroundingEvidence, 0, 3)
	limit := 2
	if correction != nil && wishlist != nil {
		limit = 1
	}
	seenKinds := map[EvidenceKind]bool{}
	for _, item := range filtered {
		if len(selected) == limit {
			break
		}
		if !seenKinds[item.Kind] {
			selected = append(selected, item)
			seenKinds[item.Kind] = true
		}
	}
	if len(selected) < limit {
		for _, item := range filtered {
			if len(selected) == limit {
				break
			}
			if !containsEvidenceID(selected, item.ID) {
				selected = append(selected, item)
			}
		}
	}
	if wishlist != nil {
		selected = append(selected, *wishlist)
	}
	if correction != nil {
		selected = append(selected, *correction)
	}
	return selected
}

func PublicEvidence(input []GroundingEvidence) []Evidence {
	result := make([]Evidence, 0, len(input))
	seen := map[string]bool{}
	for _, item := range input {
		if !validGroundingEvidence(item) || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		result = append(result, Evidence{ID: item.ID, Kind: item.Kind, Text: strings.TrimSpace(item.PublicText)})
	}
	return result
}

func DeterministicExplanation(selected []GroundingEvidence) string {
	selected = SelectGroundingEvidence(selected)
	parts := make([]string, 0, 2)
	wishlist := false
	for _, item := range selected {
		if item.Kind == EvidenceWishlistContext {
			wishlist = true
			continue
		}
		if len(parts) < 2 {
			parts = append(parts, strings.TrimSpace(item.PublicText))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if wishlist {
		return "Вы уже добавили эту игру в желаемое — следим за датой выхода."
	}
	return neutralExplanation
}

func signalWeights(anchors []EvidenceAnchor, asOf time.Time) map[uuid.UUID]profile.SignalWeights {
	signals := make([]profile.Signal, 0, len(anchors))
	for _, anchor := range anchors {
		signals = append(signals, anchor.Signal)
	}
	return profile.CalculateSignalWeights(signals, asOf)
}

type rankedValue struct {
	value     string
	strength  float64
	sourceIDs []uuid.UUID
}

func overlappingFeatures(candidateID uuid.UUID, candidate FeatureValues, anchors []EvidenceAnchor, weights map[uuid.UUID]profile.SignalWeights) ([]string, []uuid.UUID) {
	candidateValues := normalizedFeatures(candidate)
	ranked := map[string]*rankedValue{}
	for _, anchor := range anchors {
		if candidateID != uuid.Nil && anchor.Signal.GameID == candidateID {
			continue
		}
		weight, ok := positiveAnchorWeight(anchor, weights)
		if !ok {
			continue
		}
		anchorValues := normalizedFeatures(anchor.Features)
		for key, value := range candidateValues {
			if _, shared := anchorValues[key]; !shared {
				continue
			}
			item := ranked[key]
			if item == nil {
				item = &rankedValue{value: value}
				ranked[key] = item
			}
			item.strength += weight.Positive
			item.sourceIDs = appendUniqueUUID(item.sourceIDs, anchor.Signal.GameID)
		}
	}
	items := make([]rankedValue, 0, len(ranked))
	for _, item := range ranked {
		items = append(items, *item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].strength != items[j].strength {
			return items[i].strength > items[j].strength
		}
		return normalizedLess(items[i].value, items[j].value)
	})
	values := make([]string, 0, min(3, len(items)))
	sourceIDs := make([]uuid.UUID, 0)
	for i := 0; i < len(items) && i < 3; i++ {
		values = append(values, items[i].value)
		for _, id := range items[i].sourceIDs {
			sourceIDs = appendUniqueUUID(sourceIDs, id)
		}
	}
	sortUUIDs(sourceIDs)
	return values, sourceIDs
}

type rankedAnchor struct {
	anchor EvidenceAnchor
	score  float64
}

func similarAnchors(candidateID uuid.UUID, candidateVector []float32, anchors []EvidenceAnchor, weights map[uuid.UUID]profile.SignalWeights) ([]string, []uuid.UUID) {
	ranked := make([]rankedAnchor, 0, len(anchors))
	for _, anchor := range anchors {
		if candidateID != uuid.Nil && anchor.Signal.GameID == candidateID {
			continue
		}
		weight, ok := positiveAnchorWeight(anchor, weights)
		name, safe := safeEvidenceValue(anchor.Name)
		if !ok || !safe {
			continue
		}
		anchor.Name = name
		similarity := profile.Cosine(candidateVector, anchor.Signal.Vector)
		score := similarity * weight.Positive
		if similarity <= 0 || math.IsNaN(score) || math.IsInf(score, 0) {
			continue
		}
		ranked = append(ranked, rankedAnchor{anchor: anchor, score: score})
	}
	sortRankedAnchors(ranked)
	names := make([]string, 0, 2)
	ids := make([]uuid.UUID, 0, 2)
	seenNames := map[string]bool{}
	for _, item := range ranked {
		key := strings.ToLower(item.anchor.Name)
		if seenNames[key] {
			continue
		}
		seenNames[key] = true
		names = append(names, item.anchor.Name)
		ids = append(ids, item.anchor.Signal.GameID)
		if len(names) == 2 {
			break
		}
	}
	return names, ids
}

func bestComponentAnchor(candidateID uuid.UUID, candidate FeatureValues, anchors []EvidenceAnchor, weights map[uuid.UUID]profile.SignalWeights, component func(profile.SignalWeights) float64) (EvidenceAnchor, bool) {
	candidateValues := normalizedFeatures(candidate)
	ranked := make([]rankedAnchor, 0, len(anchors))
	for _, anchor := range anchors {
		if candidateID != uuid.Nil && anchor.Signal.GameID == candidateID {
			continue
		}
		weight, ok := positiveAnchorWeight(anchor, weights)
		name, safe := safeEvidenceValue(anchor.Name)
		if !ok || !safe {
			continue
		}
		overlap := featureOverlapCount(candidateValues, normalizedFeatures(anchor.Features))
		score := float64(overlap) * component(weight)
		if score <= 0 || math.IsNaN(score) || math.IsInf(score, 0) {
			continue
		}
		anchor.Name = name
		ranked = append(ranked, rankedAnchor{anchor: anchor, score: score})
	}
	sortRankedAnchors(ranked)
	if len(ranked) == 0 {
		return EvidenceAnchor{}, false
	}
	return ranked[0].anchor, true
}

func bestCompletionAnchor(candidate CandidateEvidence, anchors []EvidenceAnchor, weights map[uuid.UUID]profile.SignalWeights) (EvidenceAnchor, bool) {
	candidateValues := normalizedFeatures(candidate.Features)
	ranked := make([]rankedAnchor, 0, len(anchors))
	for _, anchor := range anchors {
		if candidate.GameID != uuid.Nil && anchor.Signal.GameID == candidate.GameID {
			continue
		}
		weight, ok := weights[anchor.Signal.GameID]
		name, safe := safeEvidenceValue(anchor.Name)
		if !ok || weight.Completion <= 0 || !safe {
			continue
		}
		match := float64(featureOverlapCount(candidateValues, normalizedFeatures(anchor.Features)))
		if similarity := profile.Cosine(candidate.Vector, anchor.Signal.Vector); similarity > match {
			match = similarity
		}
		score := match * weight.Completion
		if score <= 0 || math.IsNaN(score) || math.IsInf(score, 0) {
			continue
		}
		anchor.Name = name
		ranked = append(ranked, rankedAnchor{anchor: anchor, score: score})
	}
	sortRankedAnchors(ranked)
	if len(ranked) == 0 {
		return EvidenceAnchor{}, false
	}
	return ranked[0].anchor, true
}

func positiveFeedbackSources(candidateID uuid.UUID, anchors []EvidenceAnchor, weights map[uuid.UUID]profile.SignalWeights) []uuid.UUID {
	result := make([]uuid.UUID, 0, 1)
	for _, anchor := range anchors {
		weight, ok := positiveAnchorWeight(anchor, weights)
		if ok && anchor.Signal.GameID == candidateID && weight.Feedback > 0 {
			result = appendUniqueUUID(result, anchor.Signal.GameID)
		}
	}
	sortUUIDs(result)
	return result
}

func positiveAnchorWeight(anchor EvidenceAnchor, weights map[uuid.UUID]profile.SignalWeights) (profile.SignalWeights, bool) {
	weight, ok := weights[anchor.Signal.GameID]
	return weight, ok && weight.Positive > 0 && weight.Negative == 0 && anchor.Signal.Dislikes == 0
}

func normalizedFeatures(features FeatureValues) map[string]string {
	result := map[string]string{}
	for _, values := range [][]string{features.Themes, features.Mechanics, features.Modes, features.Tone} {
		for _, raw := range values {
			value, ok := safeEvidenceValue(raw)
			if !ok {
				continue
			}
			key := strings.ToLower(value)
			if current, exists := result[key]; !exists || value < current {
				result[key] = value
			}
		}
	}
	return result
}

func safeEvidenceValue(value string) (string, bool) {
	if containsUnsafeEvidenceSyntax(value) {
		return "", false
	}
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxEvidenceValueRunes {
		return "", false
	}
	if containsTechnicalEvidenceToken(value) {
		return "", false
	}
	return value, true
}

func validGroundingEvidence(item GroundingEvidence) bool {
	if item.Kind == EvidenceNegativeModePreference {
		return validModeEvidence(item.ID, item.PublicText) && item.Strength > 0 && !math.IsNaN(item.Strength) && !math.IsInf(item.Strength, 0)
	}
	if !validEvidenceID(item.ID) || !substantiveKind(item.Kind) && item.Kind != EvidenceWishlistContext || math.IsNaN(item.Strength) || math.IsInf(item.Strength, 0) || item.Strength < 0 {
		return false
	}
	text := strings.TrimSpace(item.PublicText)
	if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxPublicTextRunes {
		return false
	}
	return !containsUnsafeEvidenceSyntax(text) && !containsTechnicalEvidenceToken(text)
}

func containsUnsafeEvidenceSyntax(value string) bool {
	if !utf8.ValidString(value) || strings.ContainsAny(value, "{}[]") || strings.Contains(value, "\"") && strings.Contains(value, ":") {
		return true
	}
	for _, pattern := range privateEvidencePatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	for _, r := range value {
		if r < ' ' && r != '\t' && r != '\n' && r != '\r' {
			return true
		}
	}
	return false
}

func containsTechnicalEvidenceToken(value string) bool {
	var normalized strings.Builder
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			normalized.WriteRune(r)
		}
	}
	compact := normalized.String()
	for _, token := range []string{
		"positivesimilarity", "negativesimilarity", "wishlistcontext", "tagaffinity",
		"playtimeaffinity", "achievementaffinity", "recentactivityaffinity",
		"completionaffinity", "completionaffinityweight",
		"positivefeedback", "negativefeedback", "dateuncertainty",
		"negativemodepreference", "negativemodepreferenceweight",
		"semanticsimilarity", "negativesimilaritymultiplier",
		"steamid", "gameid",
		"rankingfactor", "sourcegameid", "sourcegameids", "publictext", "evidencekey",
		"algorithmversion", "profileversion", "rankingversion", "explanationsource",
	} {
		if strings.Contains(compact, token) {
			return true
		}
	}
	for _, token := range []string{"wishlist", "novelty", "confidence", "score", "strength", "vector", "factor", "weight", "uuid"} {
		if compact == token {
			return true
		}
	}
	return false
}

func validEvidenceID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if r < 'a' || r > 'z' {
			if r < '0' || r > '9' {
				if r != '-' {
					return false
				}
			}
		}
	}
	return true
}

func substantiveKind(kind EvidenceKind) bool {
	_, ok := evidenceKindPriority[kind]
	return ok
}

func evidenceLess(left, right GroundingEvidence) bool {
	if left.Strength != right.Strength {
		return left.Strength > right.Strength
	}
	if evidenceKindPriority[left.Kind] != evidenceKindPriority[right.Kind] {
		return evidenceKindPriority[left.Kind] < evidenceKindPriority[right.Kind]
	}
	if left.PublicText != right.PublicText {
		return left.PublicText < right.PublicText
	}
	return left.ID < right.ID
}

func factorContribution(factor, weight float64) float64 {
	value := clamp(factor) * weight
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}

func semanticContribution(r Recommendation) float64 {
	affinity := clamp(r.Factors.PositiveSimilarity) - r.Weights.NegativeSimilarityMultiplier*clamp(r.Factors.NegativeSimilarity)
	if affinity < 0 {
		affinity = 0
	}
	return factorContribution(affinity, r.Weights.SemanticSimilarity)
}

func featureOverlapCount(left, right map[string]string) int {
	count := 0
	for key := range left {
		if _, ok := right[key]; ok {
			count++
		}
	}
	return count
}

func sortRankedAnchors(items []rankedAnchor) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		left, right := items[i].anchor, items[j].anchor
		if strings.ToLower(left.Name) != strings.ToLower(right.Name) {
			return strings.ToLower(left.Name) < strings.ToLower(right.Name)
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Signal.GameID.String() < right.Signal.GameID.String()
	})
}

func normalizedLess(left, right string) bool {
	if strings.ToLower(left) != strings.ToLower(right) {
		return strings.ToLower(left) < strings.ToLower(right)
	}
	return left < right
}

func appendUniqueUUID(values []uuid.UUID, value uuid.UUID) []uuid.UUID {
	if value == uuid.Nil {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func sortUUIDs(values []uuid.UUID) {
	sort.Slice(values, func(i, j int) bool { return values[i].String() < values[j].String() })
}

func containsEvidenceID(values []GroundingEvidence, id string) bool {
	for _, item := range values {
		if item.ID == id {
			return true
		}
	}
	return false
}
