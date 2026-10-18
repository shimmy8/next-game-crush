-- name: AdvanceAchievementSnapshot :one
UPDATE achievement_summaries
SET snapshot_generation = snapshot_generation + 1
WHERE user_id = sqlc.arg(user_id) AND game_id = sqlc.arg(game_id)
RETURNING snapshot_generation;

-- name: UpsertAchievementDefinition :one
INSERT INTO game_achievement_definitions(
    id, game_id, api_name, display_name, description, metadata_source,
    definition_content_hash, last_successful_fetch_at
)
VALUES(
    sqlc.arg(id), sqlc.arg(game_id), sqlc.arg(api_name), sqlc.narg(display_name),
    sqlc.narg(description), sqlc.arg(metadata_source),
    sqlc.arg(definition_content_hash), sqlc.arg(last_successful_fetch_at)
)
ON CONFLICT (game_id, api_name) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    description = EXCLUDED.description,
    metadata_source = EXCLUDED.metadata_source,
    definition_content_hash = EXCLUDED.definition_content_hash,
    last_successful_fetch_at = EXCLUDED.last_successful_fetch_at,
    updated_at = now()
RETURNING *;

-- name: UpsertAchievementUnlock :one
INSERT INTO user_achievement_unlocks(
    user_id, game_id, api_name, present_in_latest, achieved_in_latest,
    ever_achieved, unlocked_at, snapshot_generation, source,
    last_successful_fetch_at
)
VALUES(
    sqlc.arg(user_id), sqlc.arg(game_id), sqlc.arg(api_name), true,
    sqlc.arg(achieved_in_latest), sqlc.arg(achieved_in_latest),
    sqlc.narg(unlocked_at), sqlc.arg(snapshot_generation),
    sqlc.arg(source), sqlc.arg(last_successful_fetch_at)
)
ON CONFLICT (user_id, game_id, api_name) DO UPDATE SET
    present_in_latest = true,
    achieved_in_latest = EXCLUDED.achieved_in_latest,
    ever_achieved = user_achievement_unlocks.ever_achieved OR EXCLUDED.ever_achieved,
    unlocked_at = CASE
        WHEN user_achievement_unlocks.unlocked_at IS NULL THEN EXCLUDED.unlocked_at
        WHEN EXCLUDED.unlocked_at IS NULL THEN user_achievement_unlocks.unlocked_at
        ELSE LEAST(user_achievement_unlocks.unlocked_at, EXCLUDED.unlocked_at)
    END,
    snapshot_generation = EXCLUDED.snapshot_generation,
    source = EXCLUDED.source,
    last_successful_fetch_at = EXCLUDED.last_successful_fetch_at,
    updated_at = now()
RETURNING *;

-- name: MarkAchievementUnlocksAbsent :execrows
UPDATE user_achievement_unlocks
SET present_in_latest = false,
    achieved_in_latest = false,
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND game_id = sqlc.arg(game_id)
  AND snapshot_generation <> sqlc.arg(snapshot_generation);

-- name: GetCurrentAchievementDefinitionSet :many
SELECT *
FROM game_achievement_definitions
WHERE game_id = sqlc.arg(game_id)
ORDER BY api_name COLLATE "C", id;

-- name: GetReusableAchievementClassifications :many
SELECT DISTINCT ON (d.api_name)
    d.api_name,
    d.definition_content_hash,
    c.id AS classification_id,
    c.class,
    c.input_content_hash,
    c.classification_source,
    c.created_at
FROM game_achievement_definitions d
JOIN achievement_semantic_classifications c
  ON c.game_id = d.game_id
 AND c.api_name = d.api_name
 AND c.definition_content_hash = d.definition_content_hash
WHERE d.game_id = sqlc.arg(game_id)
  AND c.model = sqlc.arg(model)
  AND c.prompt_version = sqlc.arg(prompt_version)
  AND c.schema_version = sqlc.arg(schema_version)
  AND c.lexical_rule_version = sqlc.arg(lexical_rule_version)
ORDER BY d.api_name COLLATE "C", c.created_at DESC, c.id DESC;

