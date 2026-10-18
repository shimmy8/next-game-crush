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
	dbgen "next-game-crush/internal/store/sqlc"
)

type SQLRepository struct {
	store                     *store.Store
	embeddingModel, textModel string
	now                       func() time.Time
	horizonDays               int
}

type rowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func NewSQLRepository(database *store.Store, embeddingModel, textModel string) (*SQLRepository, error) {
	if database == nil || strings.TrimSpace(embeddingModel) == "" {
		return nil, errors.New("recommendation store and embedding model are required")
	}
	return &SQLRepository{store: database, embeddingModel: embeddingModel, textModel: textModel, now: time.Now, horizonDays: 90}, nil
}

// SetHorizonDays configures shared candidate retrieval before the repository is used.
// Wishlist candidates remain mandatory regardless of this horizon.
func (r *SQLRepository) SetHorizonDays(days int) error {
	if days < 1 || days > 366 {
		return errors.New("recommendation horizon must be between 1 and 366 days")
	}
	r.horizonDays = days
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
	(COALESCE(cr.precision,'unknown')='day' AND cr.release_date<CURRENT_DATE),COALESCE(ld.action IN ('dislike','exclude'),false),COALESCE(f.likes,0),COALESCE(f.dislikes,0),
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
	AND jsonb_typeof(g.source_snapshot->'Name')='string' AND btrim(g.source_snapshot->>'Name')<>''
	AND jsonb_typeof(g.source_snapshot->'Type')='string' AND btrim(g.source_snapshot->>'Type')<>''
	AND jsonb_typeof(g.source_snapshot->'ReleaseDate'->'ComingSoon')='boolean'
	AND jsonb_typeof(g.source_snapshot->'ReleaseDate'->'Raw')='string'
	AND (g.source_snapshot->'ReleaseDate'->>'ComingSoon'='true' OR btrim(g.source_snapshot->'ReleaseDate'->>'Raw')<>'')
	AND (g.source_snapshot->'ReleaseDate'->>'ComingSoon'='false' OR lower(btrim(g.source_snapshot->>'Type'))<>'game'),false)
	AND %s ORDER BY %s LIMIT $3`

func (r *SQLRepository) Candidates(ctx context.Context, userID uuid.UUID, p profile.Profile, limit int) ([]Candidate, error) {
	input, err := r.RankingInputs(ctx, userID, p, limit)
	return input.Candidates, err
}

func (r *SQLRepository) RankingInputs(ctx context.Context, userID uuid.UUID, p profile.Profile, limit int) (RankingInputs, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RankingInputs{}, err
	}
	defer tx.Rollback(ctx)
	candidates, err := r.candidates(ctx, tx, userID, p, limit)
	if err != nil {
		return RankingInputs{}, err
	}
	queries := r.store.Queries.WithTx(tx)
	rows, err := queries.ListPreferenceAdjustments(ctx, dbgen.ListPreferenceAdjustmentsParams{UserID: pgtype.UUID{Bytes: userID, Valid: true}})
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
	corpus, err := r.explanationCorpus(ctx, tx, userID, gameIDs)
	if err != nil {
		return RankingInputs{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RankingInputs{}, err
	}
	return RankingInputs{Candidates: candidates, ActiveAdjustments: active, Corpus: corpus}, nil
}

func (r *SQLRepository) candidates(ctx context.Context, tx pgx.Tx, userID uuid.UUID, p profile.Profile, limit int) ([]Candidate, error) {
	if limit <= 0 {
		return nil, errors.New("candidate limit must be positive")
	}
	byID := map[uuid.UUID]Candidate{}
	asOf := r.now().UTC()
	semantic := hasVector(p.Positive)
	base := "ug.game_id IS NULL AND NOT (COALESCE(cr.precision,'unknown')='day' AND cr.release_date<CURRENT_DATE)"
	condition, order := base, `lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`
	args := []any{userID, r.embeddingModel, 1000000}
	if semantic {
		condition = "e.embedding IS NOT NULL AND " + base
		order = `e.embedding <=> $4,lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`
		args = append(args, pgvector.NewVector(p.Positive))
	}
	if err := r.readCandidates(ctx, tx, userID, fmt.Sprintf(candidateSelect, condition, order), args, p, asOf, byID); err != nil {
		return nil, err
	}
	// Wishlist entries are mandatory candidates even if they have no embedding
	// or fall outside the nearest-neighbor limit.
	if err := r.readCandidates(ctx, tx, userID, fmt.Sprintf(candidateSelect, "w.game_id IS NOT NULL AND "+base, `lower(g.name) COLLATE "C",g.name COLLATE "C",g.id`), []any{userID, r.embeddingModel, 1000000}, p, asOf, byID); err != nil {
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
	nonWishlist := 0
	for _, candidate := range ordered {
		if Eligible(candidate) && (candidate.Factors.Wishlist == 1 || withinHorizon(candidate.Release, asOf, r.horizonDays)) {
			if candidate.Factors.Wishlist == 1 || nonWishlist < limit {
				result = append(result, candidate)
				if candidate.Factors.Wishlist != 1 {
					nonWishlist++
				}
			}
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

func (r *SQLRepository) ExplanationCorpus(ctx context.Context, userID uuid.UUID, gameIDs []uuid.UUID) (EvidenceCorpus, error) {
	if len(gameIDs) == 0 {
		return EvidenceCorpus{Candidates: map[uuid.UUID]CandidateEvidence{}, Anchors: []EvidenceAnchor{}}, nil
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return EvidenceCorpus{}, err
	}
	defer tx.Rollback(ctx)
	corpus, err := r.explanationCorpus(ctx, tx, userID, gameIDs)
	if err != nil {
		return EvidenceCorpus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EvidenceCorpus{}, err
	}
	return corpus, nil
}

func (r *SQLRepository) explanationCorpus(ctx context.Context, queryer rowQuerier, userID uuid.UUID, gameIDs []uuid.UUID) (EvidenceCorpus, error) {
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

	rows, err = queryer.Query(ctx, `WITH relevant AS (
			SELECT game_id FROM user_games WHERE user_id=$1 UNION
			SELECT game_id FROM wishlist_entries WHERE user_id=$1 UNION
			SELECT rec.game_id FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id WHERE f.user_id=$1 UNION
			SELECT game_id FROM user_game_completion_projections WHERE user_id=$1
		), feedback AS (
			SELECT rec.game_id,
				(count(*) FILTER (WHERE f.action='like'))::int likes,
				(count(*) FILTER (WHERE f.action IN ('dislike','exclude')))::int dislikes
			FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id
			WHERE f.user_id=$1 GROUP BY rec.game_id
		), latest_direct AS (
			SELECT DISTINCT ON (rec.game_id) rec.game_id,f.action
			FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id
			WHERE f.user_id=$1 AND f.action IN ('like','dislike','exclude')
			ORDER BY rec.game_id,f.created_at DESC,f.id DESC
		)
		SELECT rel.game_id,g.name,e.embedding,COALESCE(ug.playtime_minutes,0),COALESCE(ug.recently_played_minutes,0),ug.last_played_at,
			CASE WHEN COALESCE(a.total,0)>0 THEN a.achieved::float8/a.total ELSE 0 END,
			COALESCE(cp.weight,0),COALESCE(ld.action IN ('dislike','exclude'),false),
			(w.game_id IS NOT NULL),COALESCE(f.likes,0),COALESCE(f.dislikes,0),gf.features
		FROM relevant rel
		JOIN games g ON g.id=rel.game_id
		JOIN game_embeddings e ON e.game_id=rel.game_id AND e.model=$2
		LEFT JOIN user_games ug ON ug.user_id=$1 AND ug.game_id=rel.game_id
		LEFT JOIN wishlist_entries w ON w.user_id=$1 AND w.game_id=rel.game_id
		LEFT JOIN achievement_summaries a ON a.user_id=$1 AND a.game_id=rel.game_id
		LEFT JOIN user_game_completion_projections cp ON cp.user_id=$1 AND cp.game_id=rel.game_id
		LEFT JOIN feedback f ON f.game_id=rel.game_id
		LEFT JOIN latest_direct ld ON ld.game_id=rel.game_id
		LEFT JOIN game_features gf ON gf.game_id=rel.game_id AND gf.model=$3
		ORDER BY rel.game_id`, userID, r.embeddingModel, r.textModel)
	if err != nil {
		return EvidenceCorpus{}, fmt.Errorf("load explanation anchors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var anchor EvidenceAnchor
		var vector pgvector.Vector
		var lastPlayedAt pgtype.Timestamptz
		var rawFeatures []byte
		if err := rows.Scan(
			&anchor.Signal.GameID,
			&anchor.Name,
			&vector,
			&anchor.Signal.PlaytimeMinutes,
			&anchor.Signal.RecentMinutes,
			&lastPlayedAt,
			&anchor.Signal.AchievementRatio,
			&anchor.Signal.CompletionWeight,
			&anchor.Signal.CompletionOverridden,
			&anchor.Signal.Wishlisted,
			&anchor.Signal.Likes,
			&anchor.Signal.Dislikes,
			&rawFeatures,
		); err != nil {
			return EvidenceCorpus{}, fmt.Errorf("scan explanation anchor: %w", err)
		}
		if lastPlayedAt.Valid {
			anchor.Signal.LastPlayedAt = lastPlayedAt.Time.UTC()
		}
		anchor.Signal.Vector = append([]float32(nil), vector.Slice()...)
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

func (r *SQLRepository) readCandidates(ctx context.Context, tx pgx.Tx, userID uuid.UUID, query string, args []any, p profile.Profile, asOf time.Time, dst map[uuid.UUID]Candidate) error {
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
		var likes int
		var releaseDate pgtype.Date
		var raw pgtype.Text
		if err := rows.Scan(&c.GameID, &c.Name, &vector, &wishlist, &c.Owned, &c.Released, &c.ExcludedByLatestDirectAction, &likes, &c.Dislikes, &c.Release.Precision, &releaseDate, &raw); err != nil {
			return err
		}
		c.Factors.Wishlist = boolFactor(wishlist)
		c.Factors.PositiveFeedback = boolFactor(likes > 0)
		c.Factors.NegativeFeedback = boolFactor(c.Dislikes > 0)
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
			c.Factors.PositiveSimilarity = cosine01(profile.Cosine(p.Positive, slice)) * p.PositiveStrength
			c.Factors.NegativeSimilarity = cosine01(profile.Cosine(p.Negative, slice))
			quality += .5
		}
		c.Factors.Confidence = quality
		c.retrievalRank = rank
		collected = append(collected, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, c := range collected {
		values, err := r.affinities(ctx, tx, userID, c.GameID, asOf)
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

func (r *SQLRepository) affinities(ctx context.Context, tx pgx.Tx, userID, gameID uuid.UUID, asOf time.Time) (affinities, error) {
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
	rows, err := tx.Query(ctx, `SELECT gf.features,ug.playtime_minutes,ug.recently_played_minutes,ug.last_played_at,
		COALESCE(a.achieved,0),COALESCE(a.total,0),COALESCE(cp.weight,0),COALESCE(ld.action IN ('dislike','exclude'),false)
		FROM user_games ug
		JOIN game_features gf ON gf.game_id=ug.game_id AND gf.model=$2
		LEFT JOIN achievement_summaries a ON a.user_id=ug.user_id AND a.game_id=ug.game_id
		LEFT JOIN user_game_completion_projections cp ON cp.user_id=ug.user_id AND cp.game_id=ug.game_id
		LEFT JOIN LATERAL (
			SELECT f.action FROM feedback_events f
			JOIN recommendations rec ON rec.id=f.recommendation_id
			WHERE f.user_id=ug.user_id AND rec.game_id=ug.game_id AND f.action IN ('like','dislike','exclude')
			ORDER BY f.created_at DESC,f.id DESC LIMIT 1
		) ld ON true
		WHERE ug.user_id=$1
		ORDER BY ug.game_id`, userID, r.textModel)
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
		var completionOverridden bool
		if err := rows.Scan(&raw, &minutes, &recentMinutes, &lastPlayedAt, &got, &total, &completionWeight, &completionOverridden); err != nil {
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
			if !completionOverridden {
				completion += overlap * completionWeight
				completionDen += completionWeight
			}
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
func withinHorizon(window ReleaseWindow, now time.Time, days int) bool {
	if window.Precision == "day" && window.ExactDate != "" {
		date, err := time.Parse(time.DateOnly, window.ExactDate)
		return err == nil && !date.After(now.AddDate(0, 0, days))
	}
	parsed := catalog.ParseReleaseDate(window.Raw, now)
	end := now.AddDate(0, 0, days)
	switch parsed.Precision {
	case catalog.PrecisionMonth:
		return parsed.Year < end.Year() || parsed.Year == end.Year() && parsed.Month <= end.Month()
	case catalog.PrecisionQuarter:
		return parsed.Year < end.Year() || parsed.Year == end.Year() && parsed.Quarter <= ((int(end.Month())-1)/3+1)
	case catalog.PrecisionYear:
		return parsed.Year <= end.Year()
	default:
		return true
	}
}

func (r *SQLRepository) SaveRanking(ctx context.Context, userID uuid.UUID, p profile.Profile, input []Recommendation, buildKey string) ([]Recommendation, int64, bool, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, 0, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `ranking:`+userID.String()); err != nil {
		return nil, 0, false, err
	}
	var rankingVersion int64
	var published bool
	err = tx.QueryRow(ctx, `SELECT ranking_version,published FROM recommendation_runs WHERE user_id=$1 AND (build_key=$2 OR ($3<>'' AND starts_with(build_key,$3))) ORDER BY ranking_version LIMIT 1`, userID, buildKey, rankingRequestPrefix(buildKey)).Scan(&rankingVersion, &published)
	replay := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, err
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
		if _, err = tx.Exec(ctx, `UPDATE recommendation_runs SET published=false,profile_id=$3,build_key=$4 WHERE user_id=$1 AND ranking_version=$2`, userID, rankingVersion, p.ID, buildKey); err != nil {
			return nil, 0, false, err
		}
	} else if err = tx.QueryRow(ctx, `SELECT COALESCE(max(ranking_version),0)+1 FROM recommendation_runs WHERE user_id=$1`, userID).Scan(&rankingVersion); err != nil {
		return nil, 0, false, err
	}
	result := make([]Recommendation, 0, len(input))
	now := r.now().UTC()
	if !replay {
		if _, err = tx.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,created_at)VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), userID, p.ID, rankingVersion, AlgorithmVersion, buildKey, now); err != nil {
			return nil, 0, false, err
		}
	}
	for _, item := range input {
		id := uuid.New()
		item.RankingVersion = rankingVersion
		err = tx.QueryRow(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score,text_model,embedding_model,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id,created_at`, id, userID, item.GameID, p.ID, item.AlgorithmVersion, rankingVersion, item.Score, nullableText(r.textModel), r.embeddingModel, now).Scan(&item.ID, &item.CreatedAt)
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

func (r *SQLRepository) Latest(ctx context.Context, userID uuid.UUID) ([]Recommendation, error) {
	rows, err := r.store.Pool.Query(ctx, `SELECT r.id,r.game_id,g.steam_app_id,g.source_snapshot,g.name,p.version,r.ranking_version,r.algorithm_version,r.score,r.explanation,r.explanation_evidence,r.explanation_generated,r.explanation_source,r.explanation_failure_reason,r.created_at,rf.factors,rf.weights,
		COALESCE(rd.precision,'unknown'),rd.release_date,rd.raw_text,gc.windows,gc.mac,gc.linux,COALESCE(gc.deck_status,'unknown'),latest_feedback.action,latest_feedback.reasons
	FROM recommendations r JOIN games g ON g.id=r.game_id JOIN preference_profiles p ON p.id=r.profile_id JOIN recommendation_factors rf ON rf.recommendation_id=r.id
	LEFT JOIN game_compatibility gc ON gc.game_id=g.id
	LEFT JOIN LATERAL(SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1)rd ON true
		LEFT JOIN LATERAL(SELECT f.action,f.reasons FROM feedback_events f JOIN recommendations source_rec ON source_rec.id=f.recommendation_id WHERE f.user_id=r.user_id AND source_rec.game_id=r.game_id AND f.action IN ('like','dislike','exclude') ORDER BY f.created_at DESC,f.id DESC LIMIT 1)latest_feedback ON true
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
		COALESCE(rd.precision,'unknown'),rd.release_date,rd.raw_text,gc.windows,gc.mac,gc.linux,COALESCE(gc.deck_status,'unknown'),latest_feedback.action,latest_feedback.reasons FROM recommendations r JOIN games g ON g.id=r.game_id JOIN preference_profiles p ON p.id=r.profile_id JOIN recommendation_factors rf ON rf.recommendation_id=r.id
	LEFT JOIN game_compatibility gc ON gc.game_id=g.id
		LEFT JOIN LATERAL(SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1)rd ON true
		LEFT JOIN LATERAL(SELECT f.action,f.reasons FROM feedback_events f JOIN recommendations source_rec ON source_rec.id=f.recommendation_id WHERE f.user_id=r.user_id AND source_rec.game_id=r.game_id AND f.action IN ('like','dislike') ORDER BY f.created_at DESC,f.id DESC LIMIT 1)latest_feedback ON true
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
	if err := row.Scan(&r.ID, &r.GameID, &r.SteamAppID, &snapshot, &r.Name, &r.ProfileVersion, &r.RankingVersion, &r.AlgorithmVersion, &r.Score, &explanation, &evidence, &generated, &source, &failure, &r.CreatedAt, &factors, &weights, &r.Release.Precision, &releaseDate, &raw, &windows, &mac, &linux, &r.DeckCompatibility, &latestFeedback, &r.LatestFeedbackReasons); err != nil {
		return Recommendation{}, err
	}
	if windows.Valid && mac.Valid && linux.Valid {
		r.Platforms = &Platforms{Windows: windows.Bool, Mac: mac.Bool, Linux: linux.Bool}
	}
	r.CoverImageURL, r.StoreURL = steamPresentation(r.SteamAppID, snapshot)
	_ = json.Unmarshal(factors, &r.Factors)
	_ = json.Unmarshal(weights, &r.Weights)
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
	return r, nil
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
