-- name: ListPreferenceAdjustments :many
SELECT * FROM preference_adjustments
WHERE user_id = sqlc.arg(user_id) AND (active OR sqlc.arg(include_inactive)::boolean)
ORDER BY created_at,id;

-- name: GetPreferenceAdjustmentForFeedback :one
SELECT * FROM preference_adjustments
WHERE user_id = sqlc.arg(user_id) AND source_feedback_id = sqlc.arg(source_feedback_id);

-- Source game and ownership come from server-owned event/recommendation rows.
-- A retry returns the stored row without reactivating a disabled adjustment.
-- name: UpsertPreferenceAdjustmentFromFeedback :one
INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)
SELECT sqlc.arg(id),event.user_id,event.id,rec.game_id,sqlc.arg(dimension),sqlc.arg(target),sqlc.arg(direction),sqlc.arg(confidence),sqlc.arg(strength)
FROM feedback_events event JOIN recommendations rec ON rec.id = event.recommendation_id AND rec.user_id = event.user_id
WHERE event.id = sqlc.arg(source_feedback_id) AND event.user_id = sqlc.arg(user_id) AND event.action = 'dislike'
ON CONFLICT(source_feedback_id) DO UPDATE SET source_feedback_id = EXCLUDED.source_feedback_id
RETURNING *;

-- name: LockPreferenceAdjustment :one
SELECT * FROM preference_adjustments WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(id) FOR UPDATE;

-- name: MarkPreferenceAdjustmentDisabled :execrows
UPDATE preference_adjustments SET active = false,
    disable_rebuild_job_id = sqlc.arg(disable_rebuild_job_id),
    disable_previous_ranking_version = sqlc.arg(disable_previous_ranking_version),
    disabled_at = sqlc.arg(disabled_at), updated_at = sqlc.arg(disabled_at)
WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(id) AND active;
