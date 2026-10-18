package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type SQLRepository struct{ store *store.Store }

type profileEvidenceSignal struct {
	id                     uuid.UUID
	name                   string
	played, recent         int64
	lastPlayedAt           pgtype.Timestamptz
	wishlisted             bool
	feedbackAction         string
	achieved, achievements int32
	features               []byte
	completionBasis        string
	endingCount            int32
	completionAt           pgtype.Timestamptz
	completionOverridden   bool
	completionWeight       float64
}

func NewSQLRepository(database *store.Store) *SQLRepository { return &SQLRepository{store: database} }

func (repo *SQLRepository) ListGameIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := repo.store.Pool.Query(ctx, `SELECT DISTINCT g.id FROM games g
		LEFT JOIN catalog_entries ce ON ce.game_id=g.id
		LEFT JOIN user_games ug ON ug.game_id=g.id AND ug.user_id=$1
		LEFT JOIN wishlist_entries w ON w.game_id=g.id AND w.user_id=$1
		WHERE ce.game_id IS NOT NULL OR ug.game_id IS NOT NULL OR w.game_id IS NOT NULL ORDER BY g.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (repo *SQLRepository) LoadGameEvidence(ctx context.Context, id uuid.UUID, model string) (StoredGameEvidence, error) {
	if repo == nil || repo.store == nil {
		return StoredGameEvidence{}, errors.New("AI store is required")
	}
	var name string
	var snapshot []byte
	if err := repo.store.Pool.QueryRow(ctx, `SELECT name, source_snapshot FROM games WHERE id=$1`, id).Scan(&name, &snapshot); err != nil {
		return StoredGameEvidence{}, err
	}
	result := gameEvidenceFromSnapshot(name, snapshot)
	feature, err := repo.store.Queries.GetLatestGameFeature(ctx, dbgen.GetLatestGameFeatureParams{GameID: uuidPG(id), Model: model})
	if err == nil {
		result.StoredFeatures = feature.Features
		result.FeatureInputHash = feature.InputContentHash
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return StoredGameEvidence{}, err
	}
	return result, nil
}

func (repo *SQLRepository) HasGameFeatures(ctx context.Context, id uuid.UUID, model, hash string) (bool, error) {
	_, err := repo.store.Queries.GetGameFeatureByHash(ctx, dbgen.GetGameFeatureByHashParams{GameID: uuidPG(id), Model: model, InputContentHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func (repo *SQLRepository) SaveGameFeatures(ctx context.Context, id uuid.UUID, model, hash string, value GameFeatures) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return repo.store.Queries.UpsertGameFeature(ctx, dbgen.UpsertGameFeatureParams{GameID: uuidPG(id), Model: model, InputContentHash: hash, Features: encoded})
}
func (repo *SQLRepository) HasGameEmbedding(ctx context.Context, id uuid.UUID, model, hash string) (bool, error) {
	_, err := repo.store.Queries.GetGameEmbeddingByHash(ctx, dbgen.GetGameEmbeddingByHashParams{GameID: uuidPG(id), Model: model, InputContentHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func (repo *SQLRepository) SaveGameEmbedding(ctx context.Context, id uuid.UUID, model, hash string, vector []float32) error {
	return repo.store.PutGameEmbedding(ctx, store.GameEmbedding{GameID: id, Model: model, ContentHash: hash, Vector: vector})
}

func (repo *SQLRepository) LoadProfileEvidence(ctx context.Context, profileID uuid.UUID) (ProfileEvidence, error) {
	var userID uuid.UUID
	if err := repo.store.Pool.QueryRow(ctx, `SELECT user_id FROM preference_profiles WHERE id=$1`, profileID).Scan(&userID); err != nil {
		return ProfileEvidence{}, err
	}
	rows, err := repo.store.Pool.Query(ctx, `WITH feedback AS (
		SELECT DISTINCT ON (rec.game_id) rec.game_id,f.action
		FROM feedback_events f JOIN recommendations rec ON rec.id=f.recommendation_id
		WHERE f.user_id=$1 AND f.action IN ('like','dislike','exclude')
		ORDER BY rec.game_id,f.created_at DESC,f.id DESC
	), relevant AS (
		SELECT game_id FROM user_games WHERE user_id=$1 UNION
		SELECT game_id FROM wishlist_entries WHERE user_id=$1 UNION
		SELECT game_id FROM feedback UNION
		SELECT game_id FROM user_game_completion_projections WHERE user_id=$1)
		SELECT g.id,g.name,COALESCE(ug.playtime_minutes,0),COALESCE(ug.recently_played_minutes,0),ug.last_played_at,
		(w.game_id IS NOT NULL),COALESCE(f.action,''),COALESCE(a.achieved,0),COALESCE(a.total,0),COALESCE(gf.features,'{}'::jsonb),
		COALESCE(cp.basis,''),COALESCE(cp.ending_count,0),cp.completion_at,
		(cp.game_id IS NOT NULL AND COALESCE(f.action,'') IN ('dislike','exclude')),COALESCE(cp.weight,0)
		FROM relevant r JOIN games g ON g.id=r.game_id
		LEFT JOIN user_games ug ON ug.user_id=$1 AND ug.game_id=r.game_id
		LEFT JOIN wishlist_entries w ON w.user_id=$1 AND w.game_id=r.game_id
		LEFT JOIN feedback f ON f.game_id=r.game_id
		LEFT JOIN achievement_summaries a ON a.user_id=$1 AND a.game_id=r.game_id
		LEFT JOIN user_game_completion_projections cp ON cp.user_id=$1 AND cp.game_id=r.game_id
		LEFT JOIN LATERAL (SELECT features FROM game_features WHERE game_id=g.id ORDER BY last_successful_fetch_at DESC LIMIT 1) gf ON true
		`, userID)
	if err != nil {
		return ProfileEvidence{}, err
	}
	defer rows.Close()
	var signals []profileEvidenceSignal
	for rows.Next() {
		var value profileEvidenceSignal
		if err := rows.Scan(
			&value.id, &value.name, &value.played, &value.recent, &value.lastPlayedAt,
			&value.wishlisted, &value.feedbackAction, &value.achieved, &value.achievements, &value.features,
			&value.completionBasis, &value.endingCount, &value.completionAt,
			&value.completionOverridden, &value.completionWeight,
		); err != nil {
			return ProfileEvidence{}, err
		}
		signals = append(signals, value)
	}
	if err := rows.Err(); err != nil {
		return ProfileEvidence{}, err
	}
	signals = selectProfileEvidence(signals, 100)
	evidence := make([]EvidenceItem, 0, len(signals))
	for index, value := range signals {
		lastPlayedAt := "unknown"
		if value.lastPlayedAt.Valid {
			lastPlayedAt = value.lastPlayedAt.Time.UTC().Format(time.RFC3339)
		}
		completionAt := "unknown"
		if value.completionAt.Valid {
			completionAt = value.completionAt.Time.UTC().Format(time.RFC3339)
		}
		completionBasis := value.completionBasis
		if completionBasis == "" {
			completionBasis = "none"
		}
		directFeedback := value.feedbackAction
		if directFeedback == "" {
			directFeedback = "none"
		}
		positiveFeedback := value.feedbackAction == "like" && !value.completionOverridden
		fact := fmt.Sprintf("%s; playtime_minutes=%d; recent_minutes=%d; last_played_at=%s; wishlisted=%t; direct_feedback=%s; positive_feedback=%t; achievements=%d/%d; completion_basis=%s; ending_count=%d; completion_at=%s; completion_overridden=%t; features=%s", value.name, value.played, value.recent, lastPlayedAt, value.wishlisted, directFeedback, positiveFeedback, value.achieved, value.achievements, completionBasis, value.endingCount, completionAt, value.completionOverridden, string(value.features))
		evidence = append(evidence, EvidenceItem{Key: fmt.Sprintf("signal-%03d", index+1), Value: fact})
	}
	if len(evidence) == 0 {
		return ProfileEvidence{}, fmt.Errorf("%w: profile has no evidence", ErrInvalidInput)
	}
	return ProfileEvidence{EvidenceKey: "profile-evidence", Evidence: evidence}, nil
}

func selectProfileEvidence(input []profileEvidenceSignal, limit int) []profileEvidenceSignal {
	if limit <= 0 || len(input) == 0 {
		return []profileEvidenceSignal{}
	}
	signals := append([]profileEvidenceSignal(nil), input...)
	sort.Slice(signals, func(i, j int) bool {
		left, right := signals[i], signals[j]
		leftTier, rightTier := profileEvidenceTier(left), profileEvidenceTier(right)
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		leftValue, rightValue := profileEvidenceMaterialValue(left), profileEvidenceMaterialValue(right)
		if leftValue != rightValue {
			return leftValue > rightValue
		}
		if left.lastPlayedAt.Valid != right.lastPlayedAt.Valid {
			return left.lastPlayedAt.Valid
		}
		if left.lastPlayedAt.Valid && !left.lastPlayedAt.Time.Equal(right.lastPlayedAt.Time) {
			return left.lastPlayedAt.Time.After(right.lastPlayedAt.Time)
		}
		leftNormalized, rightNormalized := normalizeProfileEvidenceName(left.name), normalizeProfileEvidenceName(right.name)
		if leftNormalized != rightNormalized {
			return leftNormalized < rightNormalized
		}
		if left.name != right.name {
			return left.name < right.name
		}
		return left.id.String() < right.id.String()
	})

	selected := make([]profileEvidenceSignal, 0, min(limit, len(signals)))
	seen := make(map[uuid.UUID]struct{}, len(signals))
	for _, signal := range signals {
		if _, found := seen[signal.id]; found {
			continue
		}
		seen[signal.id] = struct{}{}
		selected = append(selected, signal)
		if len(selected) == limit {
			break
		}
	}
	return selected
}

func profileEvidenceTier(signal profileEvidenceSignal) int {
	switch {
	case signal.completionBasis != "":
		return 0
	case signal.recent > 0:
		return 1
	case signal.feedbackAction != "":
		return 2
	case signal.wishlisted:
		return 3
	default:
		return 4
	}
}

func profileEvidenceMaterialValue(signal profileEvidenceSignal) float64 {
	switch profileEvidenceTier(signal) {
	case 0:
		return signal.completionWeight
	case 1:
		return float64(signal.recent)
	case 2:
		if signal.feedbackAction == "like" {
			return 1
		}
		return 0
	case 4:
		return float64(signal.played)
	default:
		return 0
	}
}

func normalizeProfileEvidenceName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func (repo *SQLRepository) HasProfileSummary(ctx context.Context, id uuid.UUID, model, hash string) (bool, error) {
	_, err := repo.store.Queries.GetProfileSummaryByHash(ctx, dbgen.GetProfileSummaryByHashParams{ID: uuidPG(id), Model: pgtype.Text{String: model, Valid: true}, InputContentHash: pgtype.Text{String: hash, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func (repo *SQLRepository) SaveProfileSummary(ctx context.Context, id uuid.UUID, model, hash string, value ProfileSummary) error {
	rows, err := repo.store.Queries.UpdateProfileSummary(ctx, dbgen.UpdateProfileSummaryParams{ID: uuidPG(id), Summary: pgtype.Text{String: value.Summary, Valid: true}, Model: pgtype.Text{String: model, Valid: true}, InputContentHash: pgtype.Text{String: hash, Valid: true}})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("profile no longer exists")
	}
	return nil
}

func uuidPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }

var _ Repository = (*SQLRepository)(nil)
