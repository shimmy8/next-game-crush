package completion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

const completionAlgorithmVersion = "completion-v1"

type SQLRepository struct {
	store *store.Store
}

func NewSQLRepository(database *store.Store) (*SQLRepository, error) {
	if database == nil || database.Pool == nil || database.Queries == nil {
		return nil, errors.New("completion store is required")
	}
	return &SQLRepository{store: database}, nil
}

func (r *SQLRepository) RecordAchievementSnapshot(ctx context.Context, userID, gameID uuid.UUID, summary steam.AchievementSummary, observedAt time.Time) error {
	_, err := r.SaveSteamSnapshot(ctx, userID, gameID, summary, observedAt)
	return err
}

func (r *SQLRepository) SaveSteamSnapshot(ctx context.Context, userID, gameID uuid.UUID, summary steam.AchievementSummary, observedAt time.Time) (SnapshotChange, error) {
	if userID == uuid.Nil || gameID == uuid.Nil {
		return SnapshotChange{}, errors.New("user ID and game ID are required")
	}
	observedAt = observedAt.UTC()
	normalized := normalizeSnapshot(summary)
	raw, err := json.Marshal(normalized)
	if err != nil {
		return SnapshotChange{}, fmt.Errorf("marshal achievement snapshot: %w", err)
	}

	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return SnapshotChange{}, fmt.Errorf("begin achievement snapshot: %w", err)
	}
	defer tx.Rollback(ctx)
	var gameName string
	if err := tx.QueryRow(ctx, `SELECT g.name FROM user_games ug JOIN games g ON g.id=ug.game_id WHERE ug.user_id=$1 AND ug.game_id=$2 FOR UPDATE OF ug`, userID, gameID).Scan(&gameName); err != nil {
		return SnapshotChange{}, fmt.Errorf("lock user game for achievement snapshot: %w", err)
	}
	gameName = strings.TrimSpace(gameName)
	queries := r.store.Queries.WithTx(tx)
	before, err := queries.GetCurrentAchievementDefinitionSet(ctx, uuidPG(gameID))
	if err != nil {
		return SnapshotChange{}, fmt.Errorf("load achievement definitions: %w", err)
	}
	beforeHash := hashDefinitionRows(before)
	priorDefinitions := make(map[string]dbgen.GameAchievementDefinition, len(before))
	for _, definition := range before {
		priorDefinitions[definition.ApiName] = definition
	}

	if err := queries.UpsertAchievementSummary(ctx, dbgen.UpsertAchievementSummaryParams{
		UserID: uuidPG(userID), GameID: uuidPG(gameID), Achieved: int32(normalized.Unlocked), Total: int32(normalized.Total),
		Source: "steam", LastSuccessfulFetchAt: timestamptz(observedAt), RawSnapshot: raw,
	}); err != nil {
		return SnapshotChange{}, fmt.Errorf("upsert achievement summary: %w", err)
	}
	generation, err := queries.AdvanceAchievementSnapshot(ctx, dbgen.AdvanceAchievementSnapshotParams{UserID: uuidPG(userID), GameID: uuidPG(gameID)})
	if err != nil {
		return SnapshotChange{}, fmt.Errorf("advance achievement snapshot: %w", err)
	}
	currentDefinitions := make(map[string]struct{}, len(normalized.Achievements))
	for _, achievement := range normalized.Achievements {
		if achievement.APIName == "" {
			continue
		}
		name, description := achievement.Name, achievement.Description
		metadataSource := strings.TrimSpace(achievement.MetadataSource)
		if metadataSource != "game_schema" {
			metadataSource = "player_achievements"
		}
		if prior, ok := priorDefinitions[achievement.APIName]; ok {
			if name == "" && prior.DisplayName.Valid {
				name = prior.DisplayName.String
				if metadataSource != "game_schema" {
					metadataSource = prior.MetadataSource
				}
			}
			if description == "" && prior.Description.Valid {
				description = prior.Description.String
				if metadataSource != "game_schema" {
					metadataSource = prior.MetadataSource
				}
			}
		}
		if name != "" || description != "" {
			currentDefinitions[achievement.APIName] = struct{}{}
			contentHash := hashDefinition(gameName, achievement.APIName, name, description)
			if _, err := queries.UpsertAchievementDefinition(ctx, dbgen.UpsertAchievementDefinitionParams{
				ID: uuidPG(uuid.New()), GameID: uuidPG(gameID), ApiName: achievement.APIName,
				DisplayName: text(name), Description: text(description), MetadataSource: metadataSource,
				DefinitionContentHash: contentHash, LastSuccessfulFetchAt: timestamptz(observedAt),
			}); err != nil {
				return SnapshotChange{}, fmt.Errorf("upsert achievement definition: %w", err)
			}
		}
		if _, err := queries.UpsertAchievementUnlock(ctx, dbgen.UpsertAchievementUnlockParams{
			UserID: uuidPG(userID), GameID: uuidPG(gameID), ApiName: achievement.APIName,
			AchievedInLatest: achievement.Achieved, UnlockedAt: nullableTime(achievement.UnlockedAt),
			SnapshotGeneration: generation, Source: "player_achievements", LastSuccessfulFetchAt: timestamptz(observedAt),
		}); err != nil {
			return SnapshotChange{}, fmt.Errorf("upsert achievement unlock: %w", err)
		}
	}
	for _, prior := range before {
		if _, current := currentDefinitions[prior.ApiName]; current {
			continue
		}
		name, description := prior.DisplayName.String, prior.Description.String
		contentHash := hashDefinition(gameName, prior.ApiName, name, description)
		if contentHash == prior.DefinitionContentHash {
			continue
		}
		if _, err := queries.UpsertAchievementDefinition(ctx, dbgen.UpsertAchievementDefinitionParams{
			ID: uuidPG(prior.ID.Bytes), GameID: uuidPG(gameID), ApiName: prior.ApiName,
			DisplayName: text(name), Description: text(description), MetadataSource: prior.MetadataSource,
			DefinitionContentHash: contentHash, LastSuccessfulFetchAt: prior.LastSuccessfulFetchAt,
		}); err != nil {
			return SnapshotChange{}, fmt.Errorf("refresh achievement definition game context: %w", err)
		}
	}
	if _, err := queries.MarkAchievementUnlocksAbsent(ctx, dbgen.MarkAchievementUnlocksAbsentParams{UserID: uuidPG(userID), GameID: uuidPG(gameID), SnapshotGeneration: generation}); err != nil {
		return SnapshotChange{}, fmt.Errorf("mark absent achievement unlocks: %w", err)
	}
	hundredPercent := normalized.Total > 0 && normalized.Unlocked == normalized.Total
	if hundredPercent {
		if _, err := queries.InsertCompletionEvidence(ctx, dbgen.InsertCompletionEvidenceParams{
			ID: uuidPG(uuid.New()), UserID: uuidPG(userID), GameID: uuidPG(gameID), Kind: "hundred_percent",
			ObservedAt: timestamptz(observedAt), AlgorithmVersion: completionAlgorithmVersion,
		}); err != nil {
			return SnapshotChange{}, fmt.Errorf("insert hundred percent evidence: %w", err)
		}
	}
	after, err := queries.GetCurrentAchievementDefinitionSet(ctx, uuidPG(gameID))
	if err != nil {
		return SnapshotChange{}, fmt.Errorf("reload achievement definitions: %w", err)
	}
	afterHash := hashDefinitionRows(after)
	if err := tx.Commit(ctx); err != nil {
		return SnapshotChange{}, fmt.Errorf("commit achievement snapshot: %w", err)
	}
	return SnapshotChange{DefinitionSetHash: afterHash, DefinitionsChanged: beforeHash != afterHash, HundredPercentObserved: hundredPercent}, nil
}

