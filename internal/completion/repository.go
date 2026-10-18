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
	"next-game-crush/internal/ai"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

const completionAlgorithmVersion = ProjectionAlgorithmVersion

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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, gameID.String()); err != nil {
		return SnapshotChange{}, fmt.Errorf("lock achievement definition set: %w", err)
	}
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
			UnlockedAt: nullableTime(normalized.LastUnlockedAt), ObservedAt: timestamptz(observedAt), AlgorithmVersion: completionAlgorithmVersion,
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

func (r *SQLRepository) LoadDefinitionGenerationInput(ctx context.Context, gameID uuid.UUID, model, promptVersion, schemaVersion, lexicalRuleVersion string) (ai.AchievementSemanticEvidence, map[string]StoredClassification, string, error) {
	if gameID == uuid.Nil || !validClassificationIdentity(model, promptVersion, schemaVersion, lexicalRuleVersion) {
		return ai.AchievementSemanticEvidence{}, nil, "", errors.New("classification identity is required")
	}
	var gameName string
	if err := r.store.Pool.QueryRow(ctx, `SELECT name FROM games WHERE id=$1`, gameID).Scan(&gameName); err != nil {
		return ai.AchievementSemanticEvidence{}, nil, "", fmt.Errorf("load classification game: %w", err)
	}
	gameName = strings.TrimSpace(gameName)
	definitions, err := r.store.Queries.GetCurrentAchievementDefinitionSet(ctx, uuidPG(gameID))
	if err != nil {
		return ai.AchievementSemanticEvidence{}, nil, "", fmt.Errorf("load classification definitions: %w", err)
	}
	evidence := ai.AchievementSemanticEvidence{EvidenceKey: "achievement-set", GameName: gameName, Achievements: make([]ai.AchievementSemanticItem, 0, len(definitions))}
	for index, definition := range definitions {
		evidence.Achievements = append(evidence.Achievements, ai.AchievementSemanticItem{
			EvidenceKey: fmt.Sprintf("achievement-%03d", index+1),
			APIName:     definition.ApiName, Name: definition.DisplayName.String, Description: definition.Description.String,
		})
	}
	reusableRows, err := r.store.Pool.Query(ctx, `SELECT d.api_name,d.definition_content_hash,c.class,c.input_content_hash,c.classification_source
		FROM game_achievement_definitions d
		JOIN achievement_semantic_classifications c ON c.game_id=d.game_id AND c.api_name=d.api_name AND c.definition_content_hash=d.definition_content_hash
		WHERE d.game_id=$1 AND c.model=$2 AND c.prompt_version=$3 AND c.schema_version=$4 AND c.lexical_rule_version=$5
		ORDER BY d.api_name COLLATE "C",c.created_at DESC,c.id DESC`, gameID, model, promptVersion, schemaVersion, lexicalRuleVersion)
	if err != nil {
		return ai.AchievementSemanticEvidence{}, nil, "", fmt.Errorf("load reusable achievement classifications: %w", err)
	}
	defer reusableRows.Close()
	reusable := make(map[string]StoredClassification)
	for reusableRows.Next() {
		var row StoredClassification
		if err := reusableRows.Scan(&row.APIName, &row.DefinitionContentHash, &row.Class, &row.InputContentHash, &row.Source); err != nil {
			return ai.AchievementSemanticEvidence{}, nil, "", fmt.Errorf("scan reusable achievement classification: %w", err)
		}
		if _, exists := reusable[row.APIName]; !exists {
			reusable[row.APIName] = row
		}
	}
	if err := reusableRows.Err(); err != nil {
		return ai.AchievementSemanticEvidence{}, nil, "", fmt.Errorf("iterate reusable achievement classifications: %w", err)
	}
	return evidence, reusable, hashDefinitionRows(definitions), nil
}

