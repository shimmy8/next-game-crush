package recommendations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
)

type SQLRepository struct {
	store                     *store.Store
	embeddingModel, textModel string
	now                       func() time.Time
	lookbackDays              int
	horizonDays               int
}

type rowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func NewSQLRepository(database *store.Store, embeddingModel, textModel string) (*SQLRepository, error) {
	if database == nil || strings.TrimSpace(embeddingModel) == "" {
		return nil, errors.New("recommendation store and embedding model are required")
	}
	return &SQLRepository{store: database, embeddingModel: embeddingModel, textModel: textModel, now: time.Now, lookbackDays: 30, horizonDays: 90}, nil
}

func (r *SQLRepository) EnsureCurrent(ctx context.Context, userID uuid.UUID) (preferences.Overlay, error) {
	repository, err := preferences.NewSQLRepository(r.store)
	if err != nil {
		return preferences.Overlay{}, err
	}
	return repository.RebuildOverlay(ctx, userID, "")
}

// SetHorizonDays configures the future side of the shared release window.
// Wishlist candidates remain subject to that window; they only bypass the
// non-wishlist result cap after admission.
func (r *SQLRepository) SetHorizonDays(days int) error {
	if days < 1 || days > 366 {
		return errors.New("recommendation horizon must be between 1 and 366 days")
	}
	r.horizonDays = days
	return nil
}

func (r *SQLRepository) SetWindowDays(lookbackDays, horizonDays int) error {
	if lookbackDays < 1 || lookbackDays > 366 || horizonDays < 1 || horizonDays > 366 {
		return errors.New("recommendation windows must be between 1 and 366 days")
	}
	r.lookbackDays, r.horizonDays = lookbackDays, horizonDays
	return nil
}

const candidateSelect = `WITH candidate_ids AS (
	SELECT game_id FROM catalog_entries
	UNION SELECT game_id FROM wishlist_entries WHERE user_id=$1
), feedback AS (
	SELECT rec.game_id,count(*) FILTER (WHERE f.action='like')::int likes,
	count(*) FILTER (WHERE f.action IN ('dislike','exclude'))::int dislikes
	FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id WHERE f.user_id=$1 GROUP BY rec.game_id
), latest_direct AS (
	SELECT DISTINCT ON (rec.game_id) rec.game_id,f.action
	FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id
	WHERE f.user_id=$1 AND f.action IN ('like','dislike','exclude')
	ORDER BY rec.game_id,f.created_at DESC,f.id DESC
), current_release AS (
	SELECT DISTINCT ON (game_id) game_id,precision,release_date,raw_text FROM game_release_dates ORDER BY game_id,observed_at DESC,id DESC
)
SELECT g.id,g.name,e.embedding,(w.game_id IS NOT NULL),(ug.game_id IS NOT NULL),
	(COALESCE(cr.precision,'unknown')='day' AND cr.release_date<CURRENT_DATE),COALESCE(ld.action IN ('dislike','exclude'),false),COALESCE(ld.action='like',false),COALESCE(f.dislikes,0),
	COALESCE(cr.precision,'unknown'),cr.release_date,cr.raw_text
FROM candidate_ids ci JOIN games g ON g.id=ci.game_id
LEFT JOIN game_embeddings e ON e.game_id=g.id AND e.model=$2
LEFT JOIN wishlist_entries w ON w.user_id=$1 AND w.game_id=g.id
LEFT JOIN user_games ug ON ug.user_id=$1 AND ug.game_id=g.id
LEFT JOIN feedback f ON f.game_id=g.id
LEFT JOIN latest_direct ld ON ld.game_id=g.id
LEFT JOIN current_release cr ON cr.game_id=g.id
WHERE NOT COALESCE(
	jsonb_typeof(g.source_snapshot->'AppID')='number' AND g.source_snapshot->>'AppID'=g.steam_app_id::text
	AND jsonb_typeof(g.source_snapshot->'Type')='string' AND btrim(g.source_snapshot->>'Type')<>''
	AND lower(btrim(g.source_snapshot->>'Type'))<>'game',false)
	AND %s ORDER BY %s LIMIT $3`

func (r *SQLRepository) Candidates(ctx context.Context, userID uuid.UUID, p profile.Profile, limit int) ([]Candidate, error) {
	input, err := r.RankingInputs(ctx, userID, p, limit)
	return input.Candidates, err
}

// RankingInputs is a rollout compatibility seam for callers that already hold
// a profile. New ranking workflows use ResolveRankingInputs so profile and
// overlay are selected inside one repeatable-read snapshot.
func (r *SQLRepository) RankingInputs(ctx context.Context, userID uuid.UUID, p profile.Profile, limit int) (RankingInputs, error) {
	return r.resolveRankingInputs(ctx, RankingRequest{UserID: userID}, limit, &p, false)
}

func (r *SQLRepository) ResolveRankingInputs(ctx context.Context, request RankingRequest, limit int) (RankingInputs, error) {
	return r.resolveRankingInputs(ctx, request, limit, nil, true)
}

func (r *SQLRepository) resolveRankingInputs(ctx context.Context, request RankingRequest, limit int, supplied *profile.Profile, requireOverlay bool) (RankingInputs, error) {
	if request.UserID == uuid.Nil || limit <= 0 {
		return RankingInputs{}, errors.New("ranking user and positive candidate limit are required")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RankingInputs{}, err
	}
	defer tx.Rollback(ctx)
	source, err := r.resolveSource(ctx, tx, request, supplied, requireOverlay)
	if err != nil {
		return RankingInputs{}, err
	}
	candidates, err := r.candidates(ctx, tx, request.UserID, source, limit)
	if err != nil {
		return RankingInputs{}, err
	}
	queries := r.store.Queries.WithTx(tx)
	rows, err := queries.ListEffectivePreferenceAdjustments(ctx, pgtype.UUID{Bytes: request.UserID, Valid: true})
	if err != nil {
		return RankingInputs{}, err
	}
	active := make([]preferences.Adjustment, 0, len(rows))
	for _, row := range rows {
		active = append(active, preferences.Adjustment{ID: uuid.UUID(row.ID.Bytes), SourceFeedbackID: uuid.UUID(row.SourceFeedbackID.Bytes), SourceGameID: uuid.UUID(row.SourceGameID.Bytes), Dimension: row.Dimension, Target: preferences.Mode(row.Target), Direction: row.Direction, Confidence: row.Confidence, Strength: row.Strength, Active: row.Active})
	}
	ids := make([]pgtype.UUID, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, pgtype.UUID{Bytes: c.GameID, Valid: true})
	}
	modes, err := queries.ListGameModes(ctx, ids)
	if err != nil {
		return RankingInputs{}, err
	}
	byID := map[uuid.UUID][]preferences.Mode{}
	for _, mode := range modes {
		if mode.Source == "steam_category" {
			id := uuid.UUID(mode.GameID.Bytes)
			byID[id] = append(byID[id], preferences.Mode(mode.Mode))
		}
	}
	for i := range candidates {
		c := &candidates[i]
		c.Modes = byID[c.GameID]
		c.Factors.NegativeModePreference = ModePreferenceFactor(c.GameID, c.Modes, active)
		c.ModePreferenceEvidence = ModePreferenceEvidence(c.GameID, c.Modes, active)
	}
	gameIDs := make([]uuid.UUID, 0, len(candidates))
	for _, candidate := range candidates {
		gameIDs = append(gameIDs, candidate.GameID)
	}
	corpus, err := r.explanationCorpus(ctx, tx, request.UserID, source.Activity.ID, gameIDs)
	if err != nil {
		return RankingInputs{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RankingInputs{}, err
	}
	return RankingInputs{Source: source, Candidates: candidates, EffectiveAdjustments: active, ActiveAdjustments: active, Corpus: corpus}, nil
}

