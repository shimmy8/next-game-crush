package recommendations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
)

const (
	JobKind           = "recommendations.rebuild"
	JobPayloadVersion = 1
)

type ExplanationClient interface {
	ExplainRecommendation(context.Context, ai.RecommendationEvidence) (ai.Explanation, error)
}
type ExplanationCache interface {
	GetExplanationByInput(context.Context, uuid.UUID, uuid.UUID, string, string) (ai.Explanation, bool, error)
	PutExplanationByInput(context.Context, uuid.UUID, uuid.UUID, string, string, ai.Explanation) error
}
type DeckRefreshPlanner interface {
	EnqueueDeckRefresh(context.Context, catalog.DeckRefreshTarget) error
}
type CatalogRebuildObserver interface {
	CatalogRebuildFinished(context.Context, uuid.UUID, string, bool) error
}
type rankingSourceVersionReader interface {
	RankingSourceVersions(context.Context, uuid.UUID, int64) (int64, int64, error)
}
type Repository interface {
	Candidates(context.Context, uuid.UUID, profile.Profile, int) ([]Candidate, error)
	RankingInputs(context.Context, uuid.UUID, profile.Profile, int) (RankingInputs, error)
	ResolveRankingInputs(context.Context, RankingRequest, int) (RankingInputs, error)
	SaveRanking(context.Context, uuid.UUID, RankingSource, []Recommendation, string) ([]Recommendation, int64, bool, error)
	SaveExplanation(context.Context, uuid.UUID, int64, string, uuid.UUID, string, ExplanationRecord) error
	PublishRanking(context.Context, uuid.UUID, int64, string) error
	PublishedDeckRefreshTargets(context.Context, uuid.UUID, int64) ([]catalog.DeckRefreshTarget, error)
	Latest(context.Context, uuid.UUID) ([]Recommendation, error)
	Get(context.Context, uuid.UUID, uuid.UUID) (Recommendation, error)
}
type preferenceOverlayEnsurer interface {
	EnsureCurrent(context.Context, uuid.UUID) (preferences.Overlay, error)
}
type preferenceRepositoryEnsurer struct{ repository *preferences.SQLRepository }

func (e preferenceRepositoryEnsurer) EnsureCurrent(ctx context.Context, userID uuid.UUID) (preferences.Overlay, error) {
	return e.repository.RebuildOverlay(ctx, userID, "")
}

type Service struct {
	repository         Repository
	profiles           *profile.Service
	overlays           preferenceOverlayEnsurer
	explainer          ExplanationClient
	limit              int
	queue              *jobs.Queue
	wishlistSource     WishlistStateReader
	discoveryStatus    catalog.DiscoveryStatusReader
	explanationCache   ExplanationCache
	explanationModel   string
	deckRefreshPlanner DeckRefreshPlanner
	catalogRebuilds    CatalogRebuildObserver
	eligibility        catalog.CurrentEligibilityReader
	clock              func() time.Time
	logger             *observability.Logger
}

func (s *Service) SetQueue(queue *jobs.Queue)                       { s.queue = queue }
func (s *Service) SetDeckRefreshPlanner(planner DeckRefreshPlanner) { s.deckRefreshPlanner = planner }
func (s *Service) SetCatalogRebuildObserver(observer CatalogRebuildObserver) {
	s.catalogRebuilds = observer
}

func (s *Service) enqueuePublishedDeckRefreshes(ctx context.Context, userID uuid.UUID, rankingVersion int64) error {
	if s.deckRefreshPlanner == nil {
		return nil
	}
	targets, err := s.repository.PublishedDeckRefreshTargets(ctx, userID, rankingVersion)
	if err != nil {
		return fmt.Errorf("load published Deck refresh targets: %w", err)
	}
	for _, target := range targets {
		if err := s.deckRefreshPlanner.EnqueueDeckRefresh(ctx, target); err != nil {
			return fmt.Errorf("enqueue published Deck refresh: %w", err)
		}
	}
	return nil
}
func (s *Service) SetExplanationCache(cache ExplanationCache, model string) {
	s.explanationCache = cache
	s.explanationModel = strings.TrimSpace(model)
}

