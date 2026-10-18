-- name: EnqueueJob :one
INSERT INTO sync_jobs (id, kind, idempotency_key, payload, max_attempts, available_at, parent_job_id, required, stage, user_id, priority)
VALUES ($1, $2, $3, $4, $5, COALESCE(sqlc.narg(available_at)::timestamptz, now()), sqlc.narg(parent_job_id), sqlc.arg(required), sqlc.arg(stage), sqlc.narg(user_id), sqlc.arg(priority))
ON CONFLICT (idempotency_key) DO UPDATE SET priority = GREATEST(sync_jobs.priority, EXCLUDED.priority)
RETURNING id;

-- name: ClaimJob :one
WITH candidate AS (
    SELECT id FROM sync_jobs
    WHERE attempts < max_attempts
      AND available_at <= now()
      AND (status IN ('queued', 'retryable') OR (status = 'running' AND lease_until <= now()))
    ORDER BY priority DESC, available_at, created_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
), claimed AS (
    UPDATE sync_jobs j SET
        attempts = attempts + 1,
        status = 'running', worker_id = sqlc.arg(worker_id),
        fencing_token = sqlc.arg(fencing_token),
        lease_until = now() + sqlc.arg(lease_duration)::interval,
        last_error = NULL,
        updated_at = now()
    FROM candidate c WHERE j.id = c.id
    RETURNING j.*
)
SELECT * FROM claimed;

-- name: GetJobWorkflowID :one
WITH RECURSIVE ancestors AS (
    SELECT job.id, job.parent_job_id
    FROM sync_jobs job
    WHERE job.id = $1

    UNION ALL

    SELECT parent.id, parent.parent_job_id
    FROM sync_jobs parent
    JOIN ancestors child ON child.parent_job_id = parent.id
)
SELECT ancestors.id
FROM ancestors
WHERE ancestors.parent_job_id IS NULL
LIMIT 1;

-- name: InsertJobAttempt :exec
INSERT INTO job_attempts (id, job_id, attempt, worker_id, fencing_token, started_at, status)
SELECT $1, j.id, j.attempts, j.worker_id, j.fencing_token, now(), 'running'
FROM sync_jobs j WHERE j.id = $2 AND j.status = 'running' AND j.worker_id = $3 AND j.fencing_token = $4;

-- name: FinishExpiredAttempts :exec
UPDATE job_attempts a SET status = 'expired', error = 'lease expired', finished_at = now()
FROM sync_jobs j
WHERE a.job_id = j.id AND a.fencing_token = j.fencing_token AND a.status = 'running'
  AND j.status = 'running' AND j.lease_until <= now();

-- name: DeadLetterExhaustedLeases :exec
UPDATE sync_jobs SET status = 'dead', worker_id = NULL, fencing_token = NULL,
    lease_until = NULL, last_error = 'lease expired after maximum attempts', updated_at = now()
WHERE status = 'running' AND lease_until <= now() AND attempts >= max_attempts;

-- name: FailParentsWithDeadRequiredChildren :exec
UPDATE sync_jobs parent SET status='dead', last_error='required child stage failed', updated_at=now()
WHERE parent.status='waiting' AND EXISTS (
    SELECT 1 FROM sync_jobs child
    WHERE child.parent_job_id=parent.id AND child.required AND child.status='dead'
);

-- name: RenewJob :execrows
UPDATE sync_jobs SET lease_until = now() + sqlc.arg(lease_duration)::interval, updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'running' AND worker_id = sqlc.arg(worker_id)
  AND fencing_token = sqlc.arg(fencing_token) AND lease_until > now();

-- name: CompleteJob :execrows
UPDATE sync_jobs SET status = 'succeeded', worker_id = NULL, fencing_token = NULL,
    lease_until = NULL, last_error = NULL, updated_at = now()
WHERE id = $1 AND status = 'running' AND worker_id = $2 AND fencing_token = $3 AND lease_until > now();

-- name: FinishSuccessfulAttempt :exec
UPDATE job_attempts SET status = 'succeeded', finished_at = now()
WHERE job_id = $1 AND worker_id = $2 AND fencing_token = $3 AND status = 'running';

-- name: DeferJobUntil :execrows
UPDATE sync_jobs SET status = 'queued', attempts = GREATEST(attempts - 1, 0),
    available_at = sqlc.arg(available_at), worker_id = NULL, fencing_token = NULL,
    lease_until = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'running' AND worker_id = sqlc.arg(worker_id)
  AND fencing_token = sqlc.arg(fencing_token) AND lease_until > now()
  AND sqlc.arg(available_at)::timestamptz > now();

-- name: FinishDeferredAttempt :exec
UPDATE job_attempts SET status = 'waiting', finished_at = now()
WHERE job_id = $1 AND worker_id = $2 AND fencing_token = $3 AND status = 'running';

-- name: FailJob :execrows
UPDATE sync_jobs SET
    status = CASE WHEN NOT sqlc.arg(retryable)::boolean OR attempts >= max_attempts THEN 'dead' ELSE 'retryable' END,
    available_at = sqlc.arg(retry_at), last_error = sqlc.arg(cause),
    worker_id = NULL, fencing_token = NULL, lease_until = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'running' AND worker_id = sqlc.arg(worker_id)
  AND fencing_token = sqlc.arg(fencing_token) AND lease_until > now();

-- name: FinishFailedAttempt :exec
UPDATE job_attempts SET status = $4, error = $5, finished_at = now()
WHERE job_id = $1 AND worker_id = $2 AND fencing_token = $3 AND status = 'running';

-- name: GetJobChildren :many
SELECT * FROM sync_jobs WHERE parent_job_id = $1 ORDER BY created_at, id;