func (r *SQLRepository) resolveSource(ctx context.Context, tx pgx.Tx, request RankingRequest, supplied *profile.Profile, requireOverlay bool) (RankingSource, error) {
	source := RankingSource{}
	if supplied != nil {
		source.Activity = *supplied
		source.ProfileAlgorithmVersion = "legacy"
	} else {
		var metadata []byte
		var vector *pgvector.Vector
		var createdAt pgtype.Timestamptz
		var buildKey, inputHash pgtype.Text
		err := tx.QueryRow(ctx, `SELECT p.id,p.user_id,p.version,p.publication_state,p.algorithm_metadata,p.created_at,p.build_key,e.input_content_hash,e.embedding
			FROM preference_profiles p
			LEFT JOIN profile_embeddings e ON e.profile_id=p.id AND e.polarity='positive' AND e.model=$3
			WHERE p.user_id=$1 AND ($2='' OR p.build_key=$2)
			ORDER BY p.version DESC LIMIT 1`, request.UserID, strings.TrimSpace(request.ProfileBuildKey), r.embeddingModel).Scan(
			&source.Activity.ID, &source.Activity.UserID, &source.Activity.Version, &source.Activity.PublicationState, &metadata, &createdAt, &buildKey, &inputHash, &vector,
		)
		if err != nil {
			return RankingSource{}, fmt.Errorf("resolve vector-complete activity profile: %w", err)
		}
		source.Activity.CreatedAt = createdAt.Time.UTC()
		if vector != nil {
			source.Activity.Positive = append([]float32(nil), vector.Slice()...)
		}
		source.ProfileBuildKey = buildKey.String
		source.ProfileInputContentHash = inputHash.String
		var profileMetadata struct {
			AlgorithmVersion string   `json:"algorithmVersion"`
			Confidence       float64  `json:"confidence"`
			PositiveStrength *float64 `json:"positiveStrength"`
		}
		_ = json.Unmarshal(metadata, &profileMetadata)
		source.ProfileAlgorithmVersion = strings.TrimSpace(profileMetadata.AlgorithmVersion)
		if source.ProfileAlgorithmVersion == "" {
			source.ProfileAlgorithmVersion = "legacy"
		}
		source.Activity.Confidence = clamp(profileMetadata.Confidence)
		source.Activity.PositiveStrength = 1
		if profileMetadata.PositiveStrength != nil {
			source.Activity.PositiveStrength = clamp(*profileMetadata.PositiveStrength)
		}
		if source.ProfileInputContentHash == "" {
			source.ProfileInputContentHash = noVectorProfileIdentity(source.Activity.ID, source.Activity.Version, source.ProfileAlgorithmVersion, source.ProfileBuildKey)
		}
	}
	queries := r.store.Queries.WithTx(tx)
	overlayRow, err := queries.GetLatestPreferenceOverlay(ctx, pgtype.UUID{Bytes: request.UserID, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) && !requireOverlay {
		return source, nil
	}
	if err != nil {
		return RankingSource{}, fmt.Errorf("resolve preference overlay: %w", err)
	}
	source.Overlay = preferences.Overlay{
		ID: uuid.UUID(overlayRow.ID.Bytes), UserID: uuid.UUID(overlayRow.UserID.Bytes), Version: overlayRow.Version,
		AlgorithmVersion: overlayRow.AlgorithmVersion, BuildKey: overlayRow.BuildKey, InputContentHash: overlayRow.InputContentHash,
	}
	if overlayRow.CreatedAt.Valid {
		source.Overlay.CreatedAt = overlayRow.CreatedAt.Time.UTC()
	}
	if err := json.Unmarshal(overlayRow.AlgorithmMetadata, &source.Overlay.Metadata); err != nil {
		return RankingSource{}, fmt.Errorf("decode preference overlay metadata: %w", err)
	}
	embeddings, err := queries.GetPreferenceOverlayEmbeddings(ctx, overlayRow.ID)
	if err != nil {
		return RankingSource{}, err
	}
	for _, embedding := range embeddings {
		if embedding.Model != r.embeddingModel {
			continue
		}
		vector := append([]float32(nil), embedding.Embedding.Slice()...)
		switch embedding.Polarity {
		case "positive":
			source.Overlay.Positive = vector
		case "negative":
			source.Overlay.Negative = vector
		}
	}
	return source, nil
}

func (r *SQLRepository) candidates(ctx context.Context, tx pgx.Tx, userID uuid.UUID, source RankingSource, limit int) ([]Candidate, error) {
	if limit <= 0 {
		return nil, errors.New("candidate limit must be positive")
	}
	byID := map[uuid.UUID]Candidate{}
	asOf := r.now().UTC()
	semantic := hasVector(source.Activity.Positive)
	base := "ug.game_id IS NULL"
	condition, order := base, `lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`
	args := []any{userID, r.embeddingModel, ActivityNeighborLimit}
	if semantic {
		condition = "e.embedding IS NOT NULL AND " + base
		order = `e.embedding <=> $4,lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`
		args = append(args, pgvector.NewVector(source.Activity.Positive))
	}
	if err := r.readCandidates(ctx, tx, userID, fmt.Sprintf(candidateSelect, condition, order), args, source, asOf, 0, byID); err != nil {
		return nil, err
	}
	if hasVector(source.Overlay.Positive) {
		overlayOrder := `e.embedding <=> $4,lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`
		args = []any{userID, r.embeddingModel, PreferenceNeighborLimit, pgvector.NewVector(source.Overlay.Positive)}
		if err := r.readCandidates(ctx, tx, userID, fmt.Sprintf(candidateSelect, "e.embedding IS NOT NULL AND "+base, overlayOrder), args, source, asOf, ActivityNeighborLimit, byID); err != nil {
			return nil, err
		}
	}
	// Wishlist entries are mandatory candidates even if they have no embedding
	// or fall outside the nearest-neighbor limit. Current catalog entries are
	// mandatory as well; eligibility and presentation caps are applied later.
	if err := r.readCandidates(ctx, tx, userID, fmt.Sprintf(candidateSelect, base, `lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`), []any{userID, r.embeddingModel, 1000000}, source, asOf, ActivityNeighborLimit+PreferenceNeighborLimit, byID); err != nil {
		return nil, err
	}
	ordered := make([]Candidate, 0, len(byID))
	for _, candidate := range byID {
		ordered = append(ordered, candidate)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].retrievalRank != ordered[j].retrievalRank {
			return ordered[i].retrievalRank < ordered[j].retrievalRank
		}
		return ordered[i].GameID.String() < ordered[j].GameID.String()
	})
	result := make([]Candidate, 0, len(byID))
	for _, candidate := range ordered {
		if Eligible(candidate) && withinReleaseWindow(candidate.Release, asOf, r.lookbackDays, r.horizonDays) {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := strings.ToLower(result[i].Name), strings.ToLower(result[j].Name)
		if left != right {
			return left < right
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].GameID.String() < result[j].GameID.String()
	})
	return result, nil
}

