package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type SQLRepository struct{ store *store.Store }

type profileEvidenceSignal struct {
	id                     uuid.UUID
	name                   string
	played, recent         int64
	lastPlayedAt           pgtype.Timestamptz
	achieved, achievements int32
	features               []byte
	completionBasis        string
	endingCount            int32
	completionAt           pgtype.Timestamptz
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
	rows, err := repo.store.Pool.Query(ctx, `SELECT g.id,g.name,e.playtime_minutes,e.recently_played_minutes,e.last_played_at,
		e.achieved,e.total,COALESCE(gf.features,'{}'::jsonb),COALESCE(e.completion_basis,''),
		e.completion_ending_count,e.completion_at,e.completion_weight
		FROM activity_profile_evidence e
		JOIN games g ON g.id=e.game_id
		LEFT JOIN LATERAL (SELECT features FROM game_features WHERE game_id=g.id ORDER BY last_successful_fetch_at DESC LIMIT 1) gf ON true
		WHERE e.profile_id=$1
		ORDER BY e.game_id`, profileID)
	if err != nil {
		return ProfileEvidence{}, err
	}
	defer rows.Close()
	var signals []profileEvidenceSignal
	for rows.Next() {
		var value profileEvidenceSignal
		if err := rows.Scan(
			&value.id, &value.name, &value.played, &value.recent, &value.lastPlayedAt,
			&value.achieved, &value.achievements, &value.features,
			&value.completionBasis, &value.endingCount, &value.completionAt, &value.completionWeight,
		); err != nil {
			return ProfileEvidence{}, err
		}
		signals = append(signals, value)
	}
	if err := rows.Err(); err != nil {
		return ProfileEvidence{}, err
	}
	facts := ActivityFacts{}
	for _, value := range signals {
		if value.played > 0 {
			facts.PlayedGames++
		}
		if value.recent > 0 {
			facts.RecentlyPlayedGames++
		}
		if value.achieved > 0 {
			facts.GamesWithAchievements++
		}
		if value.completionWeight > 0 {
			facts.CompletedGames++
		}
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
		fact := fmt.Sprintf("%s; playtime_minutes=%d; recent_minutes=%d; last_played_at=%s; achievements=%d/%d; completion_basis=%s; ending_count=%d; completion_at=%s; features=%s", value.name, value.played, value.recent, lastPlayedAt, value.achieved, value.achievements, completionBasis, value.endingCount, completionAt, string(value.features))
		evidence = append(evidence, EvidenceItem{Key: fmt.Sprintf("signal-%03d", index+1), Value: fact})
	}
	if len(evidence) == 0 {
		return ProfileEvidence{}, fmt.Errorf("%w: profile has no evidence", ErrInvalidInput)
	}
	return ProfileEvidence{EvidenceKey: "profile-evidence", Evidence: evidence, Facts: facts}, nil
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
	case signal.completionWeight > 0:
		return 0
	case signal.recent > 0:
		return 1
	case signal.achieved > 0:
		return 2
	default:
		return 3
	}
}

func profileEvidenceMaterialValue(signal profileEvidenceSignal) float64 {
	switch profileEvidenceTier(signal) {
	case 0:
		return signal.completionWeight
	case 1:
		return float64(signal.recent)
	case 2:
		return float64(signal.achieved)
	case 3:
		return float64(signal.played)
	default:
		return 0
	}
}

func normalizeProfileEvidenceName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func (repo *SQLRepository) LoadCachedProfileSummary(ctx context.Context, id uuid.UUID, model, hash string) (ProfileSummary, bool, error) {
	row, err := repo.store.Queries.GetProfileSummaryByHash(ctx, dbgen.GetProfileSummaryByHashParams{ID: uuidPG(id), Model: pgtype.Text{String: model, Valid: true}, InputContentHash: pgtype.Text{String: hash, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfileSummary{}, false, nil
	}
	if err != nil {
		return ProfileSummary{}, false, err
	}
	if !row.Valid || !validPublishedSummary(row.String) {
		return ProfileSummary{}, false, nil
	}
	return ProfileSummary{Summary: row.String}, true, nil
}

func (repo *SQLRepository) PublishProfileSummary(ctx context.Context, lease jobs.Lease, id uuid.UUID, model, hash string, value ProfileSummary, source string) error {
	if !validPublishedSummary(value.Summary) || (source != "openai" && source != "deterministic") {
		return ErrInvalidOutput
	}
	tx, err := repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	userID, err := lockOwnedSummaryJobAndProfile(ctx, tx, lease, id)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE preference_profiles SET summary=$1,model=$2,input_content_hash=$3,
		summary_source=$4,publication_state='published',published_at=now()
		WHERE id=$5 AND user_id=$6 AND publication_state='building'
		AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$6)
		AND EXISTS(SELECT 1 FROM sync_jobs j WHERE j.id=$7 AND j.user_id=$6 AND j.kind=$8
			AND j.status='running' AND j.worker_id=$9 AND j.fencing_token=$10 AND j.lease_until>clock_timestamp())`,
		value.Summary, model, hash, source, id, userID, lease.ID, ProfileSummaryJobKind, lease.WorkerID, lease.FencingToken)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		var exact bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM preference_profiles p JOIN sync_jobs j ON j.id=$7
			WHERE p.id=$1 AND p.user_id=$2 AND j.user_id=p.user_id AND j.kind=$8 AND j.status='running'
			AND j.worker_id=$9 AND j.fencing_token=$10 AND j.lease_until>clock_timestamp()
			AND p.publication_state='published' AND p.summary=$3 AND p.model=$4 AND p.input_content_hash=$5 AND p.summary_source=$6)`,
			id, userID, value.Summary, model, hash, source, lease.ID, ProfileSummaryJobKind, lease.WorkerID, lease.FencingToken).Scan(&exact); err != nil {
			return err
		}
		if !exact {
			return jobs.ErrLeaseLost
		}
	}
	return tx.Commit(ctx)
}