func NewService(repository Repository, profiles *profile.Service, explainer ExplanationClient, eligibility catalog.CurrentEligibilityReader, clock func() time.Time, overlayServices ...preferenceOverlayEnsurer) (*Service, error) {
	if repository == nil || profiles == nil || eligibility == nil || clock == nil {
		return nil, errors.New("recommendation repository, profile service, eligibility reader, and clock are required")
	}
	var overlays preferenceOverlayEnsurer
	if len(overlayServices) > 0 {
		overlays = overlayServices[0]
	} else if provider, ok := repository.(preferenceOverlayEnsurer); ok {
		overlays = provider
	} else if sqlRepository, ok := repository.(*SQLRepository); ok {
		preferenceRepository, err := preferences.NewSQLRepository(sqlRepository.store)
		if err != nil {
			return nil, err
		}
		overlays = preferenceRepositoryEnsurer{repository: preferenceRepository}
	}
	return &Service{repository: repository, profiles: profiles, overlays: overlays, explainer: explainer, eligibility: eligibility, clock: clock, logger: observability.New(nil), limit: 100}, nil
}
func (s *Service) SetLogger(logger *observability.Logger) {
	if logger == nil {
		logger = observability.New(nil)
	}
	s.logger = logger
}
func (s *Service) Rebuild(ctx context.Context, userID uuid.UUID) ([]Recommendation, error) {
	items, _, _, err := s.rebuild(ctx, userID, uuid.NewString())
	return items, err
}
func (s *Service) rebuild(ctx context.Context, userID uuid.UUID, requestKey string) ([]Recommendation, int64, ExplanationStats, error) {
	return s.rebuildForProfile(ctx, userID, "", requestKey)
}