func (r *SQLRepository) HasActiveClassificationGeneration(ctx context.Context, gameID uuid.UUID, model, promptVersion, schemaVersion, lexicalRuleVersion string) (bool, error) {
	var exists bool
	err := r.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM achievement_semantic_classification_generations WHERE game_id=$1 AND model=$2 AND prompt_version=$3 AND schema_version=$4 AND lexical_rule_version=$5 AND status='active')`, gameID, model, promptVersion, schemaVersion, lexicalRuleVersion).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect active classification generation: %w", err)
	}
	return exists, nil
}

func (r *SQLRepository) IsClassificationGenerationActive(ctx context.Context, gameID uuid.UUID, model, promptVersion, schemaVersion, lexicalRuleVersion, definitionSetHash string) (bool, error) {
	var exists bool
	err := r.store.Pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM achievement_semantic_classification_generations
		WHERE game_id=$1 AND model=$2 AND prompt_version=$3 AND schema_version=$4
		  AND lexical_rule_version=$5 AND definition_set_hash=$6 AND status='active'
	)`, gameID, model, promptVersion, schemaVersion, lexicalRuleVersion, definitionSetHash).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect target classification generation: %w", err)
	}
	return exists, nil
}

func (r *SQLRepository) PublishClassificationGeneration(ctx context.Context, gameID uuid.UUID, model, promptVersion, schemaVersion, lexicalRuleVersion, definitionSetHash string, results []StagedClassification, now time.Time) (bool, error) {
	if gameID == uuid.Nil || !validClassificationIdentity(model, promptVersion, schemaVersion, lexicalRuleVersion) || strings.TrimSpace(definitionSetHash) == "" {
		return false, errors.New("classification publication identity is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin classification publication: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, gameID.String()); err != nil {
		return false, fmt.Errorf("lock classification definition set: %w", err)
	}
	queries := r.store.Queries.WithTx(tx)
	definitions, err := queries.GetCurrentAchievementDefinitionSet(ctx, uuidPG(gameID))
	if err != nil {
		return false, fmt.Errorf("load publication definitions: %w", err)
	}
	if hashDefinitionRows(definitions) != definitionSetHash {
		return false, nil
	}
	if len(definitions) == 0 {
		return false, errors.New("classification generation has no definitions")
	}
	expected := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		expected[definition.ApiName] = definition.DefinitionContentHash
	}
	staged := make(map[string]StagedClassification, len(results))
	for _, result := range results {
		if _, duplicate := staged[result.APIName]; duplicate || expected[result.APIName] != result.DefinitionContentHash || !validSemanticClass(result.Class) || !validClassificationSource(result.Source) {
			return false, errors.New("classification results do not match the current definition set")
		}
		if result.InputContentHash == "" {
			result.InputContentHash = result.DefinitionContentHash
		}
		staged[result.APIName] = result
	}
	if len(staged) != len(expected) {
		return false, errors.New("classification generation is incomplete")
	}
	generationID := uuid.New()
	generation, err := queries.CreateClassificationGeneration(ctx, dbgen.CreateClassificationGenerationParams{
		ID: uuidPG(generationID), GameID: uuidPG(gameID), Model: model, PromptVersion: promptVersion,
		SchemaVersion: schemaVersion, DefinitionSetHash: definitionSetHash, LexicalRuleVersion: lexicalRuleVersion,
	})
	if err != nil {
		return false, fmt.Errorf("create classification generation: %w", err)
	}
	if generation.Status == "active" {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit classification replay: %w", err)
		}
		return false, nil
	}
	if generation.ID.Bytes == generationID {
		if _, err := tx.Exec(ctx, `UPDATE achievement_semantic_classification_generations SET created_at=$2,updated_at=$2 WHERE id=$1 AND status='staged'`, generation.ID, now); err != nil {
			return false, fmt.Errorf("timestamp classification generation: %w", err)
		}
	}
	for _, definition := range definitions {
		result := staged[definition.ApiName]
		classificationID := uuid.New()
		classification, err := queries.StageAchievementClassification(ctx, dbgen.StageAchievementClassificationParams{
			ID: uuidPG(classificationID), GenerationID: generation.ID, GameID: uuidPG(gameID), ApiName: result.APIName,
			DefinitionContentHash: result.DefinitionContentHash, Class: string(result.Class), Model: model,
			PromptVersion: promptVersion, SchemaVersion: schemaVersion, InputContentHash: result.InputContentHash,
			ClassificationSource: string(result.Source), LexicalRuleVersion: lexicalRuleVersion,
		})
		if err != nil {
			return false, fmt.Errorf("stage achievement classification: %w", err)
		}
		if classification.ID.Bytes == classificationID {
			if _, err := tx.Exec(ctx, `UPDATE achievement_semantic_classifications SET created_at=$2 WHERE id=$1`, classification.ID, now); err != nil {
				return false, fmt.Errorf("timestamp achievement classification: %w", err)
			}
		}
	}
	if _, err := queries.ActivateClassificationGeneration(ctx, generation.ID); err != nil {
		return false, fmt.Errorf("activate classification generation: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE achievement_semantic_classification_generations SET activated_at=$2,updated_at=$2 WHERE id=$1 AND status='active'`, generation.ID, now); err != nil {
		return false, fmt.Errorf("timestamp active classification generation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit classification generation: %w", err)
	}
	return true, nil
}

