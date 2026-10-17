package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"
	"next-game-crush/internal/store"
)

type SQLRepository struct {
	store          *store.Store
	embeddingModel string
	now            func() time.Time
}

func NewSQLRepository(database *store.Store, embeddingModel string) (*SQLRepository, error) {
	if database == nil || embeddingModel == "" {
		return nil, errors.New("profile store and embedding model are required")
	}
	return &SQLRepository{store: database, embeddingModel: embeddingModel, now: time.Now}, nil
}

func (r *SQLRepository) LoadSignals(ctx context.Context, userID uuid.UUID) ([]Signal, error) {
	rows, err := r.store.Pool.Query(ctx, `WITH relevant AS (
		SELECT game_id FROM user_games WHERE user_id=$1 UNION
		SELECT game_id FROM wishlist_entries WHERE user_id=$1 UNION
		SELECT rec.game_id FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id WHERE f.user_id=$1
	), feedback AS (
		SELECT rec.game_id,
			(count(*) FILTER (WHERE f.action='like') + count(*) FILTER (WHERE fc.actionable AND fc.polarity='positive'))::int likes,
			(count(*) FILTER (WHERE f.action IN ('dislike','exclude')) + count(*) FILTER (WHERE fc.actionable AND fc.polarity='negative'))::int dislikes
		FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id
		LEFT JOIN feedback_classifications fc ON fc.feedback_id=f.id
		WHERE f.user_id=$1 GROUP BY rec.game_id
	)
	SELECT rel.game_id,e.embedding,COALESCE(ug.playtime_minutes,0),COALESCE(ug.recently_played_minutes,0),
		CASE WHEN COALESCE(a.total,0)>0 THEN a.achieved::float8/a.total ELSE 0 END,
		(w.game_id IS NOT NULL),COALESCE(f.likes,0),COALESCE(f.dislikes,0)
	FROM relevant rel JOIN game_embeddings e ON e.game_id=rel.game_id AND e.model=$2
	LEFT JOIN user_games ug ON ug.user_id=$1 AND ug.game_id=rel.game_id
	LEFT JOIN wishlist_entries w ON w.user_id=$1 AND w.game_id=rel.game_id
	LEFT JOIN achievement_summaries a ON a.user_id=$1 AND a.game_id=rel.game_id
	LEFT JOIN feedback f ON f.game_id=rel.game_id ORDER BY rel.game_id`, userID, r.embeddingModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Signal{}
	for rows.Next() {
		var s Signal
		var vector pgvector.Vector
		if err := rows.Scan(&s.GameID, &vector, &s.PlaytimeMinutes, &s.RecentMinutes, &s.AchievementRatio, &s.Wishlisted, &s.Likes, &s.Dislikes); err != nil {
			return nil, err
		}
		s.Vector = append([]float32(nil), vector.Slice()...)
		result = append(result, s)
	}
	return result, rows.Err()
}

func (r *SQLRepository) SaveProfile(ctx context.Context, userID uuid.UUID, vectors Vectors, algorithm, buildKey string) (Profile, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `profile:`+userID.String()); err != nil {
		return Profile{}, err
	}
	if buildKey != "" {
		var existingVersion int64
		if err = tx.QueryRow(ctx, `SELECT version FROM preference_profiles WHERE user_id=$1 AND build_key=$2`, userID, buildKey).Scan(&existingVersion); err == nil {
			if err = tx.Commit(ctx); err != nil {
				return Profile{}, err
			}
			return r.profileVersion(ctx, userID, existingVersion)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Profile{}, err
		}
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM preference_profiles WHERE user_id=$1`, userID).Scan(&version); err != nil {
		return Profile{}, err
	}
	id, created := uuid.New(), r.now().UTC()
	metadata, _ := json.Marshal(map[string]any{"algorithmVersion": algorithm, "confidence": vectors.Confidence, "evidenceCount": vectors.Evidence})
	if _, err = tx.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata,created_at,build_key) VALUES($1,$2,$3,$4,$5,$6)`, id, userID, version, metadata, created, nullableBuildKey(buildKey)); err != nil {
		return Profile{}, err
	}
	hash := fmt.Sprintf("%s:%d", algorithm, version)
	for polarity, vector := range map[string][]float32{"negative": vectors.Negative, "positive": vectors.Positive} {
		if len(vector) != Dimensions {
			return Profile{}, fmt.Errorf("%s profile vector must have %d dimensions", polarity, Dimensions)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO profile_embeddings(profile_id,polarity,model,input_content_hash,embedding,source,last_successful_fetch_at) VALUES($1,$2,$3,$4,$5,'deterministic',$6)`, id, polarity, r.embeddingModel, hash, pgvector.NewVector(vector), created); err != nil {
			return Profile{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Profile{}, err
	}
	return Profile{ID: id, UserID: userID, Version: version, Confidence: vectors.Confidence, EvidenceCount: vectors.Evidence, Positive: vectors.Positive, Negative: vectors.Negative, CreatedAt: created}, nil
}

func (r *SQLRepository) LatestProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return r.profileVersion(ctx, userID, 0)
}
func (r *SQLRepository) profileVersion(ctx context.Context, userID uuid.UUID, version int64) (Profile, error) {
	var p Profile
	var summary *string
	var metadata []byte
	query := `SELECT id,user_id,version,summary,algorithm_metadata,created_at FROM preference_profiles WHERE user_id=$1 ORDER BY version DESC LIMIT 1`
	args := []any{userID}
	if version > 0 {
		query = `SELECT id,user_id,version,summary,algorithm_metadata,created_at FROM preference_profiles WHERE user_id=$1 AND version=$2`
		args = append(args, version)
	}
	err := r.store.Pool.QueryRow(ctx, query, args...).Scan(&p.ID, &p.UserID, &p.Version, &summary, &metadata, &p.CreatedAt)
	if err != nil {
		return Profile{}, err
	}
	if summary != nil {
		p.Summary = *summary
	}
	if p.Summary == "" {
		p.Summary = "AI profile summary is unavailable; recommendations use saved deterministic signals."
	}
	var values struct {
		Confidence    float64 `json:"confidence"`
		EvidenceCount int     `json:"evidenceCount"`
	}
	_ = json.Unmarshal(metadata, &values)
	p.Confidence, p.EvidenceCount = finite01(values.Confidence), values.EvidenceCount
	rows, err := r.store.Pool.Query(ctx, `SELECT polarity,embedding FROM profile_embeddings WHERE profile_id=$1 AND model=$2`, p.ID, r.embeddingModel)
	if err != nil {
		return Profile{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var polarity string
		var vector pgvector.Vector
		if err := rows.Scan(&polarity, &vector); err != nil {
			return Profile{}, err
		}
		switch polarity {
		case "positive":
			p.Positive = append([]float32(nil), vector.Slice()...)
		case "negative":
			p.Negative = append([]float32(nil), vector.Slice()...)
		}
	}
	if err := rows.Err(); err != nil {
		return Profile{}, err
	}
	if len(p.Positive) == 0 {
		p.Positive = make([]float32, Dimensions)
	}
	if len(p.Negative) == 0 {
		p.Negative = make([]float32, Dimensions)
	}
	return p, nil
}
func finite01(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
func nullableBuildKey(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var _ Repository = (*SQLRepository)(nil)