func (s *Service) rebuildForProfile(ctx context.Context, userID uuid.UUID, profileBuildKey, requestKey string) ([]Recommendation, int64, ExplanationStats, error) {
	emptyStats := ExplanationStats{FailureReasons: map[ExplanationFailureReason]int{}}
	if s.overlays == nil {
		return nil, 0, emptyStats, errors.New("preference overlay service is required")
	}
	input, err := s.resolveCoherentInputsForProfile(ctx, userID, profileBuildKey, requestKey)
	if err != nil {
		return nil, 0, emptyStats, fmt.Errorf("retrieve candidates: %w", err)
	}
	p := input.Source.Activity
	allowed, err := s.eligibility.FilterEligibleGameIDs(ctx, candidateIDs(input.Candidates), s.clock().UTC())
	if err != nil {
		return nil, 0, emptyStats, fmt.Errorf("filter current catalog eligibility: %w", err)
	}
	candidates := make([]Candidate, 0, len(input.Candidates))
	for _, candidate := range input.Candidates {
		if !allowed[candidate.GameID] {
			continue
		}
		candidates = append(candidates, candidate)
	}
	weights := DefaultWeights()
	output := make([]Recommendation, 0, len(candidates))
	for _, candidate := range candidates {
		if !Eligible(candidate) {
			continue
		}
		result := Score(candidate.Factors, weights)
		item := Recommendation{GameID: candidate.GameID, Name: candidate.Name, ProfileVersion: p.Version, AlgorithmVersion: AlgorithmVersion, Score: result.Score, Confidence: clamp(candidate.Factors.Confidence * p.Confidence), Factors: candidate.Factors, Weights: weights, Wishlisted: clamp(candidate.Factors.Wishlist) > 0, Release: candidate.Release}
		if candidate.ModePreferenceEvidence != nil {
			item.Evidence = []Evidence{*candidate.ModePreferenceEvidence}
		}
		output = append(output, item)
	}
	output = Sort(output)
	// Rank the complete deduplicated union first. The presentation cap applies
	// only afterwards so a strong item from a later retrieval leg cannot be
	// discarded before scoring; wishlist entries are mandatory and uncapped.
	output = capRankedRecommendations(output, s.limit)
	runBuildKey := RankingBuildKey(requestKey, AlgorithmVersion, input.Source)
	saved, rankingVersion, alreadyPublished, err := s.repository.SaveRanking(ctx, userID, input.Source, output, runBuildKey)
	if err != nil {
		return nil, 0, emptyStats, fmt.Errorf("persist ranking: %w", err)
	}
	if alreadyPublished {
		return nil, rankingVersion, emptyStats, s.enqueuePublishedDeckRefreshes(ctx, userID, rankingVersion)
	}
	if len(saved) == 0 {
		if err := s.repository.PublishRanking(ctx, userID, rankingVersion, runBuildKey); err != nil {
			return nil, 0, emptyStats, err
		}
		if err := s.enqueuePublishedDeckRefreshes(ctx, userID, rankingVersion); err != nil {
			return nil, rankingVersion, emptyStats, err
		}
		return saved, rankingVersion, emptyStats, nil
	}
	corpus := input.Corpus
	stats := emptyStats
	for i := range saved {
		candidate := corpus.Candidates[saved[i].GameID]
		if candidate.GameID == uuid.Nil {
			candidate.GameID = saved[i].GameID
		}
		selected := SelectGroundingEvidence(BuildGroundingEvidence(saved[i], candidate, corpus.Anchors, p.CreatedAt))
		explanation, err := s.explainCached(ctx, userID, saved[i].GameID, saved[i].Name, selected)
		if err != nil {
			return nil, 0, stats, fmt.Errorf("reuse explanation input: %w", err)
		}
		record := ExplanationRecord{Text: explanation.Text, Source: explanation.Source, FailureReason: explanation.FailureReason, Supplied: selected}
		if explanation.Source == ExplanationSourceAI {
			record.CitedIDs = append([]string(nil), explanation.EvidenceKeys...)
		}
		if err := s.repository.SaveExplanation(ctx, userID, rankingVersion, runBuildKey, saved[i].ID, saved[i].Name, record); err != nil {
			return nil, 0, stats, fmt.Errorf("persist explanation: %w", err)
		}
		saved[i].Explanation = explanation.Text
		saved[i].ExplanationSource = explanation.Source
		saved[i].GeneratedExplanation = explanation.Source == ExplanationSourceAI
		switch explanation.Source {
		case ExplanationSourceAI:
			saved[i].Evidence = PublicEvidence(citedGroundingEvidence(selected, explanation.EvidenceKeys))
			stats.AIGenerated++
		case ExplanationSourceDeterministic:
			saved[i].Evidence = PublicEvidence(selected)
			stats.Deterministic++
		default:
			saved[i].Evidence = []Evidence{}
			stats.None++
		}
		if explanation.FailureReason != "" {
			stats.FailureReasons[explanation.FailureReason]++
		}
	}
	if err := s.repository.PublishRanking(ctx, userID, rankingVersion, runBuildKey); err != nil {
		return nil, 0, stats, err
	}
	if err := s.enqueuePublishedDeckRefreshes(ctx, userID, rankingVersion); err != nil {
		return nil, rankingVersion, stats, err
	}
	return saved, rankingVersion, stats, nil
}

func capRankedRecommendations(ranked []Recommendation, nonWishlistLimit int) []Recommendation {
	result := make([]Recommendation, 0, len(ranked))
	nonWishlist := 0
	for _, item := range ranked {
		if !item.Wishlisted && nonWishlist >= nonWishlistLimit {
			continue
		}
		result = append(result, item)
		if !item.Wishlisted {
			nonWishlist++
		}
	}
	return result
}

// resolveCoherentInputs closes the gap between publishing an overlay and
// opening the ranking snapshot. A feedback commit in that gap makes the first
// snapshot's direct factors newer than its overlay; rebuilding once more
// detects that change and retries with the exact newly published overlay.
func (s *Service) resolveCoherentInputs(ctx context.Context, userID uuid.UUID, requestKey string) (RankingInputs, error) {
	return s.resolveCoherentInputsForProfile(ctx, userID, "", requestKey)
}

func (s *Service) resolveCoherentInputsForProfile(ctx context.Context, userID uuid.UUID, profileBuildKey, requestKey string) (RankingInputs, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := s.overlays.EnsureCurrent(ctx, userID); err != nil {
			return RankingInputs{}, fmt.Errorf("ensure current preference overlay: %w", err)
		}
		input, err := s.repository.ResolveRankingInputs(ctx, RankingRequest{UserID: userID, ProfileBuildKey: profileBuildKey, RequestKey: requestKey}, 1000000)
		if err != nil {
			return RankingInputs{}, err
		}
		current, err := s.overlays.EnsureCurrent(ctx, userID)
		if err != nil {
			return RankingInputs{}, fmt.Errorf("verify current preference overlay: %w", err)
		}
		if current.ID == input.Source.Overlay.ID && current.InputContentHash == input.Source.Overlay.InputContentHash {
			return input, nil
		}
	}
	return RankingInputs{}, errors.New("preference overlay changed during ranking snapshot")
}

func (s *Service) explainCached(ctx context.Context, userID, gameID uuid.UUID, name string, selected []GroundingEvidence) (ExplanationResult, error) {
	if s.explanationCache == nil || s.explanationModel == "" {
		return Explain(ctx, s.explainer, name, selected), nil
	}
	public := PublicEvidence(selected)
	input, err := json.Marshal(struct {
		Schema, Prompt, Model, Game string
		Evidence                    []Evidence
	}{ai.SchemaVersion, ai.ExplanationPromptVersion, s.explanationModel, name, public})
	if err != nil {
		return ExplanationResult{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(input))
	if cached, ok, err := s.explanationCache.GetExplanationByInput(ctx, userID, gameID, s.explanationModel, hash); err != nil {
		return ExplanationResult{}, err
	} else if ok {
		if result, valid := validatedCachedExplanation(cached, selected); valid {
			return result, nil
		}
	}
	result := Explain(ctx, s.explainer, name, selected)
	if result.Source == ExplanationSourceAI {
		if err := s.explanationCache.PutExplanationByInput(ctx, userID, gameID, s.explanationModel, hash, ai.Explanation{Text: result.Text, EvidenceKeys: append([]string(nil), result.EvidenceKeys...), Generated: true}); err != nil {
			return ExplanationResult{}, err
		}
	}
	return result, nil
}

func validatedCachedExplanation(cached ai.Explanation, selected []GroundingEvidence) (ExplanationResult, bool) {
	allowed := map[string]struct{}{}
	for _, item := range selected {
		if substantiveKind(item.Kind) {
			allowed[item.ID] = struct{}{}
		}
	}
	if !safeRussianExplanation(cached.Text) || !validExplanationCitations(cached.EvidenceKeys, allowed) {
		return ExplanationResult{}, false
	}
	return ExplanationResult{Text: strings.TrimSpace(cached.Text), Source: ExplanationSourceAI, EvidenceKeys: append([]string(nil), cached.EvidenceKeys...)}, true
}
func citedEvidence(evidence []Evidence, ids []string) []Evidence {
	allowed := map[string]Evidence{}
	for _, item := range evidence {
		allowed[item.ID] = item
	}
	result := make([]Evidence, 0, len(ids))
	for _, id := range ids {
		if item, ok := allowed[id]; ok {
			result = append(result, item)
		}
	}
	return result
}
func (s *Service) Latest(ctx context.Context, userID uuid.UUID) ([]Recommendation, error) {
	items, err := s.repository.Latest(ctx, userID)
	if err != nil {
		return nil, err
	}
	allowed, err := s.eligibility.FilterEligibleGameIDs(ctx, recommendationGameIDs(items), s.clock().UTC())
	if err != nil {
		return nil, err
	}
	filtered := make([]Recommendation, 0, len(items))
	for i := range items {
		if allowed[items[i].GameID] {
			filtered = append(filtered, safePresentation(items[i]))
		}
	}
	return filtered, nil
}
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Recommendation, error) {
	item, err := s.repository.Get(ctx, userID, id)
	if err != nil {
		return Recommendation{}, err
	}
	eligible, err := s.eligibility.Eligible(ctx, item.GameID, s.clock().UTC())
	if err != nil {
		return Recommendation{}, err
	}
	if !eligible {
		return Recommendation{}, pgx.ErrNoRows
	}
	return safePresentation(item), nil
}

func candidateIDs(items []Candidate) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.GameID)
	}
	return ids
}