func (r *SQLRepository) LoadActiveClassifications(ctx context.Context, gameID uuid.UUID, model, promptVersion, schemaVersion, lexicalRuleVersion string) (map[string]StoredClassification, error) {
	definitions, err := r.store.Queries.GetCurrentAchievementDefinitionSet(ctx, uuidPG(gameID))
	if err != nil {
		return nil, fmt.Errorf("load current classification definitions: %w", err)
	}
	rows, err := r.store.Queries.GetActiveAchievementClassifications(ctx, dbgen.GetActiveAchievementClassificationsParams{
		GameID: uuidPG(gameID), Model: model, PromptVersion: promptVersion, SchemaVersion: schemaVersion, LexicalRuleVersion: lexicalRuleVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("load active achievement classifications: %w", err)
	}
	if len(rows) != len(definitions) {
		return map[string]StoredClassification{}, nil
	}
	result := make(map[string]StoredClassification, len(rows))
	for _, row := range rows {
		result[row.ApiName] = StoredClassification{APIName: row.ApiName, DefinitionContentHash: row.DefinitionContentHash, Class: ai.AchievementSemanticClass(row.Class), InputContentHash: row.InputContentHash, Source: ClassificationSource(row.ClassificationSource)}
	}
	return result, nil
}

// RebuildProjection publishes the deterministic current projection from durable
// evidence. The returned boolean reports whether the materialized projection
// changed (including removal).
func (r *SQLRepository) RebuildProjection(ctx context.Context, userID, gameID uuid.UUID, model string, now time.Time) (Projection, bool, error) {
	return r.rebuildProjection(ctx, userID, gameID, model, now, nil)
}

func (r *SQLRepository) rebuildProjection(ctx context.Context, userID, gameID uuid.UUID, model string, now time.Time, beforeCommit func(context.Context, pgx.Tx, Projection, bool) error) (Projection, bool, error) {
	if userID == uuid.Nil || gameID == uuid.Nil || strings.TrimSpace(model) == "" {
		return Projection{}, false, errors.New("completion projection identity is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Projection{}, false, fmt.Errorf("begin completion projection: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT user_id FROM user_games WHERE user_id=$1 AND game_id=$2 FOR UPDATE`, userID, gameID).Scan(new(uuid.UUID)); err != nil {
		return Projection{}, false, fmt.Errorf("lock completion user game: %w", err)
	}

	previous, previousExists, err := loadProjection(ctx, tx, userID, gameID)
	if err != nil {
		return Projection{}, false, err
	}
	var hundredPercentAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT unlocked_at FROM user_game_completion_evidence
		WHERE user_id=$1 AND game_id=$2 AND kind='hundred_percent' AND algorithm_version=$3
		ORDER BY observed_at,id LIMIT 1`, userID, gameID, ProjectionAlgorithmVersion).Scan(&hundredPercentAt)
	everHundredPercent := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Projection{}, false, fmt.Errorf("load hundred percent completion evidence: %w", err)
	}

	definitions, err := r.store.Queries.WithTx(tx).GetCurrentAchievementDefinitionSet(ctx, uuidPG(gameID))
	if err != nil {
		return Projection{}, false, fmt.Errorf("load projection definitions: %w", err)
	}
	definitionSetHash := hashDefinitionRows(definitions)
	var generationID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT generation.id
		FROM achievement_semantic_classification_generations generation
		WHERE generation.game_id=$1 AND generation.model=$2 AND generation.prompt_version=$3
		  AND generation.schema_version=$4 AND generation.lexical_rule_version=$5
		  AND generation.definition_set_hash=$6 AND generation.status='active'
		  AND (SELECT count(*) FROM achievement_semantic_classifications classification
		       WHERE classification.generation_id=generation.id)=$7
		  AND NOT EXISTS(
		       SELECT 1 FROM game_achievement_definitions definition
		       LEFT JOIN achievement_semantic_classifications classification
		         ON classification.generation_id=generation.id
		        AND classification.api_name=definition.api_name
		        AND classification.definition_content_hash=definition.definition_content_hash
		       WHERE definition.game_id=generation.game_id AND classification.id IS NULL
		  )`,
		gameID, strings.TrimSpace(model), ai.AchievementSemanticsPromptVersion,
		ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, definitionSetHash, len(definitions)).Scan(&generationID)
	activeCurrent := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Projection{}, false, fmt.Errorf("load active completion classification generation: %w", err)
	}
	if !activeCurrent && previousExists {
		changed := false
		if everHundredPercent && !previous.HundredPercentBasis {
			previous.HundredPercentBasis = true
			previous.Basis = "both"
			if previous.Weight < 1.50 {
				previous.Weight = 1.50
			}
			if err := upsertProjection(ctx, tx, userID, gameID, previous, now); err != nil {
				return Projection{}, false, err
			}
			changed = true
		}
		if beforeCommit != nil {
			if err := beforeCommit(ctx, tx, previous, changed); err != nil {
				return Projection{}, false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return Projection{}, false, fmt.Errorf("commit preserved completion projection: %w", err)
		}
		return previous, changed, nil
	}

	if activeCurrent {
		if _, err := tx.Exec(ctx, `INSERT INTO user_game_completion_evidence(
			id,user_id,game_id,kind,api_name,definition_content_hash,
			semantic_classification_id,unlocked_at,observed_at,algorithm_version
		)
		SELECT gen_random_uuid(),unlock.user_id,unlock.game_id,'ending_marker',unlock.api_name,
		       classification.definition_content_hash,classification.id,unlock.unlocked_at,
		       unlock.last_successful_fetch_at,$4
		FROM user_achievement_unlocks unlock
		JOIN achievement_semantic_classifications classification
		  ON classification.generation_id=$3 AND classification.game_id=unlock.game_id
		 AND classification.api_name=unlock.api_name
		JOIN game_achievement_definitions definition
		  ON definition.game_id=classification.game_id AND definition.api_name=classification.api_name
		 AND definition.definition_content_hash=classification.definition_content_hash
		WHERE unlock.user_id=$1 AND unlock.game_id=$2 AND unlock.ever_achieved
		  AND classification.class IN ('base_game_ending','base_game_completion')
		ON CONFLICT DO NOTHING`, userID, gameID, generationID, ProjectionAlgorithmVersion); err != nil {
			return Projection{}, false, fmt.Errorf("append ending completion evidence: %w", err)
		}
	}

	endingUnlocks := make([]time.Time, 0)
	if activeCurrent {
		rows, err := tx.Query(ctx, `SELECT DISTINCT ON (evidence.api_name) evidence.api_name,evidence.unlocked_at
			FROM user_game_completion_evidence evidence
			JOIN achievement_semantic_classifications classification
			  ON classification.id=evidence.semantic_classification_id AND classification.generation_id=$3
			JOIN game_achievement_definitions definition
			  ON definition.game_id=classification.game_id AND definition.api_name=classification.api_name
			 AND definition.definition_content_hash=classification.definition_content_hash
			WHERE evidence.user_id=$1 AND evidence.game_id=$2 AND evidence.kind='ending_marker'
			  AND evidence.algorithm_version=$4
			  AND classification.class IN ('base_game_ending','base_game_completion')
			ORDER BY evidence.api_name, evidence.unlocked_at NULLS LAST, evidence.id`,
			userID, gameID, generationID, ProjectionAlgorithmVersion)
		if err != nil {
			return Projection{}, false, fmt.Errorf("load ending completion evidence: %w", err)
		}
		for rows.Next() {
			var apiName string
			var unlockedAt pgtype.Timestamptz
			if err := rows.Scan(&apiName, &unlockedAt); err != nil {
				rows.Close()
				return Projection{}, false, fmt.Errorf("scan ending completion evidence: %w", err)
			}
			if unlockedAt.Valid {
				endingUnlocks = append(endingUnlocks, unlockedAt.Time.UTC())
			} else {
				endingUnlocks = append(endingUnlocks, time.Time{})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return Projection{}, false, fmt.Errorf("iterate ending completion evidence: %w", err)
		}
		rows.Close()
	}
	input := ProjectionInput{EverHundredPercent: everHundredPercent, EndingUnlocks: endingUnlocks}
	if hundredPercentAt.Valid {
		input.HundredPercentAt = hundredPercentAt.Time.UTC()
	}
	projection, exists := CalculateProjection(input)
	if !exists {
		if previousExists {
			if _, err := tx.Exec(ctx, `DELETE FROM user_game_completion_projections WHERE user_id=$1 AND game_id=$2`, userID, gameID); err != nil {
				return Projection{}, false, fmt.Errorf("remove invalid completion projection: %w", err)
			}
		}
		if beforeCommit != nil {
			if err := beforeCommit(ctx, tx, Projection{}, previousExists); err != nil {
				return Projection{}, false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return Projection{}, false, fmt.Errorf("commit completion projection removal: %w", err)
		}
		return Projection{}, previousExists, nil
	}
	changed := !previousExists || !sameProjection(previous, projection)
	if changed {
		if err := upsertProjection(ctx, tx, userID, gameID, projection, now); err != nil {
			return Projection{}, false, err
		}
	}
	if beforeCommit != nil {
		if err := beforeCommit(ctx, tx, projection, changed); err != nil {
			return Projection{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Projection{}, false, fmt.Errorf("commit completion projection: %w", err)
	}
	return projection, changed, nil
}

func upsertProjection(ctx context.Context, tx pgx.Tx, userID, gameID uuid.UUID, projection Projection, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO user_game_completion_projections(
		user_id,game_id,ending_count,hundred_percent_basis,weight,completion_at,basis,algorithm_version,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
	ON CONFLICT (user_id,game_id) DO UPDATE SET ending_count=EXCLUDED.ending_count,
		hundred_percent_basis=EXCLUDED.hundred_percent_basis,weight=EXCLUDED.weight,
		completion_at=EXCLUDED.completion_at,basis=EXCLUDED.basis,
		algorithm_version=EXCLUDED.algorithm_version,updated_at=EXCLUDED.updated_at`,
		userID, gameID, projection.EndingCount, projection.HundredPercentBasis, projection.Weight,
		nullableTime(projection.CompletionAt), projection.Basis, ProjectionAlgorithmVersion, now); err != nil {
		return fmt.Errorf("publish completion projection: %w", err)
	}
	return nil
}