func withinReleaseWindow(window ReleaseWindow, now time.Time, lookbackDays, horizonDays int) bool {
	if window.Precision == "day" && window.ExactDate != "" {
		date, err := time.Parse(time.DateOnly, window.ExactDate)
		if err != nil {
			return false
		}
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return !date.Before(today.AddDate(0, 0, -lookbackDays)) && !date.After(today.AddDate(0, 0, horizonDays))
	}
	parsed := catalog.ParseReleaseDate(window.Raw, now)
	start, end, known := parsed.Interval()
	if !known {
		// Coarse year/Coming Soon/TBA admission is decided by the shared
		// provenance-aware eligibility reader.
		return true
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return !end.Before(today.AddDate(0, 0, -lookbackDays)) && !start.After(today.AddDate(0, 0, horizonDays))
}

func (r *SQLRepository) ExplanationCorpus(ctx context.Context, userID uuid.UUID, gameIDs []uuid.UUID) (EvidenceCorpus, error) {
	if len(gameIDs) == 0 {
		return EvidenceCorpus{Candidates: map[uuid.UUID]CandidateEvidence{}, Anchors: []EvidenceAnchor{}}, nil
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return EvidenceCorpus{}, err
	}
	defer tx.Rollback(ctx)
	var profileID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT p.id FROM preference_profiles p JOIN activity_profile_evidence evidence ON evidence.profile_id=p.id WHERE p.user_id=$1 ORDER BY p.version DESC LIMIT 1`, userID).Scan(&profileID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return EvidenceCorpus{}, err
	}
	corpus, err := r.explanationCorpus(ctx, tx, userID, profileID, gameIDs)
	if err != nil {
		return EvidenceCorpus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EvidenceCorpus{}, err
	}
	return corpus, nil
}

func (r *SQLRepository) explanationCorpus(ctx context.Context, queryer rowQuerier, userID, profileID uuid.UUID, gameIDs []uuid.UUID) (EvidenceCorpus, error) {
	corpus := EvidenceCorpus{Candidates: make(map[uuid.UUID]CandidateEvidence), Anchors: []EvidenceAnchor{}}
	if len(gameIDs) == 0 {
		return corpus, nil
	}

	rows, err := queryer.Query(ctx, `SELECT g.id,e.embedding,gf.features
		FROM games g
		LEFT JOIN game_embeddings e ON e.game_id=g.id AND e.model=$2
		LEFT JOIN game_features gf ON gf.game_id=g.id AND gf.model=$3
		WHERE g.id=ANY($1::uuid[])
		ORDER BY g.id`, gameIDs, r.embeddingModel, r.textModel)
	if err != nil {
		return EvidenceCorpus{}, fmt.Errorf("load explanation candidates: %w", err)
	}
	for rows.Next() {
		var candidate CandidateEvidence
		var vector *pgvector.Vector
		var rawFeatures []byte
		if err := rows.Scan(&candidate.GameID, &vector, &rawFeatures); err != nil {
			rows.Close()
			return EvidenceCorpus{}, fmt.Errorf("scan explanation candidate: %w", err)
		}
		if vector != nil {
			candidate.Vector = append([]float32(nil), vector.Slice()...)
		}
		candidate.Features, err = decodeFeatureValues(rawFeatures)
		if err != nil {
			rows.Close()
			return EvidenceCorpus{}, fmt.Errorf("decode explanation candidate features: %w", err)
		}
		corpus.Candidates[candidate.GameID] = candidate
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return EvidenceCorpus{}, fmt.Errorf("iterate explanation candidates: %w", err)
	}
	rows.Close()

	rows, err = queryer.Query(ctx, `SELECT evidence.game_id,g.name,e.embedding,evidence.playtime_minutes,evidence.recently_played_minutes,evidence.last_played_at,
			evidence.achieved,evidence.total,CASE WHEN evidence.total>0 THEN evidence.achieved::float8/evidence.total ELSE 0 END,
			evidence.completion_weight,gf.features
		FROM activity_profile_evidence evidence
		JOIN preference_profiles p ON p.id=evidence.profile_id AND p.user_id=$1
		JOIN games g ON g.id=evidence.game_id
		LEFT JOIN game_embeddings e ON e.game_id=evidence.game_id AND e.model=$3
		LEFT JOIN game_features gf ON gf.game_id=evidence.game_id AND gf.model=$4
		WHERE evidence.profile_id=$2
		ORDER BY evidence.game_id`, userID, profileID, r.embeddingModel, r.textModel)
	if err != nil {
		return EvidenceCorpus{}, fmt.Errorf("load explanation anchors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var anchor EvidenceAnchor
		var vector *pgvector.Vector
		var lastPlayedAt pgtype.Timestamptz
		var rawFeatures []byte
		if err := rows.Scan(
			&anchor.Signal.GameID,
			&anchor.Name,
			&vector,
			&anchor.Signal.PlaytimeMinutes,
			&anchor.Signal.RecentMinutes,
			&lastPlayedAt,
			&anchor.Signal.Achieved,
			&anchor.Signal.Total,
			&anchor.Signal.AchievementRatio,
			&anchor.Signal.CompletionWeight,
			&rawFeatures,
		); err != nil {
			return EvidenceCorpus{}, fmt.Errorf("scan explanation anchor: %w", err)
		}
		if lastPlayedAt.Valid {
			anchor.Signal.LastPlayedAt = lastPlayedAt.Time.UTC()
		}
		if vector != nil {
			anchor.Signal.Vector = append([]float32(nil), vector.Slice()...)
		}
		anchor.Features, err = decodeFeatureValues(rawFeatures)
		if err != nil {
			return EvidenceCorpus{}, fmt.Errorf("decode explanation anchor features: %w", err)
		}
		corpus.Anchors = append(corpus.Anchors, anchor)
	}
	if err := rows.Err(); err != nil {
		return EvidenceCorpus{}, fmt.Errorf("iterate explanation anchors: %w", err)
	}
	return corpus, nil
}

func decodeFeatureValues(raw []byte) (FeatureValues, error) {
	if len(raw) == 0 {
		return FeatureValues{}, nil
	}
	var features FeatureValues
	if err := json.Unmarshal(raw, &features); err != nil {
		return FeatureValues{}, err
	}
	return features, nil
}

func (r *SQLRepository) readCandidates(ctx context.Context, tx pgx.Tx, userID uuid.UUID, query string, args []any, source RankingSource, asOf time.Time, rankBase int, dst map[uuid.UUID]Candidate) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	rank := 0
	var collected []Candidate
	for rows.Next() {
		rank++
		var c Candidate
		var vector *pgvector.Vector
		var wishlist bool
		var liked bool
		var releaseDate pgtype.Date
		var raw pgtype.Text
		if err := rows.Scan(&c.GameID, &c.Name, &vector, &wishlist, &c.Owned, &c.Released, &c.ExcludedByLatestDirectAction, &liked, &c.Dislikes, &c.Release.Precision, &releaseDate, &raw); err != nil {
			return err
		}
		c.Factors.Wishlist = boolFactor(wishlist)
		c.Factors.PositiveDirectFeedback = boolFactor(liked)
		c.Factors.NegativeDirectFeedback = boolFactor(c.ExcludedByLatestDirectAction)
		c.Factors.Novelty = 1
		c.Factors.DateUncertainty = boolFactor(c.Release.Precision != "day")
		if releaseDate.Valid && c.Release.Precision == "day" {
			c.Release.ExactDate = releaseDate.Time.Format(time.DateOnly)
		}
		if raw.Valid {
			c.Release.Raw = raw.String
		}
		c.Released = c.Released || releaseWindowEnded(c.Release, asOf)
		quality := .25
		if c.Release.Precision != "unknown" {
			quality += .25
		}
		if vector != nil {
			slice := vector.Slice()
			c.Factors.ActivitySimilarity = cosine01(profile.Cosine(source.Activity.Positive, slice)) * source.Activity.PositiveStrength
			c.Factors.PositivePreferenceSimilarity = cosine01(profile.Cosine(source.Overlay.Positive, slice))
			c.Factors.NegativePreferenceSimilarity = cosine01(profile.Cosine(source.Overlay.Negative, slice))
			quality += .5
		}
		c.Factors.Confidence = quality
		c.retrievalRank = rankBase + rank
		collected = append(collected, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, c := range collected {
		values, err := r.affinities(ctx, tx, source.Activity.ID, c.GameID, asOf)
		if err != nil {
			return err
		}
		c.Factors.TagAffinity = values.Tag
		c.Factors.PlaytimeAffinity = values.Playtime
		c.Factors.AchievementAffinity = values.Achievement
		c.Factors.RecentActivityAffinity = values.Recent
		c.Factors.CompletionAffinity = values.Completion
		if existing, ok := dst[c.GameID]; ok && existing.retrievalRank < c.retrievalRank {
			c.retrievalRank = existing.retrievalRank
		}
		dst[c.GameID] = c
	}
	return nil
}

type affinities struct {
	Tag         float64
	Playtime    float64
	Achievement float64
	Recent      float64
	Completion  float64
}

func (r *SQLRepository) affinities(ctx context.Context, tx pgx.Tx, profileID, gameID uuid.UUID, asOf time.Time) (affinities, error) {
	var candidate []byte
	if err := tx.QueryRow(ctx, `SELECT features FROM game_features WHERE game_id=$1 AND model=$2`, gameID, r.textModel).Scan(&candidate); errors.Is(err, pgx.ErrNoRows) {
		return affinities{}, nil
	} else if err != nil {
		return affinities{}, err
	}
	target := featureSet(candidate)
	if len(target) == 0 {
		return affinities{}, nil
	}
	rows, err := tx.Query(ctx, `SELECT gf.features,evidence.playtime_minutes,evidence.recently_played_minutes,evidence.last_played_at,
		evidence.achieved,evidence.total,evidence.completion_weight
		FROM activity_profile_evidence evidence
		JOIN game_features gf ON gf.game_id=evidence.game_id AND gf.model=$2
		WHERE evidence.profile_id=$1
		ORDER BY evidence.game_id`, profileID, r.textModel)
	if err != nil {
		return affinities{}, err
	}
	defer rows.Close()
	var tag, play, ach, recent, completion, playDen, achDen, recentDen, completionDen float64
	for rows.Next() {
		var raw []byte
		var minutes, recentMinutes int64
		var lastPlayedAt pgtype.Timestamptz
		var got, total int
		var completionWeight float64
		if err := rows.Scan(&raw, &minutes, &recentMinutes, &lastPlayedAt, &got, &total, &completionWeight); err != nil {
			return affinities{}, err
		}
		overlap := setOverlap(target, featureSet(raw))
		lastPlayed := time.Time{}
		if lastPlayedAt.Valid {
			lastPlayed = lastPlayedAt.Time
		}
		recencyWeight := profile.HistoricalRecencyWeight(lastPlayed, asOf)
		if weightedOverlap := overlap * recencyWeight; weightedOverlap > tag {
			tag = weightedOverlap
		}
		pw := math.Log1p(float64(minutes))
		play += overlap * pw * recencyWeight
		playDen += pw
		rw := float64(recentMinutes)
		recent += overlap * rw
		recentDen += rw
		if completionWeight > 0 {
			completion += overlap * completionWeight
			completionDen += completionWeight
		} else if total > 0 {
			aw := float64(got) / float64(total)
			ach += overlap * aw * recencyWeight
			achDen += aw
		}
	}
	if err := rows.Err(); err != nil {
		return affinities{}, err
	}
	if playDen > 0 {
		play /= playDen
	}
	if achDen > 0 {
		ach /= achDen
	}
	if recentDen > 0 {
		recent /= recentDen
	}
	if completionDen > 0 {
		completion /= completionDen
	}
	return affinities{Tag: tag, Playtime: play, Achievement: ach, Recent: recent, Completion: completion}, nil
}
func featureSet(raw []byte) map[string]bool {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	result := map[string]bool{}
	for _, entry := range value {
		if values, ok := entry.([]any); ok {
			for _, item := range values {
				if text, ok := item.(string); ok {
					result[strings.ToLower(strings.TrimSpace(text))] = true
				}
			}
		}
	}
	return result
}
func setOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	common := 0
	for key := range a {
		if b[key] {
			common++
		}
	}
	den := len(a)
	if len(b) > den {
		den = len(b)
	}
	return float64(common) / float64(den)
}

func hasVector(vector []float32) bool {
	if len(vector) != profile.Dimensions {
		return false
	}
	for _, v := range vector {
		if v != 0 {
			return true
		}
	}
	return false
}
func cosine01(value float64) float64 {
	if value <= 0 {
		return 0
	}
	if value >= 1 {
		return 1
	}
	return value
}
func boolFactor(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func releaseWindowEnded(window ReleaseWindow, now time.Time) bool {
	if window.Precision == "day" && window.ExactDate != "" {
		date, err := time.Parse(time.DateOnly, window.ExactDate)
		return err == nil && date.Before(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))
	}
	parsed := catalog.ParseReleaseDate(window.Raw, now)
	switch parsed.Precision {
	case catalog.PrecisionMonth:
		return parsed.Year < now.Year() || parsed.Year == now.Year() && parsed.Month < now.Month()
	case catalog.PrecisionQuarter:
		currentQuarter := (int(now.Month())-1)/3 + 1
		return parsed.Year < now.Year() || parsed.Year == now.Year() && parsed.Quarter < currentQuarter
	case catalog.PrecisionYear:
		return parsed.Year < now.Year()
	default:
		return false
	}
}
func (r *SQLRepository) SaveRanking(ctx context.Context, userID uuid.UUID, source RankingSource, input []Recommendation, buildKey string) ([]Recommendation, int64, bool, error) {
	if source.Activity.ID == uuid.Nil || source.Overlay.ID == uuid.Nil {
		return nil, 0, false, errors.New("ranking source requires exact activity profile and preference overlay")
	}
	if !strings.HasPrefix(buildKey, "pair-v2:") {
		return nil, 0, false, errors.New("new ranking writes require an exact-pair build key")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, 0, false, err
	}
	defer tx.Rollback(ctx)
	persistedSource, err := r.persistedRankingSource(ctx, tx, userID, source.Activity.ID, source.Overlay.ID)
	if err != nil {
		return nil, 0, false, fmt.Errorf("validate ranking source: %w", err)
	}
	fields := strings.Split(buildKey, ":")
	if len(fields) != 4 || fields[0] != "pair-v2" || len(fields[1]) != 64 || fields[2] != AlgorithmVersion || fields[3] != rankingSourceDigest(persistedSource) {
		return nil, 0, false, errors.New("ranking build key does not match persisted exact pair")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `ranking:`+userID.String()); err != nil {
		return nil, 0, false, err
	}
	var rankingVersion int64
	var published bool
	var existingBuildKey string
	var existingProfileID uuid.UUID
	var existingOverlayID pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT ranking_version,published,build_key,profile_id,preference_overlay_id FROM recommendation_runs WHERE user_id=$1 AND (build_key=$2 OR ($3<>'' AND starts_with(build_key,$3))) ORDER BY ranking_version LIMIT 1`, userID, buildKey, rankingRequestPrefix(buildKey)).Scan(&rankingVersion, &published, &existingBuildKey, &existingProfileID, &existingOverlayID)
	replay := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, err
	}
	if replay && (existingBuildKey != buildKey || existingProfileID != source.Activity.ID || !existingOverlayID.Valid || uuid.UUID(existingOverlayID.Bytes) != source.Overlay.ID) {
		return nil, 0, false, errors.New("ranking build key collision")
	}
	if replay && published {
		if err = tx.Commit(ctx); err != nil {
			return nil, 0, false, err
		}
		return nil, rankingVersion, true, nil
	}
	if replay {
		if _, err = tx.Exec(ctx, `DELETE FROM recommendations WHERE user_id=$1 AND ranking_version=$2`, userID, rankingVersion); err != nil {
			return nil, 0, false, err
		}
		if _, err = tx.Exec(ctx, `UPDATE recommendation_runs SET published=false,profile_id=$3,preference_overlay_id=$4,build_key=$5 WHERE user_id=$1 AND ranking_version=$2`, userID, rankingVersion, source.Activity.ID, source.Overlay.ID, buildKey); err != nil {
			return nil, 0, false, err
		}
	} else if err = tx.QueryRow(ctx, `SELECT COALESCE(max(ranking_version),0)+1 FROM recommendation_runs WHERE user_id=$1`, userID).Scan(&rankingVersion); err != nil {
		return nil, 0, false, err
	}
	result := make([]Recommendation, 0, len(input))
	now := r.now().UTC()
	if !replay {
		if _, err = tx.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,preference_overlay_id,ranking_version,algorithm_version,build_key,created_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.New(), userID, source.Activity.ID, source.Overlay.ID, rankingVersion, AlgorithmVersion, buildKey, now); err != nil {
			return nil, 0, false, err
		}
	}
	for _, item := range input {
		id := uuid.New()
		item.RankingVersion = rankingVersion
		err = tx.QueryRow(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,preference_overlay_id,algorithm_version,ranking_version,score,text_model,embedding_model,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id,created_at`, id, userID, item.GameID, source.Activity.ID, source.Overlay.ID, item.AlgorithmVersion, rankingVersion, item.Score, nullableText(r.textModel), r.embeddingModel, now).Scan(&item.ID, &item.CreatedAt)
		if err != nil {
			return nil, 0, false, err
		}
		factors, _ := json.Marshal(item.Factors)
		weights, _ := json.Marshal(item.Weights)
		if _, err = tx.Exec(ctx, `INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3) ON CONFLICT(recommendation_id) DO UPDATE SET factors=EXCLUDED.factors,weights=EXCLUDED.weights`, item.ID, factors, weights); err != nil {
			return nil, 0, false, err
		}
		result = append(result, item)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, false, err
	}
	return result, rankingVersion, false, nil
}

func (r *SQLRepository) persistedRankingSource(ctx context.Context, tx pgx.Tx, userID, profileID, overlayID uuid.UUID) (RankingSource, error) {
	var source RankingSource
	var metadata []byte
	var profileBuildKey, profileInputHash pgtype.Text
	err := tx.QueryRow(ctx, `SELECT p.id,p.user_id,p.version,p.algorithm_metadata,p.build_key,e.input_content_hash
		FROM preference_profiles p
		LEFT JOIN profile_embeddings e ON e.profile_id=p.id AND e.polarity='positive' AND e.model=$3
		WHERE p.id=$1 AND p.user_id=$2`, profileID, userID, r.embeddingModel).Scan(
		&source.Activity.ID, &source.Activity.UserID, &source.Activity.Version, &metadata, &profileBuildKey, &profileInputHash,
	)
	if err != nil {
		return RankingSource{}, err
	}
	var profileMetadata struct {
		AlgorithmVersion string `json:"algorithmVersion"`
	}
	_ = json.Unmarshal(metadata, &profileMetadata)
	source.ProfileAlgorithmVersion = strings.TrimSpace(profileMetadata.AlgorithmVersion)
	if source.ProfileAlgorithmVersion == "" {
		source.ProfileAlgorithmVersion = "legacy"
	}
	source.ProfileBuildKey = profileBuildKey.String
	source.ProfileInputContentHash = profileInputHash.String
	if source.ProfileInputContentHash == "" {
		source.ProfileInputContentHash = noVectorProfileIdentity(source.Activity.ID, source.Activity.Version, source.ProfileAlgorithmVersion, source.ProfileBuildKey)
	}
	err = tx.QueryRow(ctx, `SELECT id,user_id,version,algorithm_version,build_key,input_content_hash FROM preference_overlays WHERE id=$1 AND user_id=$2`, overlayID, userID).Scan(
		&source.Overlay.ID, &source.Overlay.UserID, &source.Overlay.Version, &source.Overlay.AlgorithmVersion, &source.Overlay.BuildKey, &source.Overlay.InputContentHash,
	)
	if err != nil {
		return RankingSource{}, err
	}
	return source, nil
}

func nullableUUID(value uuid.UUID) any {
	if value == uuid.Nil {
		return nil
	}
	return value
}

func (r *SQLRepository) SaveExplanation(ctx context.Context, userID uuid.UUID, rankingVersion int64, buildKey string, id uuid.UUID, gameName string, record ExplanationRecord) error {
	stored := StoredEvidence{Version: StoredEvidenceVersion, Items: append([]GroundingEvidence(nil), record.Supplied...), CitedIDs: append([]string(nil), record.CitedIDs...)}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode explanation evidence: %w", err)
	}
	input, err := json.Marshal(struct {
		Model          string     `json:"model"`
		PromptVersion  string     `json:"promptVersion"`
		EvidenceSchema int        `json:"evidenceSchema"`
		Game           string     `json:"game"`
		Evidence       []Evidence `json:"evidence"`
	}{r.textModel, "recommendation-explanation-v3", StoredEvidenceVersion, gameName, PublicEvidence(record.Supplied)})
	if err != nil {
		return fmt.Errorf("encode explanation input: %w", err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(input))
	generated := record.Source == ExplanationSourceAI
	command, err := r.store.Pool.Exec(ctx, `UPDATE recommendations r SET explanation=$2,explanation_input_content_hash=$3,explanation_evidence=$4,explanation_generated=$5,explanation_source=$6,explanation_failure_reason=$7
		WHERE r.id=$1 AND r.user_id=$8 AND r.ranking_version=$9 AND EXISTS(
			SELECT 1 FROM recommendation_runs rr WHERE rr.user_id=$8 AND rr.ranking_version=$9 AND rr.build_key=$10 AND NOT rr.published
		)`, id, record.Text, hash, encoded, generated, record.Source, nullableExplanationFailure(record.FailureReason), userID, rankingVersion, buildKey)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *SQLRepository) GetExplanationByInput(ctx context.Context, userID, gameID uuid.UUID, model, inputHash string) (ai.Explanation, bool, error) {
	if userID == uuid.Nil || gameID == uuid.Nil || strings.TrimSpace(model) == "" || strings.TrimSpace(inputHash) == "" {
		return ai.Explanation{}, false, errors.New("explanation cache identity is required")
	}
	var raw []byte
	err := r.store.Pool.QueryRow(ctx, `SELECT explanation FROM catalog_explanation_cache WHERE user_id=$1 AND game_id=$2 AND model=$3 AND input_hash=$4`, userID, gameID, model, inputHash).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ai.Explanation{}, false, nil
	}
	if err != nil {
		return ai.Explanation{}, false, err
	}
	var value ai.Explanation
	if json.Unmarshal(raw, &value) != nil {
		return ai.Explanation{}, false, errors.New("invalid cached explanation")
	}
	return value, true, nil
}

func (r *SQLRepository) PutExplanationByInput(ctx context.Context, userID, gameID uuid.UUID, model, inputHash string, value ai.Explanation) error {
	if userID == uuid.Nil || gameID == uuid.Nil || strings.TrimSpace(model) == "" || strings.TrimSpace(inputHash) == "" {
		return errors.New("explanation cache identity is required")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, userID); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `INSERT INTO catalog_explanation_cache(user_id,game_id,model,input_hash,explanation)
		SELECT $1,$2,$3,$4,$5 FROM users
		WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$1)
		ON CONFLICT(user_id,game_id,model,input_hash) DO UPDATE SET explanation=EXCLUDED.explanation,created_at=now()`, userID, gameID, model, inputHash, raw)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}
func (r *SQLRepository) PublishRanking(ctx context.Context, userID uuid.UUID, version int64, buildKey string) error {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `ranking:`+userID.String()); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE recommendation_runs rr SET published=true
		WHERE rr.user_id=$1 AND rr.ranking_version=$2 AND rr.build_key=$3 AND NOT rr.published
		AND NOT EXISTS(
			SELECT 1 FROM recommendations r WHERE r.user_id=rr.user_id AND r.ranking_version=rr.ranking_version
			AND (r.explanation IS NULL OR r.explanation_input_content_hash IS NULL OR jsonb_typeof(r.explanation_evidence)<>'object' OR r.explanation_evidence->>'version'<>$4)
		)`, userID, version, buildKey, fmt.Sprint(StoredEvidenceVersion))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

// PublishedDeckRefreshTargets deliberately reads the requested immutable run,
// without current candidate/feedback filters or a latest-run lookup. A retry
// must recover all targets even if the catalog or a newer ranking has changed.
func (r *SQLRepository) PublishedDeckRefreshTargets(ctx context.Context, userID uuid.UUID, rankingVersion int64) ([]catalog.DeckRefreshTarget, error) {
	rows, err := r.store.Pool.Query(ctx, `SELECT DISTINCT r.game_id,g.steam_app_id,r.ranking_version
		FROM recommendations r JOIN games g ON g.id=r.game_id
		JOIN recommendation_runs run ON run.user_id=r.user_id AND run.ranking_version=r.ranking_version AND run.published
		WHERE r.user_id=$1 AND r.ranking_version=$2 ORDER BY g.steam_app_id,r.game_id`, userID, rankingVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []catalog.DeckRefreshTarget{}
	for rows.Next() {
		var target catalog.DeckRefreshTarget
		if err := rows.Scan(&target.GameID, &target.SteamAppID, &target.RankingVersion); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (r *SQLRepository) Latest(ctx context.Context, userID uuid.UUID) ([]Recommendation, error) {
	rows, err := r.store.Pool.Query(ctx, `SELECT r.id,r.game_id,g.steam_app_id,g.source_snapshot,g.name,p.version,r.ranking_version,r.algorithm_version,r.score,r.explanation,r.explanation_evidence,r.explanation_generated,r.explanation_source,r.explanation_failure_reason,r.created_at,rf.factors,rf.weights,
		COALESCE(rd.precision,'unknown'),rd.release_date,rd.raw_text,gc.windows,gc.mac,gc.linux,COALESCE(gc.deck_status,'unknown'),latest_feedback.action,latest_feedback.reasons,
		review.review_band,review.review_label,review.positive_percent,review.review_total,review.review_observed_at
	FROM recommendations r JOIN games g ON g.id=r.game_id JOIN preference_profiles p ON p.id=r.profile_id JOIN recommendation_factors rf ON rf.recommendation_id=r.id
	LEFT JOIN game_compatibility gc ON gc.game_id=g.id
	LEFT JOIN LATERAL(SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1)rd ON true
		LEFT JOIN LATERAL(SELECT f.action,f.reasons FROM feedback_events f JOIN recommendations source_rec ON source_rec.id=f.recommendation_id WHERE f.user_id=r.user_id AND source_rec.game_id=r.game_id AND f.action IN ('like','dislike','exclude') ORDER BY f.created_at DESC,f.id DESC LIMIT 1)latest_feedback ON true
	LEFT JOIN catalog_search_current review ON review.steam_app_id=g.steam_app_id AND review.coming_soon=false
	WHERE r.user_id=$1 AND r.ranking_version=(SELECT max(ranking_version) FROM recommendation_runs WHERE user_id=$1 AND published)
	AND (latest_feedback.action IS NULL OR latest_feedback.action='like')
	ORDER BY r.score DESC,lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Recommendation{}
	for rows.Next() {
		item, err := scanRecommendation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func (r *SQLRepository) Get(ctx context.Context, userID, id uuid.UUID) (Recommendation, error) {
	row := r.store.Pool.QueryRow(ctx, `SELECT r.id,r.game_id,g.steam_app_id,g.source_snapshot,g.name,p.version,r.ranking_version,r.algorithm_version,r.score,r.explanation,r.explanation_evidence,r.explanation_generated,r.explanation_source,r.explanation_failure_reason,r.created_at,rf.factors,rf.weights,
		COALESCE(rd.precision,'unknown'),rd.release_date,rd.raw_text,gc.windows,gc.mac,gc.linux,COALESCE(gc.deck_status,'unknown'),latest_feedback.action,latest_feedback.reasons,
		review.review_band,review.review_label,review.positive_percent,review.review_total,review.review_observed_at FROM recommendations r JOIN games g ON g.id=r.game_id JOIN preference_profiles p ON p.id=r.profile_id JOIN recommendation_factors rf ON rf.recommendation_id=r.id
	LEFT JOIN game_compatibility gc ON gc.game_id=g.id
		LEFT JOIN LATERAL(SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1)rd ON true
		LEFT JOIN LATERAL(SELECT f.action,f.reasons FROM feedback_events f JOIN recommendations source_rec ON source_rec.id=f.recommendation_id WHERE f.user_id=r.user_id AND source_rec.game_id=r.game_id AND f.action IN ('like','dislike') ORDER BY f.created_at DESC,f.id DESC LIMIT 1)latest_feedback ON true
		LEFT JOIN catalog_search_current review ON review.steam_app_id=g.steam_app_id AND review.coming_soon=false
		WHERE r.id=$1 AND r.user_id=$2 AND EXISTS(SELECT 1 FROM recommendation_runs run WHERE run.user_id=r.user_id AND run.ranking_version=r.ranking_version AND run.published)`, id, userID)
	return scanRecommendation(row)
}

type scanner interface{ Scan(...any) error }

func scanRecommendation(row scanner) (Recommendation, error) {
	var r Recommendation
	var explanation pgtype.Text
	var evidence []byte
	var generated bool
	var source string
	var failure pgtype.Text
	var factors, weights []byte
	var snapshot []byte
	var releaseDate pgtype.Date
	var raw pgtype.Text
	var windows, mac, linux pgtype.Bool
	var latestFeedback pgtype.Text
	var reviewBand, reviewLabel pgtype.Text
	var reviewPercent pgtype.Int4
	var reviewTotal pgtype.Int8
	var reviewObservedAt pgtype.Timestamptz
	if err := row.Scan(&r.ID, &r.GameID, &r.SteamAppID, &snapshot, &r.Name, &r.ProfileVersion, &r.RankingVersion, &r.AlgorithmVersion, &r.Score, &explanation, &evidence, &generated, &source, &failure, &r.CreatedAt, &factors, &weights, &r.Release.Precision, &releaseDate, &raw, &windows, &mac, &linux, &r.DeckCompatibility, &latestFeedback, &r.LatestFeedbackReasons, &reviewBand, &reviewLabel, &reviewPercent, &reviewTotal, &reviewObservedAt); err != nil {
		return Recommendation{}, err
	}
	if windows.Valid && mac.Valid && linux.Valid {
		r.Platforms = &Platforms{Windows: windows.Bool, Mac: mac.Bool, Linux: linux.Bool}
	}
	r.CoverImageURL, r.StoreURL = steamPresentation(r.SteamAppID, snapshot)
	r.Factors = decodeFactors(factors)
	r.Weights = decodeWeights(weights)
	r.Confidence = r.Factors.Confidence
	r.Wishlisted = clamp(r.Factors.Wishlist) > 0
	mapStoredExplanation(&r, explanation, evidence, generated, ExplanationSource(source), failure)
	if releaseDate.Valid && r.Release.Precision == "day" {
		r.Release.ExactDate = releaseDate.Time.Format(time.DateOnly)
	}
	if raw.Valid {
		r.Release.Raw = raw.String
	}
	if latestFeedback.Valid {
		action := latestFeedback.String
		r.LatestFeedbackAction = &action
	}
	r.Reviews = validReviewSummary(reviewBand, reviewLabel, reviewPercent, reviewTotal, reviewObservedAt)
	return r, nil
}

func decodeFactors(raw []byte) Factors {
	var result Factors
	_ = json.Unmarshal(raw, &result)
	var legacy struct {
		PositiveSimilarity float64 `json:"positiveSimilarity"`
		NegativeSimilarity float64 `json:"negativeSimilarity"`
		PositiveFeedback   float64 `json:"positiveFeedback"`
		NegativeFeedback   float64 `json:"negativeFeedback"`
	}
	_ = json.Unmarshal(raw, &legacy)
	if result.ActivitySimilarity == 0 {
		result.ActivitySimilarity = legacy.PositiveSimilarity
	}
	if result.NegativePreferenceSimilarity == 0 {
		result.NegativePreferenceSimilarity = legacy.NegativeSimilarity
	}
	if result.PositiveDirectFeedback == 0 {
		result.PositiveDirectFeedback = legacy.PositiveFeedback
	}
	if result.NegativeDirectFeedback == 0 {
		result.NegativeDirectFeedback = legacy.NegativeFeedback
	}
	return result
}

func decodeWeights(raw []byte) Weights {
	var result Weights
	_ = json.Unmarshal(raw, &result)
	var legacy struct {
		SemanticSimilarity           float64 `json:"semanticSimilarity"`
		PositiveFeedback             float64 `json:"positiveFeedback"`
		NegativeFeedback             float64 `json:"negativeFeedback"`
		NegativeSimilarityMultiplier float64 `json:"negativeSimilarityMultiplier"`
		NegativeModePreferenceWeight float64 `json:"negativeModePreferenceWeight"`
	}
	_ = json.Unmarshal(raw, &legacy)
	if result.ActivitySimilarity == 0 {
		result.ActivitySimilarity = legacy.SemanticSimilarity
	}
	if result.PositiveDirectFeedback == 0 {
		result.PositiveDirectFeedback = legacy.PositiveFeedback
	}
	if result.NegativeDirectFeedback == 0 {
		result.NegativeDirectFeedback = legacy.NegativeFeedback
	}
	if result.NegativePreferenceSimilarity == 0 && legacy.SemanticSimilarity != 0 {
		result.NegativePreferenceSimilarity = -legacy.SemanticSimilarity * legacy.NegativeSimilarityMultiplier
	}
	if result.NegativeModePreference == 0 {
		result.NegativeModePreference = legacy.NegativeModePreferenceWeight
	}
	return result
}

func validReviewSummary(band, label pgtype.Text, percent pgtype.Int4, total pgtype.Int8, observedAt pgtype.Timestamptz) *ReviewSummary {
	const maxJavaScriptSafeInteger = int64(9_007_199_254_740_991)
	if !band.Valid || !label.Valid || !total.Valid || total.Int64 < 0 || total.Int64 > maxJavaScriptSafeInteger || !observedAt.Valid || observedAt.Time.IsZero() {
		return nil
	}
	cleanLabel := strings.TrimSpace(label.String)
	if cleanLabel == "" || len([]rune(cleanLabel)) > 64 || strings.ContainsAny(cleanLabel, "<>") {
		return nil
	}
	for _, character := range cleanLabel {
		if character < 32 || character == 127 {
			return nil
		}
	}
	review := &ReviewSummary{Band: ReviewBand(band.String), Label: cleanLabel, Total: total.Int64, ObservedAt: observedAt.Time.UTC()}
	switch review.Band {
	case ReviewUnrated:
		if percent.Valid || review.Total != 0 {
			return nil
		}
	case ReviewPositive, ReviewMixedOrBelow:
		if !percent.Valid || percent.Int32 < 0 || percent.Int32 > 100 || review.Total <= 0 {
			return nil
		}
		value := int(percent.Int32)
		review.PositivePercent = &value
	default:
		return nil
	}
	return review
}

func (r *SQLRepository) RankingProfileVersion(ctx context.Context, userID uuid.UUID, rankingVersion int64) (int64, error) {
	var version int64
	err := r.store.Pool.QueryRow(ctx, `SELECT p.version
		FROM recommendation_runs run
		JOIN preference_profiles p ON p.id=run.profile_id AND p.user_id=run.user_id
		WHERE run.user_id=$1 AND run.ranking_version=$2 AND run.published`, userID, rankingVersion).Scan(&version)
	return version, err
}

func nullableExplanationFailure(reason ExplanationFailureReason) any {
	if reason == "" {
		return nil
	}
	return string(reason)
}

func mapStoredExplanation(r *Recommendation, explanation pgtype.Text, raw []byte, legacyGenerated bool, source ExplanationSource, failure pgtype.Text) {
	r.explanationLoaded = true
	r.Explanation = neutralExplanation
	r.ExplanationSource = ExplanationSourceNone
	r.GeneratedExplanation = false
	r.Evidence = []Evidence{}
	if !explanation.Valid || !safeRussianExplanation(explanation.String) || !validPersistedExplanationStatus(source, failure) {
		return
	}

	var stored StoredEvidence
	if json.Unmarshal(raw, &stored) == nil && (stored.Version == StoredEvidenceVersion || stored.Version == 2) {
		if !validStoredEvidence(stored, source) {
			return
		}
		for _, item := range stored.Items {
			if item.Kind == EvidenceNegativeModePreference && clamp(r.Factors.NegativeModePreference) == 0 {
				return
			}
		}
		text := strings.TrimSpace(explanation.String)
		switch source {
		case ExplanationSourceNone:
			return
		case ExplanationSourceAI:
			if containsUnsupportedAffinityClaim(text) {
				return
			}
		case ExplanationSourceDeterministic:
			if text != DeterministicExplanation(stored.Items) {
				return
			}
		}
		r.Explanation = text
		r.ExplanationSource = source
		r.GeneratedExplanation = source == ExplanationSourceAI
		switch source {
		case ExplanationSourceAI:
			r.Evidence = PublicEvidence(citedGroundingEvidence(stored.Items, stored.CitedIDs))
		case ExplanationSourceDeterministic:
			r.Evidence = PublicEvidence(stored.Items)
		}
		return
	}

	// Legacy rows stored a public, unversioned evidence array. Only safe
	// generated output with recognizable citations survives this compatibility
	// cycle; all other legacy data degrades to neutral presentation.
	if source != ExplanationSourceAI || !legacyGenerated {
		return
	}
	var legacy []Evidence
	if json.Unmarshal(raw, &legacy) != nil || !validLegacyEvidence(legacy) || containsUnsupportedAffinityClaim(explanation.String) {
		return
	}
	r.Explanation = strings.TrimSpace(explanation.String)
	r.ExplanationSource = ExplanationSourceAI
	r.GeneratedExplanation = true
	r.Evidence = legacy
}

func validPersistedExplanationStatus(source ExplanationSource, failure pgtype.Text) bool {
	if source != ExplanationSourceAI && source != ExplanationSourceDeterministic && source != ExplanationSourceNone {
		return false
	}
	if !failure.Valid {
		return true
	}
	return source != ExplanationSourceAI && validExplanationFailure(ExplanationFailureReason(failure.String))
}

func validExplanationFailure(reason ExplanationFailureReason) bool {
	switch reason {
	case ExplanationFailureDisabled, ExplanationFailureRateLimited, ExplanationFailureTimeout,
		ExplanationFailureProviderUnavailable, ExplanationFailureRefusal, ExplanationFailureInvalidOutput,
		ExplanationFailureInvalidCitation, ExplanationFailureUnsafeText, ExplanationFailureUnknown:
		return true
	default:
		return false
	}
}

func validStoredEvidence(stored StoredEvidence, source ExplanationSource) bool {
	seen := make(map[string]struct{}, len(stored.Items))
	for _, item := range stored.Items {
		if stored.Version == 2 && item.Kind == EvidenceCompletionAnchor {
			return false
		}
		if !validGroundingEvidence(item) {
			return false
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return false
		}
		seen[item.ID] = struct{}{}
	}
	if source == ExplanationSourceNone {
		return len(stored.Items) == 0 && len(stored.CitedIDs) == 0
	}
	if source == ExplanationSourceAI {
		return validStoredCitations(stored.Items, stored.CitedIDs)
	}
	return len(stored.Items) > 0 && (len(stored.CitedIDs) == 0 || validStoredCitations(stored.Items, stored.CitedIDs))
}

func validStoredCitations(items []GroundingEvidence, ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	allowed := make(map[string]struct{}, len(items))
	for _, item := range items {
		if substantiveKind(item.Kind) {
			allowed[item.ID] = struct{}{}
		}
	}
	return validExplanationCitations(ids, allowed)
}

func citedGroundingEvidence(items []GroundingEvidence, ids []string) []GroundingEvidence {
	allowed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	result := make([]GroundingEvidence, 0, len(ids))
	for _, item := range items {
		if _, ok := allowed[item.ID]; ok || item.Kind == EvidenceNegativeModePreference {
			result = append(result, item)
		}
	}
	return result
}

func validLegacyEvidence(items []Evidence) bool {
	if len(items) == 0 || len(items) > 3 {
		return false
	}
	seen := map[string]bool{}
	hasSubstantive := false
	for i := range items {
		item := &items[i]
		if seen[item.ID] || !validEvidenceID(item.ID) || strings.TrimSpace(item.Text) == "" || !safeRussianExplanation(item.Text) {
			return false
		}
		seen[item.ID] = true
		switch item.ID {
		case "semantic-affinity":
			item.Kind = EvidenceSimilarGame
			hasSubstantive = true
		case "tag-affinity":
			item.Kind = EvidenceFeatureOverlap
			hasSubstantive = true
		case "playtime-affinity":
			item.Kind = EvidencePlaytimeAnchor
			hasSubstantive = true
		case "achievement-affinity":
			item.Kind = EvidenceAchievementAnchor
			hasSubstantive = true
		case "recent-activity":
			item.Kind = EvidenceRecentActivityAnchor
			hasSubstantive = true
		case "positive-feedback":
			item.Kind = EvidencePositiveFeedback
			hasSubstantive = true
		case "wishlist":
			item.Kind = EvidenceWishlistContext
		default:
			return false
		}
	}
	return hasSubstantive
}

func steamPresentation(appID int64, snapshot []byte) (string, string) {
	if appID <= 0 {
		return "", ""
	}
	storeURL := fmt.Sprintf("https://store.steampowered.com/app/%d/", appID)
	var details steam.GameDetails
	if len(snapshot) == 0 || json.Unmarshal(snapshot, &details) != nil {
		return "", storeURL
	}
	coverURL, err := steam.ValidateCoverURL(details.HeaderImage)
	if err != nil {
		return "", storeURL
	}
	return coverURL, storeURL
}
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var _ Repository = (*SQLRepository)(nil)
