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