func recommendationGameIDs(items []Recommendation) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.GameID)
	}
	return ids
}
func Explain(ctx context.Context, client ExplanationClient, name string, selected []GroundingEvidence) ExplanationResult {
	selected = SelectGroundingEvidence(selected)
	fallback := ExplanationResult{Text: DeterministicExplanation(selected), EvidenceKeys: groundingEvidenceIDs(selected)}

	substantive := make([]GroundingEvidence, 0, 2)
	for _, item := range selected {
		if substantiveKind(item.Kind) {
			substantive = append(substantive, item)
		}
	}
	if len(substantive) == 0 {
		if len(selected) == 0 {
			fallback.Source = ExplanationSourceNone
		} else {
			fallback.Source = ExplanationSourceDeterministic
		}
		return fallback
	}
	fallback.Source = ExplanationSourceDeterministic
	if client == nil {
		fallback.FailureReason = ExplanationFailureDisabled
		return fallback
	}

	items := make([]ai.EvidenceItem, 0, len(substantive))
	allowed := make(map[string]struct{}, len(substantive))
	for _, item := range substantive {
		items = append(items, ai.EvidenceItem{Key: item.ID, Value: strings.TrimSpace(item.PublicText)})
		allowed[item.ID] = struct{}{}
	}
	out, err := client.ExplainRecommendation(ctx, ai.RecommendationEvidence{EvidenceKey: "recommendation-evidence", GameName: name, Evidence: items})
	if err != nil {
		fallback.FailureReason = classifyExplanationFailure(err)
		return fallback
	}
	if !safeRussianExplanation(out.Text) || containsUnsupportedAffinityClaim(out.Text) {
		fallback.FailureReason = ExplanationFailureUnsafeText
		return fallback
	}
	if !validExplanationCitations(out.EvidenceKeys, allowed) {
		fallback.FailureReason = ExplanationFailureInvalidCitation
		return fallback
	}
	return ExplanationResult{
		Text:         strings.TrimSpace(out.Text),
		Source:       ExplanationSourceAI,
		EvidenceKeys: append([]string(nil), out.EvidenceKeys...),
	}
}

func classifyExplanationFailure(err error) ExplanationFailureReason {
	if errors.Is(err, ai.ErrCapabilityDisabled) {
		return ExplanationFailureDisabled
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ExplanationFailureTimeout
	}
	var requestErr *ai.RequestError
	if errors.As(err, &requestErr) {
		switch {
		case requestErr.StatusCode == http.StatusTooManyRequests:
			return ExplanationFailureRateLimited
		case requestErr.StatusCode == http.StatusRequestTimeout:
			return ExplanationFailureTimeout
		case requestErr.StatusCode >= 500:
			return ExplanationFailureProviderUnavailable
		}
	}
	switch {
	case errors.Is(err, ai.ErrRefusal):
		return ExplanationFailureRefusal
	case errors.Is(err, ai.ErrInvalidEvidence):
		return ExplanationFailureInvalidCitation
	case errors.Is(err, ai.ErrInvalidOutput), errors.Is(err, ai.ErrInvalidInput), errors.Is(err, ai.ErrIncomplete):
		return ExplanationFailureInvalidOutput
	case errors.Is(err, ai.ErrHTTP):
		return ExplanationFailureProviderUnavailable
	default:
		return ExplanationFailureUnknown
	}
}