func normalizeSnapshot(summary steam.AchievementSummary) steam.AchievementSummary {
	type accumulator struct {
		achievement steam.Achievement
	}
	byAPIName := make(map[string]accumulator, len(summary.Achievements))
	for _, candidate := range summary.Achievements {
		candidate.APIName = strings.TrimSpace(candidate.APIName)
		if candidate.APIName == "" {
			continue
		}
		candidate.Name = strings.TrimSpace(candidate.Name)
		candidate.Description = strings.TrimSpace(candidate.Description)
		current := byAPIName[candidate.APIName].achievement
		current.APIName = candidate.APIName
		if candidate.Name > current.Name {
			current.Name = candidate.Name
		}
		if candidate.Description > current.Description {
			current.Description = candidate.Description
		}
		if candidate.MetadataSource == "game_schema" {
			current.MetadataSource = "game_schema"
		} else if current.MetadataSource == "" {
			current.MetadataSource = "player_achievements"
		}
		current.Achieved = current.Achieved || candidate.Achieved
		if !candidate.UnlockedAt.IsZero() && (current.UnlockedAt.IsZero() || candidate.UnlockedAt.Before(current.UnlockedAt)) {
			current.UnlockedAt = candidate.UnlockedAt.UTC()
		}
		byAPIName[candidate.APIName] = accumulator{achievement: current}
	}
	result := summary
	result.Achievements = make([]steam.Achievement, 0, len(byAPIName))
	result.Unlocked = 0
	result.LastUnlockedAt = time.Time{}
	for _, value := range byAPIName {
		achievement := value.achievement
		result.Achievements = append(result.Achievements, achievement)
		if achievement.Achieved {
			result.Unlocked++
		}
		if achievement.UnlockedAt.After(result.LastUnlockedAt) {
			result.LastUnlockedAt = achievement.UnlockedAt
		}
	}
	sort.Slice(result.Achievements, func(i, j int) bool { return result.Achievements[i].APIName < result.Achievements[j].APIName })
	result.Total = len(result.Achievements)
	if result.Total == 0 {
		result.Percentage = 0
	} else {
		result.Percentage = float64(result.Unlocked) * 100 / float64(result.Total)
	}
	return result
}

func hashDefinition(gameName, apiName, name, description string) string {
	encoded, _ := json.Marshal([]string{strings.TrimSpace(gameName), apiName, name, description})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func hashDefinitionRows(rows []dbgen.GameAchievementDefinition) string {
	definitions := make([][2]string, 0, len(rows))
	for _, row := range rows {
		definitions = append(definitions, [2]string{row.ApiName, row.DefinitionContentHash})
	}
	sort.Slice(definitions, func(i, j int) bool {
		if definitions[i][0] != definitions[j][0] {
			return definitions[i][0] < definitions[j][0]
		}
		return definitions[i][1] < definitions[j][1]
	})
	encoded, _ := json.Marshal(definitions)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func uuidPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }
func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
func nullableTime(value time.Time) pgtype.Timestamptz {
	if value.IsZero() {
		return pgtype.Timestamptz{}
	}
	return timestamptz(value.UTC())
}
func text(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }
