-- name: CreateOrGetSearchRun :one
INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,config_snapshot)
VALUES($1,$2,$3,$4)
ON CONFLICT(schedule_name,occurrence_at) DO UPDATE SET schedule_name=EXCLUDED.schedule_name
RETURNING *;

-- name: AllocateObservationOrdinal :one
UPDATE catalog_search_runs SET observation_ordinal=observation_ordinal+1
WHERE id=$1 RETURNING observation_ordinal;

-- name: CommitSearchPage :one
INSERT INTO catalog_search_pages(run_id,feed,start_offset,page_generation,observation_ordinal,next_offset,result_count,has_more)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT(run_id,feed,start_offset,page_generation) DO NOTHING
RETURNING *;

-- Publication protocol: begin a READ COMMITTED transaction, run
-- LockSearchFeedPublication, then PublishFeedMembership as a separate statement,
-- then commit. The separate statement gets a fresh snapshot after any prior
-- publisher commits. Acquiring this lock inside the publication CTE is unsafe:
-- its pre-wait snapshot can miss the previous publisher's new membership rows.
-- name: LockSearchFeedPublication :one
SELECT * FROM catalog_search_feed_publications WHERE feed=$1 FOR UPDATE;

-- Requires LockSearchFeedPublication in the same READ COMMITTED transaction.
-- name: PublishFeedMembership :one
WITH published AS (
    UPDATE catalog_search_feed_publications p SET published_run_revision=r.run_revision,published_at=sqlc.arg(published_at)
    FROM catalog_search_runs r JOIN catalog_search_feeds f ON f.run_id=r.id
    WHERE r.id=sqlc.arg(run_id) AND f.feed=sqlc.arg(feed) AND p.feed=f.feed
        AND r.run_revision>p.published_run_revision AND f.status='complete' AND f.safety_cap_reason=''
        AND ((f.feed IN ('popular_upcoming','popular_new') AND f.natural_end AND NOT f.boundary_stop)
            OR (f.feed IN ('recent','coming_soon') AND (f.natural_end OR (f.boundary_stop AND f.monotonic))))
    RETURNING p.feed,p.published_run_revision,p.published_at
), observed AS (
    SELECT DISTINCT o.steam_app_id FROM catalog_search_observations o
    WHERE o.run_id=sqlc.arg(run_id) AND o.feed=sqlc.arg(feed)
), changed AS (
    INSERT INTO catalog_search_memberships(feed,steam_app_id,published_run_revision,active,omission_count,first_seen_at,last_seen_at,latest_complete_at)
    SELECT p.feed,o.steam_app_id,p.published_run_revision,true,0,p.published_at,p.published_at,p.published_at
    FROM published p CROSS JOIN observed o
    UNION ALL
    SELECT p.feed,m.steam_app_id,p.published_run_revision,
        CASE WHEN p.feed IN ('popular_upcoming','popular_new') THEN false ELSE m.active AND m.omission_count+1<2 END,
        m.omission_count+1,m.first_seen_at,m.last_seen_at,p.published_at
    FROM published p JOIN catalog_search_memberships m ON m.feed=p.feed
    WHERE NOT EXISTS(SELECT 1 FROM observed o WHERE o.steam_app_id=m.steam_app_id)
    ON CONFLICT(feed,steam_app_id) DO UPDATE SET
        published_run_revision=EXCLUDED.published_run_revision,active=EXCLUDED.active,
        omission_count=EXCLUDED.omission_count,last_seen_at=EXCLUDED.last_seen_at,latest_complete_at=EXCLUDED.latest_complete_at
    WHERE EXCLUDED.published_run_revision>catalog_search_memberships.published_run_revision
    RETURNING steam_app_id
)
SELECT EXISTS(SELECT 1 FROM published) AS published;