-- name: CreateClassificationGeneration :one
INSERT INTO achievement_semantic_classification_generations(
    id, game_id, model, prompt_version, schema_version, definition_set_hash,
    lexical_rule_version, status
)
VALUES(
    sqlc.arg(id), sqlc.arg(game_id), sqlc.arg(model), sqlc.arg(prompt_version),
    sqlc.arg(schema_version), sqlc.arg(definition_set_hash),
    sqlc.arg(lexical_rule_version), 'staged'
)
ON CONFLICT (
    game_id, model, prompt_version, schema_version, lexical_rule_version,
    definition_set_hash
) DO UPDATE SET updated_at = achievement_semantic_classification_generations.updated_at
RETURNING *;

-- name: StageAchievementClassification :one
INSERT INTO achievement_semantic_classifications(
    id, generation_id, game_id, api_name, definition_content_hash, class,
    model, prompt_version, schema_version, input_content_hash,
    classification_source, lexical_rule_version
)
VALUES(
    sqlc.arg(id), sqlc.arg(generation_id), sqlc.arg(game_id), sqlc.arg(api_name),
    sqlc.arg(definition_content_hash), sqlc.arg(class), sqlc.arg(model),
    sqlc.arg(prompt_version), sqlc.arg(schema_version), sqlc.arg(input_content_hash),
    sqlc.arg(classification_source), sqlc.arg(lexical_rule_version)
)
ON CONFLICT (generation_id, api_name) DO UPDATE SET
    api_name = achievement_semantic_classifications.api_name
RETURNING *;

-- name: ActivateClassificationGeneration :one
WITH candidate AS (
    SELECT generation.id, generation.game_id, generation.model,
           generation.prompt_version, generation.schema_version,
           generation.lexical_rule_version
    FROM achievement_semantic_classification_generations generation
    WHERE generation.id = sqlc.arg(id)
      AND generation.status IN ('staged','superseded')
      AND (SELECT count(*) FROM achievement_semantic_classifications classification
           WHERE classification.generation_id = generation.id)
          = (SELECT count(*) FROM game_achievement_definitions definition
             WHERE definition.game_id = generation.game_id)
      AND NOT EXISTS (
          SELECT 1
          FROM game_achievement_definitions definition
          LEFT JOIN achievement_semantic_classifications classification
            ON classification.generation_id = generation.id
           AND classification.api_name = definition.api_name
           AND classification.definition_content_hash = definition.definition_content_hash
          WHERE definition.game_id = generation.game_id
            AND classification.id IS NULL
      )
      AND NOT EXISTS (
          SELECT 1
          FROM achievement_semantic_classifications classification
          LEFT JOIN game_achievement_definitions definition
            ON definition.game_id = generation.game_id
           AND definition.api_name = classification.api_name
           AND definition.definition_content_hash = classification.definition_content_hash
          WHERE classification.generation_id = generation.id
            AND definition.id IS NULL
      )
), superseded AS (
    UPDATE achievement_semantic_classification_generations generation
    SET status = 'superseded', updated_at = now()
    FROM candidate
    WHERE generation.game_id = candidate.game_id
      AND generation.model = candidate.model
      AND generation.prompt_version = candidate.prompt_version
      AND generation.schema_version = candidate.schema_version
      AND generation.lexical_rule_version = candidate.lexical_rule_version
      AND generation.status = 'active'
    RETURNING generation.id
)
UPDATE achievement_semantic_classification_generations generation
SET status = 'active', activated_at = now(), updated_at = now()
FROM candidate
WHERE generation.id = candidate.id
  AND (SELECT count(*) FROM superseded) >= 0
RETURNING generation.*;

-- name: GetActiveAchievementClassifications :many
SELECT classification.*
FROM achievement_semantic_classifications classification
JOIN achievement_semantic_classification_generations generation
  ON generation.id = classification.generation_id
JOIN game_achievement_definitions definition
  ON definition.game_id = classification.game_id
 AND definition.api_name = classification.api_name
 AND definition.definition_content_hash = classification.definition_content_hash
