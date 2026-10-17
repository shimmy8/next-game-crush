package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type SQLRepository struct{ store *store.Store }

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
	result := StoredGameEvidence{Name: name}
	var details steam.GameDetails
	if len(snapshot) > 0 && json.Unmarshal(snapshot, &details) == nil {
		result.Description = details.ShortDescription
		for _, genre := range details.Genres {
			result.Genres = append(result.Genres, genre.Description)
		}
		for _, category := range details.Categories {
			result.Categories = append(result.Categories, category.Description)
		}
	}
	feature, err := repo.store.Queries.GetLatestGameFeature(ctx, dbgen.GetLatestGameFeatureParams{GameID: uuidPG(id), Model: model})
	if err == nil {
		result.StoredFeatures = feature.Features
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
	rows, err := repo.store.Pool.Query(ctx, `WITH relevant AS (
		SELECT game_id FROM user_games WHERE user_id=$1 UNION SELECT game_id FROM wishlist_entries WHERE user_id=$1)
		SELECT g.name, COALESCE(ug.playtime_minutes,0), COALESCE(ug.recently_played_minutes,0), (w.game_id IS NOT NULL),
		COALESCE(a.achieved,0), COALESCE(a.total,0), COALESCE(gf.features, '{}'::jsonb)
		FROM relevant r JOIN games g ON g.id=r.game_id
		LEFT JOIN user_games ug ON ug.user_id=$1 AND ug.game_id=r.game_id
		LEFT JOIN wishlist_entries w ON w.user_id=$1 AND w.game_id=r.game_id
		LEFT JOIN achievement_summaries a ON a.user_id=$1 AND a.game_id=r.game_id
		LEFT JOIN LATERAL (SELECT features FROM game_features WHERE game_id=g.id ORDER BY last_successful_fetch_at DESC LIMIT 1) gf ON true
		ORDER BY COALESCE(ug.playtime_minutes,0) DESC, g.name LIMIT 100`, userID)
	if err != nil {
		return ProfileEvidence{}, err
	}
	defer rows.Close()
	type signal struct {
		name                   string
		played, recent         int64
		wishlisted             bool
		achieved, achievements int32
		features               []byte
	}
	var signals []signal
	for rows.Next() {
		var value signal
		if err := rows.Scan(&value.name, &value.played, &value.recent, &value.wishlisted, &value.achieved, &value.achievements, &value.features); err != nil {
			return ProfileEvidence{}, err
		}
		signals = append(signals, value)
	}
	if err := rows.Err(); err != nil {
		return ProfileEvidence{}, err
	}
	sort.SliceStable(signals, func(i, j int) bool { return signals[i].played > signals[j].played })
	evidence := make([]EvidenceItem, 0, len(signals))
	for index, value := range signals {
		fact := fmt.Sprintf("%s; playtime_minutes=%d; recent_minutes=%d; wishlisted=%t; achievements=%d/%d; features=%s", value.name, value.played, value.recent, value.wishlisted, value.achieved, value.achievements, string(value.features))
		evidence = append(evidence, EvidenceItem{Key: "signal-" + strconv.Itoa(index+1), Value: fact})
	}
	if len(evidence) == 0 {
		return ProfileEvidence{}, fmt.Errorf("%w: profile has no evidence", ErrInvalidInput)
	}
	return ProfileEvidence{EvidenceKey: "profile-evidence", Evidence: evidence}, nil
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
