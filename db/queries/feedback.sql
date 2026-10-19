-- name: GetFeedbackEvent :one
SELECT id, recommendation_id, user_id, action, reason, reasons, free_text, idempotency_key,
       recommendation_snapshot, previous_ranking_version, resulting_ranking_version, rebuild_job_id, created_at
FROM feedback_events WHERE id = $1;

-- name: GetFeedbackEventByIdempotencyKey :one
SELECT id, recommendation_id, user_id, action, reason, reasons, free_text, idempotency_key,
       recommendation_snapshot, previous_ranking_version, resulting_ranking_version, rebuild_job_id, created_at
FROM feedback_events WHERE idempotency_key = $1;

-- name: LinkFeedbackRebuildJob :execrows
UPDATE feedback_events SET rebuild_job_id = $2 WHERE id = $1 AND (rebuild_job_id IS NULL OR rebuild_job_id = $2);

-- name: UpsertFeedbackClassification :exec
INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,schema_version,preference_dimension,preference_target,preference_direction)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,sqlc.narg(preference_dimension),sqlc.narg(preference_target),sqlc.narg(preference_direction))
ON CONFLICT(feedback_id) DO NOTHING;

-- name: GetFeedbackClassification :one
SELECT * FROM feedback_classifications WHERE feedback_id = $1;

-- name: ListEffectiveDirectReactions :many
-- embedding_snapshot is [model,input_content_hash,vector_text]; all three entries
-- are empty when no saved embedding exists, so callers never infer a zero vector.
WITH effective AS (
    SELECT DISTINCT ON (rec.game_id)
        f.id,f.user_id,rec.game_id,f.action,f.created_at
    FROM feedback_events f
    JOIN recommendations rec ON rec.id=f.recommendation_id AND rec.user_id=f.user_id
    WHERE f.user_id=sqlc.arg(user_id) AND f.action IN ('like','dislike','exclude')
    ORDER BY rec.game_id,f.created_at DESC,f.id DESC
)
SELECT effective.id AS feedback_id,effective.game_id,effective.action,effective.created_at,
       ARRAY[
           COALESCE(saved.model,''),
           COALESCE(saved.input_content_hash,''),
           COALESCE(saved.embedding::text,'')
       ]::text[] AS embedding_snapshot
FROM effective
LEFT JOIN LATERAL (
    SELECT model,input_content_hash,embedding
    FROM game_embeddings
    WHERE game_id=effective.game_id
    ORDER BY last_successful_fetch_at DESC,model
    LIMIT 1
) saved ON true
ORDER BY effective.game_id,effective.id;

-- name: ListEffectivePreferenceAdjustments :many
WITH effective_negative AS (
    SELECT DISTINCT ON (rec.game_id) f.id,f.user_id,rec.game_id,f.action
    FROM feedback_events f
    JOIN recommendations rec ON rec.id=f.recommendation_id AND rec.user_id=f.user_id
    WHERE f.user_id=sqlc.arg(user_id) AND f.action IN ('like','dislike','exclude')
    ORDER BY rec.game_id,f.created_at DESC,f.id DESC
)
SELECT adjustment.*
FROM preference_adjustments adjustment
JOIN effective_negative effective
  ON effective.id=adjustment.source_feedback_id
 AND effective.user_id=adjustment.user_id
 AND effective.game_id=adjustment.source_game_id
 AND effective.action IN ('dislike','exclude')
WHERE adjustment.user_id=sqlc.arg(user_id) AND adjustment.active
ORDER BY adjustment.created_at,adjustment.id;