func validExplanationCitations(keys []string, allowed map[string]struct{}) bool {
	if len(keys) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, ok := allowed[key]; !ok {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func containsUnsupportedAffinityClaim(text string) bool {
	lower := strings.ToLower(text)
	for _, token := range []string{"добавляет новое", "добавит новое", "новизн", "новый опыт", "расшир", "желаем", "список желан"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func groundingEvidenceIDs(items []GroundingEvidence) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.ID)
	}
	return keys
}

// legacyGroundingEvidence keeps the pre-v2 rebuild flow safe until v2 corpus
// persistence is wired by the next phase. It drops bookkeeping-only inputs
// and assigns fresh opaque keys rather than exposing factor names to OpenAI.
func legacyGroundingEvidence(items []Evidence) []GroundingEvidence {
	result := make([]GroundingEvidence, 0, len(items))
	for _, item := range items {
		if validModeEvidence(item.ID, item.Text) {
			result = append(result, GroundingEvidence{ID: item.ID, Kind: EvidenceNegativeModePreference, PublicText: item.Text, Strength: 1})
			continue
		}
		kind := EvidenceFeatureOverlap
		switch item.ID {
		case "wishlist":
			kind = EvidenceWishlistContext
		case "semantic-affinity":
			kind = EvidenceSimilarGame
		case "playtime-affinity":
			kind = EvidencePlaytimeAnchor
		case "achievement-affinity":
			kind = EvidenceAchievementAnchor
		case "recent-activity":
			kind = EvidenceRecentActivityAnchor
		case "positive-feedback":
			kind = EvidencePositiveFeedback
		case "tag-affinity":
			kind = EvidenceFeatureOverlap
		default:
			continue
		}
		result = append(result, GroundingEvidence{ID: fmt.Sprintf("evidence-%d", len(result)+1), Kind: kind, PublicText: item.Text, Strength: 1})
	}
	return result
}

func hasEvidenceID(evidence []Evidence, id string) bool {
	for _, item := range evidence {
		if item.ID == id {
			return true
		}
	}
	return false
}

func hasSubstantiveEvidence(evidence []Evidence) bool {
	for _, item := range evidence {
		switch item.ID {
		case "semantic-affinity", "tag-affinity", "playtime-affinity", "achievement-affinity", "recent-activity", "positive-feedback":
			return true
		}
	}
	return false
}

func fallbackExplanation(evidence []Evidence) ai.Explanation {
	facts := make([]string, 0, len(evidence))
	keys := make([]string, 0, len(evidence))
	for _, item := range evidence {
		facts = append(facts, item.Text)
		keys = append(keys, item.ID)
	}
	out := ai.DeterministicExplanation(facts)
	out.EvidenceKeys = keys
	if hasEvidenceID(evidence, "wishlist") && !hasSubstantiveEvidence(evidence) {
		out.Text = "Игра уже в вашем списке желаемого — следим за её релизом."
	}
	return out
}

func safeRussianExplanation(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 1000 || containsUnsafeEvidenceSyntax(text) || containsTechnicalEvidenceToken(text) {
		return false
	}
	hasRussianLetter := false
	for _, r := range text {
		if r == 'Ё' || r == 'ё' || r >= 'А' && r <= 'я' {
			hasRussianLetter = true
			break
		}
	}
	if !hasRussianLetter {
		return false
	}
	lower := strings.ToLower(text)
	for _, token := range []string{
		"{", "}", "[", "]",
		"deterministic", "score",
		"positivesimilarity", "positive_similarity",
		"negativesimilarity", "negative_similarity",
		"wishlist", "tagaffinity", "tag_affinity",
		"playtimeaffinity", "playtime_affinity",
		"achievementaffinity", "achievement_affinity",
		"recentactivityaffinity", "recent_activity_affinity",
		"positivefeedback", "positive_feedback",
		"novelty",
		"negativefeedback", "negative_feedback",
		"negativemodepreference", "negative_mode_preference", "negativemodepreferenceweight", "negative_mode_preference_weight",
		"dateuncertainty", "date_uncertainty",
		"semanticsimilarity", "semantic_similarity",
		"negativesimilaritymultiplier", "negative_similarity_multiplier",
		"confidence", "release-window", "release_precision",
		"day:", "month:", "quarter:", "year:", "coming_soon:", "coming soon:", "unknown:",
	} {
		if strings.Contains(lower, token) {
			return false
		}
	}
	return true
}

func publicEvidence(r Recommendation) []Evidence {
	type reason struct {
		Evidence
		contribution float64
		order        int
	}

	affinity := r.Factors.ActivitySimilarity
	reasons := []reason{
		{Evidence: Evidence{ID: "semantic-affinity", Text: "Похожа на игры, в которые вы играли"}, contribution: clamp(affinity) * r.Weights.ActivitySimilarity, order: 0},
		{Evidence: Evidence{ID: "preference-affinity", Text: "Похожа на игры, которые вы отмечали как понравившиеся"}, contribution: clamp(r.Factors.PositivePreferenceSimilarity) * r.Weights.PositivePreferenceSimilarity, order: 1},
		{Evidence: Evidence{ID: "tag-affinity", Text: "Совпадает с вашими любимыми жанрами и метками"}, contribution: clamp(r.Factors.TagAffinity) * r.Weights.TagAffinity, order: 2},
		{Evidence: Evidence{ID: "playtime-affinity", Text: "Похожа на игры, которым вы уделяли много времени"}, contribution: clamp(r.Factors.PlaytimeAffinity) * r.Weights.PlaytimeAffinity, order: 3},
		{Evidence: Evidence{ID: "achievement-affinity", Text: "Похожа на игры, в которых вы часто получали достижения"}, contribution: clamp(r.Factors.AchievementAffinity) * r.Weights.AchievementAffinity, order: 4},
		{Evidence: Evidence{ID: "recent-activity", Text: "Соответствует вашим недавним игровым интересам"}, contribution: clamp(r.Factors.RecentActivityAffinity) * r.Weights.RecentActivityAffinity, order: 5},
		{Evidence: Evidence{ID: "positive-feedback", Text: "Учитывает вашу положительную оценку этой игры"}, contribution: clamp(r.Factors.PositiveDirectFeedback) * r.Weights.PositiveDirectFeedback, order: 6},
		{Evidence: Evidence{ID: "novelty", Text: "Добавляет новое к вашим привычным игровым интересам"}, contribution: clamp(r.Factors.Novelty) * r.Weights.Novelty, order: 7},
	}
	positive := reasons[:0]
	for _, item := range reasons {
		if item.contribution > 0 {
			positive = append(positive, item)
		}
	}
	sort.SliceStable(positive, func(i, j int) bool {
		if positive[i].contribution != positive[j].contribution {
			return positive[i].contribution > positive[j].contribution
		}
		return positive[i].order < positive[j].order
	})
	result := make([]Evidence, 0, 3)
	correction := modeEvidence(r)
	// Reserve one persisted human evidence slot even though the factor reduces score.
	if correction != nil {
		result = append(result, *correction)
	}
	if r.Factors.Wishlist > 0 {
		result = append(result, Evidence{ID: "wishlist", Text: "Игра есть в вашем списке желаемого"})
		// Keep a real affinity signal even when novelty contributes more points.
		for _, item := range positive {
			if hasSubstantiveEvidence([]Evidence{item.Evidence}) {
				result = append(result, item.Evidence)
				break
			}
		}
	}
	for _, item := range positive {
		if len(result) == 3 {
			break
		}
		if !hasEvidenceID(result, item.ID) {
			result = append(result, item.Evidence)
		}
	}
	return result
}

func safePresentation(r Recommendation) Recommendation {
	if r.ExplanationSource != "" {
		if r.ExplanationSource == ExplanationSourceNone {
			r.Explanation = neutralExplanation
			r.GeneratedExplanation = false
			r.Evidence = []Evidence{}
			// Mode-preference correction is persisted ranking evidence from the
			// same snapshot, not regenerated affinity evidence. Preserve this
			// independent feedback presentation when legacy explanation data is
			// discarded.
			if !r.explanationLoaded {
				if correction := modeEvidence(r); correction != nil {
					r.Evidence = []Evidence{*correction}
					r.Explanation = correction.Text
					r.ExplanationSource = ExplanationSourceDeterministic
				}
			}
			return r
		}
		valid := (r.ExplanationSource == ExplanationSourceAI || r.ExplanationSource == ExplanationSourceDeterministic) && safeRussianExplanation(r.Explanation)
		for _, item := range r.Evidence {
			grounding := GroundingEvidence{ID: item.ID, Kind: item.Kind, PublicText: item.Text, Strength: 1}
			if !validGroundingEvidence(grounding) || item.Kind == EvidenceNegativeModePreference && clamp(r.Factors.NegativeModePreference) == 0 {
				valid = false
				break
			}
		}
		if valid && (r.ExplanationSource != ExplanationSourceAI || len(r.Evidence) > 0) {
			r.GeneratedExplanation = r.ExplanationSource == ExplanationSourceAI
			return r
		}
		r.Explanation = neutralExplanation
		r.ExplanationSource = ExplanationSourceNone
		r.GeneratedExplanation = false
		r.Evidence = []Evidence{}
		return r
	}

	// Unversioned in-memory values are kept only for old callers/tests. SQL
	// reads are mapped strictly in mapStoredExplanation above.
	evidence := publicEvidence(r)
	allowed := make(map[string]string, len(evidence))
	for _, item := range evidence {
		allowed[item.ID] = item.Text
	}
	groundedGenerated := r.GeneratedExplanation && safeRussianExplanation(r.Explanation) && len(r.Evidence) > 0
	if hasEvidenceID(evidence, "wishlist") && !hasSubstantiveEvidence(r.Evidence) {
		groundedGenerated = false
	}
	for _, item := range r.Evidence {
		if allowed[item.ID] != item.Text {
			groundedGenerated = false
			break
		}
	}
	r.Evidence = evidence
	if groundedGenerated {
		return r
	}
	r.Explanation = fallbackExplanation(evidence).Text
	r.GeneratedExplanation = false
	return r
}

type JobPayload struct {
	Version         int       `json:"version"`
	UserID          uuid.UUID `json:"user_id"`
	ProfileBuildKey string    `json:"profile_build_key,omitempty"`
	RequestKey      string    `json:"request_key,omitempty"`
}

func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	return worker.Register(JobKind, s.handleJob)
}