-- name: InsertSearchObservation :execrows
INSERT INTO catalog_search_observations(run_id,feed,start_offset,page_generation,steam_app_id,run_revision,observation_ordinal,
    position,release_text,precision,fingerprint,review_band,review_label,positive_percent,review_total,observed_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT DO NOTHING;

-- name: UpsertSearchWorklist :execrows
INSERT INTO catalog_search_worklist(run_id,steam_app_id,run_revision,observation_ordinal,priority)
VALUES($1,$2,$3,$4,$5)
ON CONFLICT(run_id,steam_app_id) DO UPDATE SET run_revision=EXCLUDED.run_revision,
    observation_ordinal=EXCLUDED.observation_ordinal,priority=GREATEST(catalog_search_worklist.priority,EXCLUDED.priority)
WHERE (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>
    (catalog_search_worklist.run_revision,catalog_search_worklist.observation_ordinal);

-- name: UpsertCurrentSearchObservation :execrows
INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,release_text,precision,
    release_date,release_year,release_month,release_quarter,fingerprint,
    review_band,review_label,positive_percent,review_total,review_observed_at,last_seen_at,priority)
VALUES(sqlc.arg(steam_app_id),sqlc.arg(run_revision),sqlc.arg(observation_ordinal),sqlc.arg(release_text),
    sqlc.arg(precision),
    CASE WHEN sqlc.arg(precision)='day' THEN sqlc.narg(release_date)::date END,
    CASE WHEN sqlc.arg(precision) IN ('day','month','quarter','year') THEN sqlc.narg(release_year)::integer END,
    CASE WHEN sqlc.arg(precision) IN ('day','month') THEN sqlc.narg(release_month)::integer END,
    CASE WHEN sqlc.arg(precision)='quarter' THEN sqlc.narg(release_quarter)::integer END,
    sqlc.arg(fingerprint),sqlc.arg(review_band),sqlc.arg(review_label),
    sqlc.narg(positive_percent),sqlc.arg(review_total),
    CASE WHEN sqlc.arg(review_band)='unknown' THEN NULL ELSE sqlc.arg(review_observed_at)::timestamptz END,
    sqlc.arg(review_observed_at),sqlc.arg(priority))
ON CONFLICT(steam_app_id) DO UPDATE SET run_revision=EXCLUDED.run_revision,observation_ordinal=EXCLUDED.observation_ordinal,
    release_text=EXCLUDED.release_text,precision=EXCLUDED.precision,fingerprint=EXCLUDED.fingerprint,
    release_date=EXCLUDED.release_date,release_year=EXCLUDED.release_year,
    release_month=EXCLUDED.release_month,release_quarter=EXCLUDED.release_quarter,
    review_band=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_band ELSE EXCLUDED.review_band END,
    review_label=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_label ELSE EXCLUDED.review_label END,
    positive_percent=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.positive_percent ELSE EXCLUDED.positive_percent END,
    review_total=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_total ELSE EXCLUDED.review_total END,
    review_observed_at=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_observed_at ELSE EXCLUDED.review_observed_at END,
    last_seen_at=EXCLUDED.last_seen_at,priority=GREATEST(catalog_search_current.priority,EXCLUDED.priority)
WHERE (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>
    (catalog_search_current.run_revision,catalog_search_current.observation_ordinal);

-- Atomic UPSERT locks the UTC-hour bucket across app instances, including retries
-- of an already charged identity. A full bucket returns no row without charging.
-- name: ReserveCatalogBudget :one
INSERT INTO catalog_budget_reservations(kind,hour_at,reservations,used)
SELECT sqlc.arg(kind),sqlc.arg(hour_at),jsonb_build_object(sqlc.arg(identity_hash)::text,true),1
WHERE sqlc.arg(budget_limit)::integer BETWEEN 1 AND 100000
    AND char_length(sqlc.arg(identity_hash)::text) BETWEEN 1 AND 128
ON CONFLICT(kind,hour_at) DO UPDATE SET
    reservations=catalog_budget_reservations.reservations||EXCLUDED.reservations,
    used=catalog_budget_reservations.used+CASE WHEN catalog_budget_reservations.reservations ? sqlc.arg(identity_hash)::text THEN 0 ELSE 1 END
WHERE catalog_budget_reservations.reservations ? sqlc.arg(identity_hash)::text
    OR catalog_budget_reservations.used<sqlc.arg(budget_limit)::integer
RETURNING *;

-- name: AcquireStoreReservation :one
INSERT INTO catalog_store_reservations(steam_app_id,id,run_revision,observation_ordinal,fingerprint,fencing_token,lease_until)
SELECT sqlc.arg(steam_app_id),sqlc.arg(id),sqlc.arg(run_revision),sqlc.arg(observation_ordinal),
    sqlc.arg(fingerprint),sqlc.arg(fencing_token),sqlc.arg(lease_until)
WHERE NOT EXISTS(SELECT 1 FROM catalog_search_current c WHERE c.steam_app_id=sqlc.arg(steam_app_id)
    AND (c.run_revision,c.observation_ordinal)>(sqlc.arg(run_revision)::bigint,sqlc.arg(observation_ordinal)::bigint))
ON CONFLICT(steam_app_id) DO UPDATE SET id=EXCLUDED.id,run_revision=EXCLUDED.run_revision,
    observation_ordinal=EXCLUDED.observation_ordinal,fingerprint=EXCLUDED.fingerprint,
    fencing_token=EXCLUDED.fencing_token,lease_until=EXCLUDED.lease_until,status='reserved',updated_at=sqlc.arg(now_at)
WHERE (catalog_store_reservations.status<>'reserved' OR catalog_store_reservations.lease_until<=sqlc.arg(now_at))
    AND (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_store_reservations.run_revision,catalog_store_reservations.observation_ordinal)
RETURNING *;

-- name: ReleaseStoreReservation :execrows
UPDATE catalog_store_reservations SET status='released',updated_at=sqlc.arg(now_at)
WHERE steam_app_id=sqlc.arg(steam_app_id) AND id=sqlc.arg(id) AND fencing_token=sqlc.arg(fencing_token)
    AND status='reserved' AND lease_until>sqlc.arg(now_at);

-- name: ListDueEligibilityChanges :many
SELECT * FROM catalog_search_current
WHERE next_eligibility_at<=sqlc.arg(due_at)
ORDER BY next_eligibility_at,steam_app_id LIMIT sqlc.arg(result_limit);

-- name: GetCatalogWorkflowState :one
SELECT * FROM catalog_workflow_state WHERE singleton;

-- Must execute inside the legacy enqueue transaction. Holding this row lock
-- serializes that producer with the final cutover transaction.
-- name: LockCatalogWorkflowState :one
SELECT * FROM catalog_workflow_state WHERE singleton FOR UPDATE;

-- name: CloseLegacyCatalogWorkflow :execrows
UPDATE catalog_workflow_state SET legacy_enqueue_enabled=false,legacy_cutover_at=sqlc.arg(cutover_at)
WHERE singleton AND legacy_enqueue_enabled;