func (r *SQLRepository) ListCompletionUsersForGame(ctx context.Context, gameID uuid.UUID) ([]uuid.UUID, error) {
	if gameID == uuid.Nil {
		return nil, errors.New("game ID is required")
	}
	rows, err := r.store.Pool.Query(ctx, `SELECT observed.user_id FROM (
		SELECT user_id FROM user_achievement_unlocks WHERE game_id=$1
		UNION SELECT user_id FROM achievement_summaries WHERE game_id=$1
	) observed
	JOIN users ON users.id=observed.user_id
	WHERE NOT EXISTS(SELECT 1 FROM deleted_user_tombstones tombstone WHERE tombstone.user_id=observed.user_id)
	ORDER BY observed.user_id`, gameID)
	if err != nil {
		return nil, fmt.Errorf("list completion users: %w", err)
	}
	defer rows.Close()
	users := make([]uuid.UUID, 0)
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("scan completion user: %w", err)
		}
		users = append(users, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completion users: %w", err)
	}
	return users, nil
}

func (r *SQLRepository) ActiveClassificationGenerationHash(ctx context.Context, gameID uuid.UUID, model string) (string, error) {
	definitions, err := r.store.Queries.GetCurrentAchievementDefinitionSet(ctx, uuidPG(gameID))
	if err != nil {
		return "", fmt.Errorf("load current classification definition set: %w", err)
	}
	currentHash := hashDefinitionRows(definitions)
	var hash string
	err = r.store.Pool.QueryRow(ctx, `SELECT generation.id::text || ':' ||
		       (extract(epoch FROM generation.activated_at)*1000000)::bigint::text || ':' ||
		       generation.definition_set_hash
		FROM achievement_semantic_classification_generations generation
		WHERE generation.game_id=$1 AND generation.model=$2 AND generation.prompt_version=$3 AND generation.schema_version=$4
		  AND generation.lexical_rule_version=$5 AND generation.definition_set_hash=$6 AND generation.status='active'`, gameID, strings.TrimSpace(model),
		ai.AchievementSemanticsPromptVersion, ai.AchievementSemanticsSchemaVersion, LexicalRuleVersion, currentHash).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load active classification generation identity: %w", err)
	}
	return hash, nil
}