func (s *Service) handleJob(ctx context.Context, lease jobs.Lease) error {
	started := s.clock()
	var p JobPayload
	if json.Unmarshal(lease.Payload, &p) != nil || p.Version != JobPayloadVersion || p.UserID == uuid.Nil || lease.UserID != p.UserID {
		return errors.New("unsupported recommendations job payload schema")
	}
	requestKey := strings.TrimSpace(p.RequestKey)
	if requestKey == "" {
		requestKey = stableRebuildRequestKey(lease.IdempotencyKey)
	}
	result, rankingVersion, explanationStats, err := s.rebuildForProfile(ctx, p.UserID, strings.TrimSpace(p.ProfileBuildKey), requestKey)
	if err != nil {
		s.logger.RankingPublication(ctx, observability.RankingPublicationEvent{Algorithm: AlgorithmVersion, Duration: s.clock().Sub(started), Reason: observability.ReasonPersistenceFailed})
		if s.catalogRebuilds != nil {
			if observerErr := s.catalogRebuilds.CatalogRebuildFinished(ctx, p.UserID, lease.IdempotencyKey, false); observerErr != nil {
				return errors.Join(err, observerErr)
			}
		}
		return err
	}
	if s.catalogRebuilds != nil {
		if err := s.catalogRebuilds.CatalogRebuildFinished(ctx, p.UserID, lease.IdempotencyKey, true); err != nil {
			return err
		}
	}
	if s.queue != nil {
		var profileVersion int64
		var overlayVersion int64
		if reader, ok := s.repository.(rankingSourceVersionReader); ok {
			profileVersion, overlayVersion, err = reader.RankingSourceVersions(ctx, p.UserID, rankingVersion)
		} else if len(result) > 0 {
			profileVersion = result[0].ProfileVersion
		} else {
			profileValue, latestErr := s.profiles.Latest(ctx, p.UserID)
			err, profileVersion = latestErr, profileValue.Version
		}
		if err != nil {
			return fmt.Errorf("resolve ranking activity profile version: %w", err)
		}
		if profileVersion < 1 {
			return errors.New("resolve ranking activity profile version: invalid version")
		}
		if err := s.queue.RecordResult(ctx, lease, map[string]any{"profileVersion": profileVersion, "preferenceOverlayVersion": overlayVersion, "rankingVersion": rankingVersion, "algorithmVersion": AlgorithmVersion, "recommendationCount": len(result), "explanations": explanationStats}); err != nil {
			return err
		}
		s.logger.RankingPublication(ctx, observability.RankingPublicationEvent{Algorithm: AlgorithmVersion, ProfileVersion: profileVersion, OverlayVersion: overlayVersion, Duration: s.clock().Sub(started)})
	}
	return nil
}

func stableRebuildRequestKey(key string) string {
	const prefix = "catalog:recommendations.rebuild:v2:"
	if !strings.HasPrefix(key, prefix) {
		return key
	}
	retry := strings.LastIndex(key, ":retry:")
	if retry <= len(prefix) || retry+len(":retry:") >= len(key) {
		return key
	}
	if _, err := strconv.Atoi(key[retry+len(":retry:"):]); err != nil {
		return key
	}
	identity := strings.Split(key[len(prefix):retry], ":")
	if len(identity) != 2 {
		return key
	}
	if _, err := uuid.Parse(identity[0]); err != nil {
		return key
	}
	if revision, err := strconv.ParseInt(identity[1], 10, 64); err != nil || revision < 1 {
		return key
	}
	return key[:retry]
}
