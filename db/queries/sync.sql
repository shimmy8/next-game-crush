-- name: GetSyncJob :one
SELECT * FROM sync_jobs WHERE id = $1;

-- name: ListRecentSyncJobs :many
SELECT * FROM sync_jobs ORDER BY created_at DESC LIMIT $1;

-- name: FindActiveUserSync :one
SELECT * FROM sync_jobs
WHERE kind = 'steam.sync'
  AND payload->>'user_id' = sqlc.arg(user_id)::text
  AND status IN ('queued','running','waiting','retryable')
ORDER BY created_at DESC
LIMIT 1;
