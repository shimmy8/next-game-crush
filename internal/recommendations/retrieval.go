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
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/store"
)

type SQLRepository struct {
	store                     *store.Store
	embeddingModel, textModel string
	now                       func() time.Time
}

func NewSQLRepository(database *store.Store, embeddingModel, textModel string) (*SQLRepository, error) {
	if database == nil || strings.TrimSpace(embeddingModel) == "" {
		return nil, errors.New("recommendation store and embedding model are required")
	}
	return &SQLRepository{store: database, embeddingModel: embeddingModel, textModel: textModel, now: time.Now}, nil
}

const candidateSelect = `WITH feedback AS (
	SELECT rec.game_id,count(*) FILTER (WHERE f.action='like')::int likes,
	count(*) FILTER (WHERE f.action IN ('dislike','exclude'))::int dislikes,
	bool_or(f.action='exclude') hidden
	FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id WHERE f.user_id=$1 GROUP BY rec.game_id
), current_release AS (
	SELECT DISTINCT ON (game_id) game_id,precision,release_date,raw_text FROM game_release_dates ORDER BY game_id,observed_at DESC,id DESC
)
SELECT g.id,g.name,e.embedding,(w.game_id IS NOT NULL),(ug.game_id IS NOT NULL),
	(COALESCE(cr.precision,'unknown')='day' AND cr.release_date<CURRENT_DATE),COALESCE(f.hidden,false),COALESCE(f.likes,0),COALESCE(f.dislikes,0),
	COALESCE(cr.precision,'unknown'),cr.release_date,cr.raw_text
FROM catalog_entries ce JOIN games g ON g.id=ce.game_id
LEFT JOIN game_embeddings e ON e.game_id=g.id AND e.model=$2
LEFT JOIN wishlist_entries w ON w.user_id=$1 AND w.game_id=g.id
LEFT JOIN user_games ug ON ug.user_id=$1 AND ug.game_id=g.id
LEFT JOIN feedback f ON f.game_id=g.id LEFT JOIN current_release cr ON cr.game_id=g.id
WHERE %s ORDER BY %s LIMIT $3`

