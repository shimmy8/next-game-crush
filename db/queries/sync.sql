-- name: GetSyncJob :one
SELECT * FROM sync_jobs WHERE id = $1;

-- name: ListRecentSyncJobs :many
SELECT * FROM sync_jobs ORDER BY created_at DESC LIMIT $1;