WHERE classification.game_id = sqlc.arg(game_id)
  AND generation.status = 'active'
  AND generation.model = sqlc.arg(model)
  AND generation.prompt_version = sqlc.arg(prompt_version)
  AND generation.schema_version = sqlc.arg(schema_version)
  AND generation.lexical_rule_version = sqlc.arg(lexical_rule_version)
ORDER BY classification.api_name COLLATE "C", classification.id;

-- name: InsertCompletionEvidence :execrows
INSERT INTO user_game_completion_evidence(
    id, user_id, game_id, kind, api_name, definition_content_hash,
    semantic_classification_id, unlocked_at, observed_at, algorithm_version
)
VALUES(
    sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(game_id), sqlc.arg(kind),
    sqlc.narg(api_name), sqlc.narg(definition_content_hash),
    sqlc.narg(semantic_classification_id), sqlc.narg(unlocked_at),
    sqlc.arg(observed_at), sqlc.arg(algorithm_version)
)
ON CONFLICT DO NOTHING;

-- name: GetCompletionProjectionInput :many
SELECT evidence.kind,
       evidence.api_name,
       evidence.unlocked_at,
       classification.class,
       evidence.observed_at
FROM user_game_completion_evidence evidence
LEFT JOIN achievement_semantic_classifications classification
  ON classification.id = evidence.semantic_classification_id
LEFT JOIN achievement_semantic_classification_generations generation
  ON generation.id = classification.generation_id AND generation.status = 'active'
LEFT JOIN game_achievement_definitions definition
  ON definition.game_id = classification.game_id
 AND definition.api_name = classification.api_name
 AND definition.definition_content_hash = classification.definition_content_hash
WHERE evidence.user_id = sqlc.arg(user_id)
  AND evidence.game_id = sqlc.arg(game_id)
  AND evidence.algorithm_version = sqlc.arg(algorithm_version)
  AND (
      evidence.kind = 'hundred_percent'
      OR (
          evidence.kind = 'ending_marker'
          AND generation.id IS NOT NULL
          AND generation.model = sqlc.arg(model)
          AND generation.prompt_version = sqlc.arg(prompt_version)
          AND generation.schema_version = sqlc.arg(schema_version)
          AND generation.lexical_rule_version = sqlc.arg(lexical_rule_version)
          AND definition.id IS NOT NULL
          AND classification.class IN ('base_game_ending','base_game_completion')
      )
  )
ORDER BY evidence.kind COLLATE "C", evidence.api_name COLLATE "C" NULLS FIRST,
         evidence.semantic_classification_id NULLS FIRST;

-- name: UpsertCompletionProjection :one
INSERT INTO user_game_completion_projections(
    user_id, game_id, ending_count, hundred_percent_basis, weight,
    completion_at, basis, algorithm_version, updated_at
)
VALUES(
    sqlc.arg(user_id), sqlc.arg(game_id), sqlc.arg(ending_count),
    sqlc.arg(hundred_percent_basis), sqlc.arg(weight), sqlc.narg(completion_at),
    sqlc.arg(basis), sqlc.arg(algorithm_version), sqlc.arg(updated_at)
)
ON CONFLICT (user_id, game_id) DO UPDATE SET
    ending_count = EXCLUDED.ending_count,
    hundred_percent_basis = EXCLUDED.hundred_percent_basis,
    weight = EXCLUDED.weight,
    completion_at = EXCLUDED.completion_at,
    basis = EXCLUDED.basis,
    algorithm_version = EXCLUDED.algorithm_version,
    updated_at = EXCLUDED.updated_at
RETURNING *;

-- name: DeleteCompletionProjection :execrows
DELETE FROM user_game_completion_projections
WHERE user_id = sqlc.arg(user_id) AND game_id = sqlc.arg(game_id);

-- name: ListCompletionUsersForGame :many
SELECT observed.user_id
FROM (
    SELECT unlock.user_id
    FROM user_achievement_unlocks unlock
    WHERE unlock.game_id = sqlc.arg(game_id)
    UNION
    SELECT summary.user_id
    FROM achievement_summaries summary
    WHERE summary.game_id = sqlc.arg(game_id)
) observed
ORDER BY observed.user_id;