func (repo *SQLRepository) HandleTerminalProfileSummaryFailure(ctx context.Context, lease jobs.Lease, id uuid.UUID, model string) (bool, error) {
	tx, err := repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	userID, err := lockOwnedSummaryJobAndProfile(ctx, tx, lease, id)
	if err != nil {
		return false, err
	}
	var lockedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM preference_profiles WHERE id=$1 AND user_id=$2 AND publication_state='building' FOR UPDATE`, id, userID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, jobs.ErrLeaseLost
		}
		return false, err
	}
	var olderPublished bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM preference_profiles WHERE user_id=$1 AND id<>$2 AND publication_state='published')`, userID, id).Scan(&olderPublished); err != nil {
		return false, err
	}
	if olderPublished {
		command, err := tx.Exec(ctx, `UPDATE preference_profiles SET publication_state='failed' WHERE id=$1 AND user_id=$2 AND publication_state='building'
			AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$2)
			AND EXISTS(SELECT 1 FROM sync_jobs j WHERE j.id=$3 AND j.user_id=$2 AND j.kind=$4
				AND j.status='running' AND j.worker_id=$5 AND j.fencing_token=$6 AND j.lease_until>clock_timestamp())`,
			id, userID, lease.ID, ProfileSummaryJobKind, lease.WorkerID, lease.FencingToken)
		if err != nil {
			return false, err
		}
		if command.RowsAffected() != 1 {
			return false, jobs.ErrLeaseLost
		}
		return false, tx.Commit(ctx)
	}
	facts, err := loadActivityFacts(ctx, tx, id)
	if err != nil {
		return false, err
	}
	summary := DeterministicActivitySummary(facts)
	hash, err := ContentHash(ProfileSummaryJobKind, SchemaVersion, "deterministic-activity-fallback-v1", model, facts)
	if err != nil {
		return false, err
	}
	command, err := tx.Exec(ctx, `UPDATE preference_profiles SET summary=$1,model=$2,input_content_hash=$3,
		summary_source='deterministic',publication_state='published',published_at=now()
		WHERE id=$4 AND user_id=$5 AND publication_state='building'
		AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$5)
		AND EXISTS(SELECT 1 FROM sync_jobs j WHERE j.id=$6 AND j.user_id=$5 AND j.kind=$7
			AND j.status='running' AND j.worker_id=$8 AND j.fencing_token=$9 AND j.lease_until>clock_timestamp())`,
		summary, model, hash, id, userID, lease.ID, ProfileSummaryJobKind, lease.WorkerID, lease.FencingToken)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() != 1 {
		return false, jobs.ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func lockOwnedSummaryJobAndProfile(ctx context.Context, tx pgx.Tx, lease jobs.Lease, profileID uuid.UUID) (uuid.UUID, error) {
	var userID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT p.user_id FROM sync_jobs j JOIN preference_profiles p ON p.id=$1
		WHERE j.id=$2 AND j.kind=$3 AND j.status='running' AND j.worker_id=$4 AND j.fencing_token=$5
		AND j.lease_until>clock_timestamp() AND j.user_id=p.user_id FOR UPDATE OF j,p`,
		profileID, lease.ID, ProfileSummaryJobKind, lease.WorkerID, lease.FencingToken).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, jobs.ErrLeaseLost
	}
	return userID, err
}

func loadActivityFacts(ctx context.Context, tx pgx.Tx, profileID uuid.UUID) (ActivityFacts, error) {
	var facts ActivityFacts
	err := tx.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE playtime_minutes>0),
		count(*) FILTER (WHERE recently_played_minutes>0),
		count(*) FILTER (WHERE achieved>0),
		count(*) FILTER (WHERE completion_weight>0)
		FROM activity_profile_evidence WHERE profile_id=$1`, profileID).Scan(
		&facts.PlayedGames, &facts.RecentlyPlayedGames, &facts.GamesWithAchievements, &facts.CompletedGames,
	)
	return facts, err
}

func validPublishedSummary(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 4000
}

func uuidPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }

var _ Repository = (*SQLRepository)(nil)