func loadProjection(ctx context.Context, tx pgx.Tx, userID, gameID uuid.UUID) (Projection, bool, error) {
	var result Projection
	var completionAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT ending_count,hundred_percent_basis,weight,completion_at,basis
		FROM user_game_completion_projections WHERE user_id=$1 AND game_id=$2`, userID, gameID).Scan(
		&result.EndingCount, &result.HundredPercentBasis, &result.Weight, &completionAt, &result.Basis)
	if errors.Is(err, pgx.ErrNoRows) {
		return Projection{}, false, nil
	}
	if err != nil {
		return Projection{}, false, fmt.Errorf("load current completion projection: %w", err)
	}
	if completionAt.Valid {
		result.CompletionAt = completionAt.Time.UTC()
	}
	return result, true, nil
}

func sameProjection(left, right Projection) bool {
	return left.EndingCount == right.EndingCount &&
		left.HundredPercentBasis == right.HundredPercentBasis &&
		left.Weight == right.Weight && left.Basis == right.Basis &&
		left.CompletionAt.Equal(right.CompletionAt)
}

func validClassificationIdentity(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func validClassificationSource(source ClassificationSource) bool {
	switch source {
	case ClassificationSourceReused, ClassificationSourceLexical, ClassificationSourceOpenAI, ClassificationSourceUnknown:
		return true
	default:
		return false
	}
}

func validSemanticClass(class ai.AchievementSemanticClass) bool {
	switch class {
	case ai.AchievementSemanticBaseGameEnding, ai.AchievementSemanticBaseGameCompletion,
		ai.AchievementSemanticDLCEndingOrCompletion, ai.AchievementSemanticMetaEndingCollection,
		ai.AchievementSemanticPartialProgress, ai.AchievementSemanticOptionalChallenge,
		ai.AchievementSemanticOther, ai.AchievementSemanticUnknown:
		return true
	default:
		return false
	}
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