func (r *SQLRepository) Candidates(ctx context.Context, userID uuid.UUID, p profile.Profile, limit int) ([]Candidate, error) {
	if limit <= 0 {
		return nil, errors.New("candidate limit must be positive")
	}
	byID := map[uuid.UUID]Candidate{}
	semantic := hasVector(p.Positive)
	base := "ug.game_id IS NULL AND NOT COALESCE(f.hidden,false) AND COALESCE(f.dislikes,0)<2 AND NOT (COALESCE(cr.precision,'unknown')='day' AND cr.release_date<CURRENT_DATE)"
	condition, order := base, "lower(g.name),g.id"
	args := []any{userID, r.embeddingModel, 1000000}
	if semantic {
		condition = "e.embedding IS NOT NULL AND " + base
		order = "e.embedding <=> $4,lower(g.name),g.id"
		args = append(args, pgvector.NewVector(p.Positive))
	}
	if err := r.readCandidates(ctx, userID, fmt.Sprintf(candidateSelect, condition, order), args, p, byID); err != nil {
		return nil, err
	}
	// Wishlist entries are mandatory candidates even if they have no embedding
	// or fall outside the nearest-neighbor limit.
	if err := r.readCandidates(ctx, userID, fmt.Sprintf(candidateSelect, "w.game_id IS NOT NULL AND "+base, "lower(g.name),g.id"), []any{userID, r.embeddingModel, 1000000}, p, byID); err != nil {
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
		if Eligible(candidate) && (candidate.Factors.Wishlist == 1 || withinHorizon(candidate.Release, r.now().UTC(), 90)) {
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

func (r *SQLRepository) readCandidates(ctx context.Context, userID uuid.UUID, query string, args []any, p profile.Profile, dst map[uuid.UUID]Candidate) error {
	rows, err := r.store.Pool.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	rank := 0
	for rows.Next() {
		rank++
		var c Candidate
		var vector *pgvector.Vector
		var wishlist bool
		var likes int
		var releaseDate pgtype.Date
		var raw pgtype.Text
		if err := rows.Scan(&c.GameID, &c.Name, &vector, &wishlist, &c.Owned, &c.Released, &c.ExplicitlyHidden, &likes, &c.Dislikes, &c.Release.Precision, &releaseDate, &raw); err != nil {
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
		c.Released = c.Released || releaseWindowEnded(c.Release, r.now().UTC())
		quality := .25
		if c.Release.Precision != "unknown" {
			quality += .25
		}
		if vector != nil {
			slice := vector.Slice()
			c.Factors.PositiveSimilarity = cosine01(profile.Cosine(p.Positive, slice))
			c.Factors.NegativeSimilarity = cosine01(profile.Cosine(p.Negative, slice))
			quality += .5
		}
		tag, play, achievement, recent, err := r.affinities(ctx, userID, c.GameID)
		if err != nil {
			return err
		}
		c.Factors.TagAffinity, c.Factors.PlaytimeAffinity, c.Factors.AchievementAffinity, c.Factors.RecentActivityAffinity = tag, play, achievement, recent
		c.Factors.Confidence = quality
		c.retrievalRank = rank
		if existing, ok := dst[c.GameID]; ok && existing.retrievalRank < c.retrievalRank {
			c.retrievalRank = existing.retrievalRank
		}
		dst[c.GameID] = c
	}
	return rows.Err()
}

func (r *SQLRepository) affinities(ctx context.Context, userID, gameID uuid.UUID) (float64, float64, float64, float64, error) {
	var candidate []byte
	if err := r.store.Pool.QueryRow(ctx, `SELECT features FROM game_features WHERE game_id=$1 AND model=$2`, gameID, r.textModel).Scan(&candidate); errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, 0, nil
	} else if err != nil {
		return 0, 0, 0, 0, err
	}
	target := featureSet(candidate)
	if len(target) == 0 {
		return 0, 0, 0, 0, nil
	}
	rows, err := r.store.Pool.Query(ctx, `SELECT gf.features,ug.playtime_minutes,ug.recently_played_minutes,COALESCE(a.achieved,0),COALESCE(a.total,0) FROM user_games ug JOIN game_features gf ON gf.game_id=ug.game_id AND gf.model=$2 LEFT JOIN achievement_summaries a ON a.user_id=ug.user_id AND a.game_id=ug.game_id WHERE ug.user_id=$1`, userID, r.textModel)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()
	var tag, play, ach, recent, playDen, achDen, recentDen float64
	for rows.Next() {
		var raw []byte
		var minutes, recentMinutes int64
		var got, total int
		if err := rows.Scan(&raw, &minutes, &recentMinutes, &got, &total); err != nil {
			return 0, 0, 0, 0, err
		}
		overlap := setOverlap(target, featureSet(raw))
		if overlap > tag {
			tag = overlap
		}
		pw := math.Log1p(float64(minutes))
		play += overlap * pw
		playDen += pw
		rw := float64(recentMinutes)
		recent += overlap * rw
		recentDen += rw
		if total > 0 {
			aw := float64(got) / float64(total)
			ach += overlap * aw
			achDen += aw
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, err
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
	return tag, play, ach, recent, nil
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

func (r *SQLRepository) SaveRanking(ctx context.Context, userID uuid.UUID, p profile.Profile, input []Recommendation, buildKey string) ([]Recommendation, int64, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `ranking:`+userID.String()); err != nil {
		return nil, 0, err
	}
	var rankingVersion int64
	var published bool
	err = tx.QueryRow(ctx, `SELECT ranking_version,published FROM recommendation_runs WHERE user_id=$1 AND build_key=$2`, userID, buildKey).Scan(&rankingVersion, &published)
	replay := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, err
	}
	if replay && published {
		if err = tx.Commit(ctx); err != nil {
			return nil, 0, err
		}
		return nil, rankingVersion, nil
	}
	if replay {
		if _, err = tx.Exec(ctx, `DELETE FROM recommendations WHERE user_id=$1 AND ranking_version=$2`, userID, rankingVersion); err != nil {
			return nil, 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE recommendation_runs SET published=false,profile_id=$3 WHERE user_id=$1 AND ranking_version=$2`, userID, rankingVersion, p.ID); err != nil {
			return nil, 0, err
		}
	} else if err = tx.QueryRow(ctx, `SELECT COALESCE(max(ranking_version),0)+1 FROM recommendation_runs WHERE user_id=$1`, userID).Scan(&rankingVersion); err != nil {
		return nil, 0, err
	}
	result := make([]Recommendation, 0, len(input))
	now := r.now().UTC()
	if !replay {
		if _, err = tx.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,created_at)VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), userID, p.ID, rankingVersion, AlgorithmVersion, buildKey, now); err != nil {
			return nil, 0, err
		}
	}
	for _, item := range input {
		id := uuid.New()
		item.RankingVersion = rankingVersion
		err = tx.QueryRow(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score,text_model,embedding_model,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id,created_at`, id, userID, item.GameID, p.ID, item.AlgorithmVersion, rankingVersion, item.Score, nullableText(r.textModel), r.embeddingModel, now).Scan(&item.ID, &item.CreatedAt)
		if err != nil {
			return nil, 0, err
		}
		factors, _ := json.Marshal(item.Factors)
		weights, _ := json.Marshal(item.Weights)
		if _, err = tx.Exec(ctx, `INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3) ON CONFLICT(recommendation_id) DO UPDATE SET factors=EXCLUDED.factors,weights=EXCLUDED.weights`, item.ID, factors, weights); err != nil {
			return nil, 0, err
		}
		result = append(result, item)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return result, rankingVersion, nil
}

func (r *SQLRepository) SaveExplanation(ctx context.Context, id uuid.UUID, gameName, text string, generated bool, supplied, cited []Evidence) error {
	encoded, _ := json.Marshal(cited)
	input, _ := json.Marshal(map[string]any{"model": r.textModel, "schema": "recommendation-explanation-v1", "game": gameName, "evidence": supplied})
	hash := fmt.Sprintf("%x", sha256.Sum256(input))
	command, err := r.store.Pool.Exec(ctx, `UPDATE recommendations SET explanation=$2,explanation_input_content_hash=$3,explanation_evidence=$4,explanation_generated=$5 WHERE id=$1`, id, text, hash, encoded, generated)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
func (r *SQLRepository) PublishRanking(ctx context.Context, userID uuid.UUID, version int64) error {
	command, err := r.store.Pool.Exec(ctx, `UPDATE recommendation_runs SET published=true WHERE user_id=$1 AND ranking_version=$2`, userID, version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *SQLRepository) Latest(ctx context.Context, userID uuid.UUID) ([]Recommendation, error) {
	rows, err := r.store.Pool.Query(ctx, `SELECT r.id,r.game_id,g.name,p.version,r.ranking_version,r.algorithm_version,r.score,r.explanation,r.explanation_evidence,r.explanation_generated,r.created_at,rf.factors,rf.weights,
	COALESCE(rd.precision,'unknown'),rd.release_date,rd.raw_text
	FROM recommendations r JOIN games g ON g.id=r.game_id JOIN preference_profiles p ON p.id=r.profile_id JOIN recommendation_factors rf ON rf.recommendation_id=r.id
	LEFT JOIN LATERAL(SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1)rd ON true
	WHERE r.user_id=$1 AND r.ranking_version=(SELECT max(ranking_version) FROM recommendation_runs WHERE user_id=$1 AND published)
	ORDER BY r.score DESC,lower(g.name),g.id`, userID)
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
	row := r.store.Pool.QueryRow(ctx, `SELECT r.id,r.game_id,g.name,p.version,r.ranking_version,r.algorithm_version,r.score,r.explanation,r.explanation_evidence,r.explanation_generated,r.created_at,rf.factors,rf.weights,
	COALESCE(rd.precision,'unknown'),rd.release_date,rd.raw_text FROM recommendations r JOIN games g ON g.id=r.game_id JOIN preference_profiles p ON p.id=r.profile_id JOIN recommendation_factors rf ON rf.recommendation_id=r.id
	LEFT JOIN LATERAL(SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1)rd ON true WHERE r.id=$1 AND r.user_id=$2 AND EXISTS(SELECT 1 FROM recommendation_runs run WHERE run.user_id=r.user_id AND run.ranking_version=r.ranking_version AND run.published)`, id, userID)
	return scanRecommendation(row)
}

type scanner interface{ Scan(...any) error }

func scanRecommendation(row scanner) (Recommendation, error) {
	var r Recommendation
	var explanation pgtype.Text
	var evidence []byte
	var generated bool
	var factors, weights []byte
	var releaseDate pgtype.Date
	var raw pgtype.Text
	if err := row.Scan(&r.ID, &r.GameID, &r.Name, &r.ProfileVersion, &r.RankingVersion, &r.AlgorithmVersion, &r.Score, &explanation, &evidence, &generated, &r.CreatedAt, &factors, &weights, &r.Release.Precision, &releaseDate, &raw); err != nil {
		return Recommendation{}, err
	}
	_ = json.Unmarshal(factors, &r.Factors)
	_ = json.Unmarshal(weights, &r.Weights)
	r.Confidence = r.Factors.Confidence
	if explanation.Valid {
		r.Explanation = explanation.String
	}
	r.GeneratedExplanation = generated
	_ = json.Unmarshal(evidence, &r.Evidence)
	if releaseDate.Valid && r.Release.Precision == "day" {
		r.Release.ExactDate = releaseDate.Time.Format(time.DateOnly)
	}
	if raw.Valid {
		r.Release.Raw = raw.String
	}
	return r, nil
}
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var _ Repository = (*SQLRepository)(nil)
