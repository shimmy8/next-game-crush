-- name: GetCatalogDiscoveryState :one
-- Legacy producers must lock/check catalog_workflow_state before enqueueing.
SELECT * FROM catalog_discovery_state WHERE source='steam' AND mode=$1;

-- name: DeadLetterQueuedLegacyDiscovery :many
UPDATE sync_jobs SET status='dead',worker_id=NULL,fencing_token=NULL,lease_until=NULL,
    last_error='superseded_catalog_workflow',updated_at=now()
WHERE kind IN ('catalog.discovery.tick','catalog.discover.incremental','catalog.discover.backfill','catalog.qualify','catalog.rebuild_wakeup')
  AND status IN ('queued','waiting','retryable')
RETURNING id;

-- name: FenceRunningLegacyDiscovery :many
UPDATE sync_jobs SET max_attempts=attempts,updated_at=now()
WHERE kind IN ('catalog.discovery.tick','catalog.discover.incremental','catalog.discover.backfill','catalog.qualify','catalog.rebuild_wakeup')
  AND status='running'
RETURNING id;

-- name: CountNonterminalLegacyDiscovery :one
SELECT count(*) FROM sync_jobs
WHERE kind IN ('catalog.discovery.tick','catalog.discover.incremental','catalog.discover.backfill','catalog.qualify','catalog.rebuild_wakeup')
  AND status IN ('queued','running','waiting','retryable');

-- name: LockCatalogDiscoveryState :one
SELECT * FROM catalog_discovery_state WHERE source='steam' AND mode=$1 FOR UPDATE;

-- name: BeginCatalogDiscoveryScan :one
UPDATE catalog_discovery_state SET cursor_app_id=0,
    watermark_at=CASE WHEN mode='incremental' THEN COALESCE(watermark_at,sqlc.arg(started_at)::timestamptz-interval '24 hours') ELSE watermark_at END,
    scan_started_at=sqlc.arg(started_at),updated_at=sqlc.arg(started_at)
WHERE source='steam' AND mode=sqlc.arg(mode) RETURNING *;

-- name: UpsertDiscoveryObservation :exec
INSERT INTO catalog_discovery_observations(source,steam_app_id,last_modified,status,precision,release_date,raw_release_text,attempts,retry_generation,next_retry_at,safe_error)
VALUES('steam',$1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT(source,steam_app_id,last_modified) DO UPDATE SET
    status=EXCLUDED.status,precision=EXCLUDED.precision,release_date=EXCLUDED.release_date,
    raw_release_text=EXCLUDED.raw_release_text,last_attempt_at=now(),
    attempts=GREATEST(catalog_discovery_observations.attempts,EXCLUDED.attempts),
    retry_generation=GREATEST(catalog_discovery_observations.retry_generation,EXCLUDED.retry_generation),
    next_retry_at=EXCLUDED.next_retry_at,safe_error=EXCLUDED.safe_error
WHERE EXCLUDED.retry_generation>=catalog_discovery_observations.retry_generation
    AND EXCLUDED.attempts>=catalog_discovery_observations.attempts;

-- name: ListDueDiscoveryFailures :many
SELECT * FROM catalog_discovery_observations
WHERE status='failed' AND next_retry_at<=sqlc.arg(due_at)
ORDER BY next_retry_at,steam_app_id,last_modified LIMIT sqlc.arg(result_limit);

-- name: GetDiscoveryObservation :one
SELECT * FROM catalog_discovery_observations WHERE source='steam' AND steam_app_id=$1 AND last_modified=$2;

-- name: LockDiscoveryObservation :one
SELECT * FROM catalog_discovery_observations WHERE source='steam' AND steam_app_id=$1 AND last_modified=$2 FOR UPDATE;

-- name: CountDiscoveryProvenance :one
SELECT count(DISTINCT game_id) FROM catalog_entry_sources WHERE source='steam_discovery' AND active;

-- name: ListPendingDiscoveryQualifications :many
SELECT id FROM sync_jobs WHERE kind='catalog.qualify' AND status IN ('queued','running','waiting','retryable') ORDER BY available_at,created_at,id;

-- name: EnsureDiscoveryBudget :exec
INSERT INTO catalog_discovery_budget(hour_at) VALUES($1) ON CONFLICT DO NOTHING;

-- name: LockDiscoveryBudget :one
SELECT * FROM catalog_discovery_budget WHERE hour_at=$1 FOR UPDATE;

-- name: ReserveDiscoveryBudget :exec
UPDATE catalog_discovery_budget SET reservations=reservations||jsonb_build_object(sqlc.arg(identity_hash)::text,true),used=used+1 WHERE hour_at=sqlc.arg(hour_at);

-- name: LockCatalogEnrichmentExecution :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(execution_prefix)::text,0));

-- name: GetLatestCatalogEnrichmentExecution :one
SELECT idempotency_key,status,split_part(idempotency_key,':',5)::bigint AS generation
FROM sync_jobs WHERE parent_job_id IS NULL AND idempotency_key LIKE sqlc.arg(execution_prefix)::text || '%'
ORDER BY generation DESC LIMIT 1 FOR UPDATE;
