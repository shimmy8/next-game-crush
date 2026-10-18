-- name: CreateOrGetSearchRun :one
INSERT INTO catalog_search_runs(schedule_name,occurrence_at,kind,config_snapshot)
VALUES($1,$2,$3,$4)
ON CONFLICT(schedule_name,occurrence_at) DO UPDATE SET schedule_name=EXCLUDED.schedule_name
RETURNING *;

-- name: SearchRunJobExists :one
SELECT EXISTS(SELECT 1 FROM sync_jobs WHERE idempotency_key=$1)::boolean;

-- name: SearchCatalogNeedsInitial :one
SELECT (NOT EXISTS(SELECT 1 FROM catalog_search_runs
    WHERE status IN ('queued','running','complete') OR last_successful_at IS NOT NULL)
    AND NOT EXISTS(SELECT 1 FROM catalog_search_memberships WHERE active))::boolean AS needs_initial;

-- The workflow singleton lock serializes coordinators; the row lock also fences
-- future page/run completion transactions using the same logical occurrence.
-- name: ListTerminalSearchRuns :many
SELECT r.*,j.updated_at AS job_updated_at FROM catalog_search_runs r
JOIN sync_jobs j ON j.idempotency_key='catalog.search.run:v1:'||r.schedule_name||':'||
    extract(epoch FROM r.occurrence_at)::bigint::text||':retry:'||r.retry_generation::text
WHERE j.status='dead' AND r.status IN ('queued','running','failed','partial')
ORDER BY r.run_revision FOR UPDATE OF r;

-- name: MarkSearchRunFailed :exec
UPDATE catalog_search_runs SET status='failed',safe_error='retry_exhausted',next_retry_at=$2
WHERE id=$1 AND status IN ('queued','running','failed','partial');

-- name: AdvanceSearchRunGeneration :one
UPDATE catalog_search_runs SET retry_generation=retry_generation+1,status='queued',next_retry_at=NULL,
    safe_error='',completed_at=NULL
WHERE id=sqlc.arg(id) AND retry_generation=sqlc.arg(retry_generation)
    AND status='failed' AND next_retry_at<=sqlc.arg(due_at)
RETURNING *;

-- name: AllocateObservationOrdinal :one
UPDATE catalog_search_runs SET observation_ordinal=observation_ordinal+1
WHERE id=$1 RETURNING observation_ordinal;

-- name: MarkSearchRunRunning :execrows
UPDATE catalog_search_runs SET status='running',started_at=COALESCE(started_at,sqlc.arg(start_time))
WHERE id=sqlc.arg(run_id) AND run_revision=sqlc.arg(run_revision)
    AND retry_generation=sqlc.arg(retry_generation) AND status IN ('queued','running');

-- name: PrepareSearchRunFeeds :execrows
INSERT INTO catalog_search_feeds(run_id,feed)
SELECT sqlc.arg(run_id),feed FROM unnest(ARRAY['popular_upcoming','popular_new','recent','coming_soon']) AS feed
ON CONFLICT(run_id,feed) DO NOTHING;

-- name: GetSearchRun :one
SELECT * FROM catalog_search_runs WHERE id=$1;

-- name: GetCommittedSearchPage :one
SELECT * FROM catalog_search_pages
WHERE run_id=$1 AND feed=$2 AND start_offset=$3 AND page_generation=$4;

-- name: LockSearchFeed :one
SELECT * FROM catalog_search_feeds WHERE run_id=$1 AND feed=$2 FOR UPDATE;

-- name: FailSearchFeedAtCursor :execrows
UPDATE catalog_search_feeds SET status='failed',completed_at=sqlc.arg(completed_at),safe_error=sqlc.arg(safe_error)
WHERE run_id=sqlc.arg(run_id) AND feed=sqlc.arg(feed) AND status IN ('queued','running')
    AND cursor_offset=sqlc.arg(expected_offset) AND page_generation=sqlc.arg(page_generation);

-- name: RecordSearchFeedProgress :execrows
UPDATE catalog_search_feeds SET status=sqlc.arg(status),cursor_offset=sqlc.arg(cursor_offset),
    first_page_at=COALESCE(first_page_at,sqlc.arg(observed_at)),last_page_at=sqlc.arg(observed_at),
    completed_at=CASE WHEN sqlc.arg(status) IN ('complete','partial') THEN sqlc.arg(observed_at) ELSE NULL END,
    pages=pages+1,observed_count=observed_count+sqlc.arg(observed_count),
    natural_end=sqlc.arg(natural_end),monotonic=monotonic AND sqlc.arg(monotonic),
    boundary_stop=sqlc.arg(boundary_stop),last_ordering_date=sqlc.narg(last_ordering_date),
    safety_cap_reason=sqlc.arg(safety_cap_reason),safe_error=''
WHERE run_id=sqlc.arg(run_id) AND feed=sqlc.arg(feed)
    AND cursor_offset=sqlc.arg(expected_offset) AND page_generation=sqlc.arg(page_generation);

-- name: CountSearchRunWorklist :one
SELECT count(*)::integer FROM catalog_search_worklist WHERE run_id=$1;

-- name: SearchWorklistContains :one
SELECT EXISTS(SELECT 1 FROM catalog_search_worklist WHERE run_id=$1 AND steam_app_id=$2)::boolean;

-- name: UpdateSearchRunNewAppCount :exec
UPDATE catalog_search_runs SET new_app_count=$2 WHERE id=$1;

-- name: FinishSearchRunIfTerminal :execrows
UPDATE catalog_search_runs r SET
    status=CASE
        WHEN EXISTS(SELECT 1 FROM catalog_search_feeds f WHERE f.run_id=r.id AND f.status='failed') THEN 'failed'
        WHEN EXISTS(SELECT 1 FROM catalog_search_feeds f WHERE f.run_id=r.id AND f.status='partial') THEN 'partial'
        ELSE 'complete' END,
    completed_at=sqlc.arg(completed_at),
    last_successful_at=CASE WHEN NOT EXISTS(SELECT 1 FROM catalog_search_feeds f WHERE f.run_id=r.id AND f.status IN ('partial','failed')) THEN sqlc.arg(completed_at) ELSE last_successful_at END
WHERE r.id=sqlc.arg(run_id) AND r.status IN ('queued','running')
    AND NOT EXISTS(SELECT 1 FROM catalog_search_feeds f WHERE f.run_id=r.id AND f.status NOT IN ('complete','partial','failed'));

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

-- name: UpsertSearchWorklist :one
WITH inserted AS (
    INSERT INTO catalog_search_worklist(run_id,steam_app_id,run_revision,observation_ordinal,priority,
        material_run_revision,material_observation_ordinal)
    VALUES($1,$2,$3,$4,$5,sqlc.arg(material_run_revision)::bigint,sqlc.arg(material_observation_ordinal)::bigint)
    ON CONFLICT(run_id,steam_app_id) DO NOTHING RETURNING true AS created
), updated AS (
    UPDATE catalog_search_worklist SET run_revision=$3,observation_ordinal=$4,
        priority=GREATEST(priority,$5),material_run_revision=sqlc.arg(material_run_revision)::bigint,
        material_observation_ordinal=sqlc.arg(material_observation_ordinal)::bigint,
        status=CASE WHEN (sqlc.arg(material_run_revision)::bigint,sqlc.arg(material_observation_ordinal)::bigint)>(COALESCE(material_run_revision,run_revision),COALESCE(material_observation_ordinal,observation_ordinal)) THEN 'pending' ELSE status END,
        next_retry_at=CASE WHEN (sqlc.arg(material_run_revision)::bigint,sqlc.arg(material_observation_ordinal)::bigint)>(COALESCE(material_run_revision,run_revision),COALESCE(material_observation_ordinal,observation_ordinal)) THEN NULL ELSE next_retry_at END
    WHERE run_id=$1 AND steam_app_id=$2 AND NOT EXISTS(SELECT 1 FROM inserted)
        AND ($3::bigint,$4::bigint)>(run_revision,observation_ordinal)
    RETURNING false AS created
)
SELECT created FROM inserted UNION ALL SELECT created FROM updated
UNION ALL SELECT false WHERE NOT EXISTS(SELECT 1 FROM inserted) AND NOT EXISTS(SELECT 1 FROM updated);

-- name: UpsertCurrentSearchObservation :execrows
INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,release_text,precision,
    release_date,release_year,release_month,release_quarter,fingerprint,
    review_band,review_label,positive_percent,review_total,review_observed_at,last_seen_at,priority,
    material_run_revision,material_observation_ordinal,material_fingerprint)
VALUES(sqlc.arg(steam_app_id),sqlc.arg(run_revision),sqlc.arg(observation_ordinal),sqlc.arg(release_text),
    sqlc.arg(precision),
    CASE WHEN sqlc.arg(precision)='day' THEN sqlc.narg(release_date)::date END,
    CASE WHEN sqlc.arg(precision) IN ('day','month','quarter','year') THEN sqlc.narg(release_year)::integer END,
    CASE WHEN sqlc.arg(precision) IN ('day','month') THEN sqlc.narg(release_month)::integer END,
    CASE WHEN sqlc.arg(precision)='quarter' THEN sqlc.narg(release_quarter)::integer END,
    sqlc.arg(fingerprint),sqlc.arg(review_band),sqlc.arg(review_label),
    sqlc.narg(positive_percent),sqlc.arg(review_total),
    CASE WHEN sqlc.arg(review_band)='unknown' THEN NULL ELSE sqlc.arg(review_observed_at)::timestamptz END,
    sqlc.arg(review_observed_at),sqlc.arg(priority),sqlc.arg(run_revision),sqlc.arg(observation_ordinal),sqlc.arg(fingerprint))
ON CONFLICT(steam_app_id) DO UPDATE SET run_revision=EXCLUDED.run_revision,observation_ordinal=EXCLUDED.observation_ordinal,
    release_text=EXCLUDED.release_text,precision=EXCLUDED.precision,fingerprint=EXCLUDED.fingerprint,
    release_date=EXCLUDED.release_date,release_year=EXCLUDED.release_year,
    release_month=EXCLUDED.release_month,release_quarter=EXCLUDED.release_quarter,
    review_band=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_band ELSE EXCLUDED.review_band END,
    review_label=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_label ELSE EXCLUDED.review_label END,
    positive_percent=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.positive_percent ELSE EXCLUDED.positive_percent END,
    review_total=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_total ELSE EXCLUDED.review_total END,
    review_observed_at=CASE WHEN EXCLUDED.review_band='unknown' THEN catalog_search_current.review_observed_at ELSE EXCLUDED.review_observed_at END,
    last_seen_at=EXCLUDED.last_seen_at,priority=GREATEST(catalog_search_current.priority,EXCLUDED.priority),
    material_run_revision=CASE WHEN EXCLUDED.fingerprint<>catalog_search_current.fingerprint
        THEN EXCLUDED.run_revision ELSE COALESCE(catalog_search_current.material_run_revision,catalog_search_current.run_revision) END,
    material_observation_ordinal=CASE WHEN EXCLUDED.fingerprint<>catalog_search_current.fingerprint
        THEN EXCLUDED.observation_ordinal ELSE COALESCE(catalog_search_current.material_observation_ordinal,catalog_search_current.observation_ordinal) END,
    material_fingerprint=CASE WHEN EXCLUDED.fingerprint<>catalog_search_current.fingerprint
        THEN EXCLUDED.fingerprint ELSE COALESCE(catalog_search_current.material_fingerprint,catalog_search_current.fingerprint) END
WHERE (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>
    (catalog_search_current.run_revision,catalog_search_current.observation_ordinal);

-- name: GetCurrentMaterialCandidate :one
SELECT material_run_revision,material_observation_ordinal,material_fingerprint
FROM catalog_metadata_heads WHERE steam_app_id=$1;

-- name: SyncCurrentMaterialCandidate :execrows
UPDATE catalog_search_current SET material_run_revision=sqlc.arg(run_revision),
    material_observation_ordinal=sqlc.arg(observation_ordinal),material_fingerprint=sqlc.arg(fingerprint)
WHERE steam_app_id=sqlc.arg(steam_app_id);

-- name: UpsertMetadataCandidateHead :one
INSERT INTO catalog_metadata_heads(steam_app_id,latest_run_revision,latest_observation_ordinal,latest_fingerprint,
    material_run_revision,material_observation_ordinal,material_fingerprint,updated_at)
VALUES(sqlc.arg(steam_app_id),sqlc.arg(run_revision),sqlc.arg(observation_ordinal),sqlc.arg(fingerprint),
    sqlc.arg(run_revision),sqlc.arg(observation_ordinal),sqlc.arg(fingerprint),sqlc.arg(now_at))
ON CONFLICT(steam_app_id) DO UPDATE SET
    latest_run_revision=CASE WHEN (EXCLUDED.latest_run_revision,EXCLUDED.latest_observation_ordinal)>
        (catalog_metadata_heads.latest_run_revision,catalog_metadata_heads.latest_observation_ordinal)
        THEN EXCLUDED.latest_run_revision ELSE catalog_metadata_heads.latest_run_revision END,
    latest_observation_ordinal=CASE WHEN (EXCLUDED.latest_run_revision,EXCLUDED.latest_observation_ordinal)>
        (catalog_metadata_heads.latest_run_revision,catalog_metadata_heads.latest_observation_ordinal)
        THEN EXCLUDED.latest_observation_ordinal ELSE catalog_metadata_heads.latest_observation_ordinal END,
    latest_fingerprint=CASE WHEN (EXCLUDED.latest_run_revision,EXCLUDED.latest_observation_ordinal)>
        (catalog_metadata_heads.latest_run_revision,catalog_metadata_heads.latest_observation_ordinal)
        THEN EXCLUDED.latest_fingerprint ELSE catalog_metadata_heads.latest_fingerprint END,
    material_run_revision=CASE WHEN (EXCLUDED.latest_run_revision,EXCLUDED.latest_observation_ordinal)>
        (catalog_metadata_heads.latest_run_revision,catalog_metadata_heads.latest_observation_ordinal)
        AND EXCLUDED.latest_fingerprint<>catalog_metadata_heads.latest_fingerprint
        THEN EXCLUDED.latest_run_revision ELSE catalog_metadata_heads.material_run_revision END,
    material_observation_ordinal=CASE WHEN (EXCLUDED.latest_run_revision,EXCLUDED.latest_observation_ordinal)>
        (catalog_metadata_heads.latest_run_revision,catalog_metadata_heads.latest_observation_ordinal)
        AND EXCLUDED.latest_fingerprint<>catalog_metadata_heads.latest_fingerprint
        THEN EXCLUDED.latest_observation_ordinal ELSE catalog_metadata_heads.material_observation_ordinal END,
    material_fingerprint=CASE WHEN (EXCLUDED.latest_run_revision,EXCLUDED.latest_observation_ordinal)>
        (catalog_metadata_heads.latest_run_revision,catalog_metadata_heads.latest_observation_ordinal)
        AND EXCLUDED.latest_fingerprint<>catalog_metadata_heads.latest_fingerprint
        THEN EXCLUDED.latest_fingerprint ELSE catalog_metadata_heads.material_fingerprint END,
    updated_at=CASE WHEN (EXCLUDED.latest_run_revision,EXCLUDED.latest_observation_ordinal)>
        (catalog_metadata_heads.latest_run_revision,catalog_metadata_heads.latest_observation_ordinal)
        THEN EXCLUDED.updated_at ELSE catalog_metadata_heads.updated_at END
RETURNING *;

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
WITH locked_current AS MATERIALIZED (
    SELECT h.material_run_revision,h.material_observation_ordinal,c.next_refresh_at
    FROM catalog_metadata_heads h LEFT JOIN catalog_search_current c USING(steam_app_id)
    WHERE h.steam_app_id=sqlc.arg(steam_app_id) FOR UPDATE OF h
)
INSERT INTO catalog_store_reservations(steam_app_id,id,run_revision,observation_ordinal,fingerprint,fencing_token,lease_until)
SELECT sqlc.arg(steam_app_id),sqlc.arg(id),sqlc.arg(run_revision),sqlc.arg(observation_ordinal),
    sqlc.arg(fingerprint),sqlc.arg(fencing_token),sqlc.arg(lease_until)
WHERE NOT EXISTS(SELECT 1 FROM locked_current c
    WHERE (c.material_run_revision,c.material_observation_ordinal)>(sqlc.arg(run_revision)::bigint,sqlc.arg(observation_ordinal)::bigint))
  AND (NOT EXISTS(SELECT 1 FROM catalog_store_reservations r WHERE r.steam_app_id=sqlc.arg(steam_app_id))
       OR EXISTS(SELECT 1 FROM catalog_store_reservations r WHERE r.steam_app_id=sqlc.arg(steam_app_id)
           AND r.fingerprint<>sqlc.arg(fingerprint))
       OR COALESCE((SELECT next_refresh_at<=sqlc.arg(now_at) FROM locked_current LIMIT 1),true))
ON CONFLICT(steam_app_id) DO UPDATE SET id=EXCLUDED.id,run_revision=EXCLUDED.run_revision,
    observation_ordinal=EXCLUDED.observation_ordinal,fingerprint=EXCLUDED.fingerprint,
    fencing_token=EXCLUDED.fencing_token,lease_until=EXCLUDED.lease_until,status='reserved',updated_at=sqlc.arg(now_at)
WHERE (catalog_store_reservations.status<>'reserved' OR catalog_store_reservations.lease_until<=sqlc.arg(now_at)
       OR (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>(catalog_store_reservations.run_revision,catalog_store_reservations.observation_ordinal)
          AND EXCLUDED.fingerprint<>catalog_store_reservations.fingerprint)
    AND (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_store_reservations.run_revision,catalog_store_reservations.observation_ordinal)
RETURNING *;

-- name: GetStoreReservation :one
SELECT * FROM catalog_store_reservations WHERE steam_app_id=$1;

-- Search qualification owns this row together with the per-AppID advisory
-- lock. The id/token and candidate revision form the publication fence.
-- name: LockSearchQualificationReservation :one
SELECT * FROM catalog_store_reservations WHERE steam_app_id=$1 FOR UPDATE;

-- name: CompleteSearchQualificationReservation :execrows
UPDATE catalog_store_reservations SET status='complete',retry_generation=0,next_retry_at=NULL,
  lease_until=sqlc.arg(now_at),updated_at=sqlc.arg(now_at)
WHERE steam_app_id=sqlc.arg(steam_app_id) AND id=sqlc.arg(id)
  AND fencing_token=sqlc.arg(fencing_token) AND status='reserved';

-- name: FailSearchQualificationReservation :execrows
UPDATE catalog_store_reservations SET status='reserved',retry_generation=sqlc.arg(retry_generation),
  next_retry_at=sqlc.arg(next_retry_at),lease_until=sqlc.arg(lease_until),updated_at=sqlc.arg(now_at)
WHERE steam_app_id=sqlc.arg(steam_app_id) AND id=sqlc.arg(id)
  AND fencing_token=sqlc.arg(fencing_token) AND status='reserved';

-- name: CompleteSearchQualificationWork :execrows
UPDATE catalog_search_worklist SET status='complete',next_retry_at=NULL
WHERE steam_app_id=sqlc.arg(steam_app_id)
  AND COALESCE(material_run_revision,run_revision)=sqlc.arg(run_revision)::bigint
  AND COALESCE(material_observation_ordinal,observation_ordinal)=sqlc.arg(observation_ordinal)::bigint
  AND status='reserved';

-- name: FailSearchQualificationWork :execrows
UPDATE catalog_search_worklist SET status='reserved',next_retry_at=sqlc.arg(next_retry_at)
WHERE steam_app_id=sqlc.arg(steam_app_id)
  AND COALESCE(material_run_revision,run_revision)=sqlc.arg(run_revision)::bigint
  AND COALESCE(material_observation_ordinal,observation_ordinal)=sqlc.arg(observation_ordinal)::bigint
  AND status='reserved';

-- name: RecordMetadataProvenance :exec
INSERT INTO catalog_metadata_provenance(steam_app_id,ingress_kind,ingress_identity,run_revision,observation_ordinal,first_seen_at,last_seen_at)
VALUES($1,$2,$3,$4,$5,$6,$6)
ON CONFLICT(steam_app_id,ingress_kind,ingress_identity) DO UPDATE SET
    run_revision=GREATEST(catalog_metadata_provenance.run_revision,EXCLUDED.run_revision),
    observation_ordinal=CASE WHEN EXCLUDED.run_revision>catalog_metadata_provenance.run_revision
        THEN EXCLUDED.observation_ordinal
        WHEN EXCLUDED.run_revision=catalog_metadata_provenance.run_revision
        THEN GREATEST(catalog_metadata_provenance.observation_ordinal,EXCLUDED.observation_ordinal)
        ELSE catalog_metadata_provenance.observation_ordinal END,
    last_seen_at=EXCLUDED.last_seen_at;

-- name: PeekNextMetadataWork :one
SELECT w.run_id,w.steam_app_id,COALESCE(w.material_run_revision,w.run_revision)::bigint AS run_revision,
    COALESCE(w.material_observation_ordinal,w.observation_ordinal)::bigint AS observation_ordinal,w.priority,r.kind,
    c.material_fingerprint::text AS fingerprint
FROM catalog_search_worklist w
JOIN catalog_search_runs r ON r.id=w.run_id
JOIN catalog_metadata_heads c ON c.steam_app_id=w.steam_app_id
WHERE (w.status='pending' OR (w.status='deferred' AND w.next_retry_at<=sqlc.arg(now_at))
    OR (w.status='reserved' AND EXISTS(SELECT 1 FROM catalog_store_reservations sr
        WHERE sr.steam_app_id=w.steam_app_id AND sr.status='reserved'
          AND sr.lease_until<=sqlc.arg(now_at))))
  AND (COALESCE(w.material_run_revision,w.run_revision),COALESCE(w.material_observation_ordinal,w.observation_ordinal))=(c.material_run_revision,c.material_observation_ordinal)
ORDER BY CASE WHEN w.status IN ('deferred','reserved') THEN 0 WHEN r.kind='reconciliation' THEN 2 ELSE 1 END,
    w.priority DESC,w.run_revision DESC,w.observation_ordinal DESC,w.created_at,w.steam_app_id
LIMIT 1;

-- name: PeekNextIndependentMetadataWork :one
SELECT w.run_id,w.steam_app_id,COALESCE(w.material_run_revision,w.run_revision)::bigint AS run_revision,
    COALESCE(w.material_observation_ordinal,w.observation_ordinal)::bigint AS observation_ordinal,w.priority,r.kind,
    c.material_fingerprint::text AS fingerprint
FROM catalog_search_worklist w
JOIN catalog_search_runs r ON r.id=w.run_id
JOIN catalog_metadata_heads h ON h.steam_app_id=w.steam_app_id
JOIN catalog_search_current c ON c.steam_app_id=w.steam_app_id
WHERE (w.status='pending' OR (w.status='deferred' AND w.next_retry_at<=sqlc.arg(now_at))
    OR (w.status='reserved' AND EXISTS(SELECT 1 FROM catalog_store_reservations sr
        WHERE sr.steam_app_id=w.steam_app_id AND sr.status='reserved' AND sr.lease_until<=sqlc.arg(now_at))))
  AND (COALESCE(w.material_run_revision,w.run_revision),COALESCE(w.material_observation_ordinal,w.observation_ordinal))=
      (h.material_run_revision,h.material_observation_ordinal)
  AND c.precision IN ('day','month','quarter')
  AND CASE c.precision
      WHEN 'day' THEN c.release_date
      WHEN 'month' THEN make_date(c.release_year,c.release_month,1)
      ELSE make_date(c.release_year,(c.release_quarter-1)*3+1,1)
      END <= sqlc.arg(window_end)::date
  AND CASE c.precision
      WHEN 'day' THEN c.release_date
      WHEN 'month' THEN (make_date(c.release_year,c.release_month,1)+interval '1 month - 1 day')::date
      ELSE (make_date(c.release_year,(c.release_quarter-1)*3+1,1)+interval '3 months - 1 day')::date
      END >= sqlc.arg(window_start)::date
  AND (CASE c.precision
      WHEN 'day' THEN c.release_date
      WHEN 'month' THEN (make_date(c.release_year,c.release_month,1)+interval '1 month - 1 day')::date
      ELSE (make_date(c.release_year,(c.release_quarter-1)*3+1,1)+interval '3 months - 1 day')::date
      END >= sqlc.arg(today)::date OR c.review_band='positive')
ORDER BY CASE WHEN w.status IN ('deferred','reserved') THEN 0 WHEN r.kind='reconciliation' THEN 2 ELSE 1 END,
    w.priority DESC,w.run_revision DESC,w.observation_ordinal DESC,w.created_at,w.steam_app_id
LIMIT 1;

-- name: ClaimMetadataWork :one
SELECT w.run_id,w.steam_app_id,COALESCE(w.material_run_revision,w.run_revision)::bigint AS run_revision,
    COALESCE(w.material_observation_ordinal,w.observation_ordinal)::bigint AS observation_ordinal,w.priority,r.kind,
    c.material_fingerprint::text AS fingerprint
FROM catalog_search_worklist w
JOIN catalog_search_runs r ON r.id=w.run_id
JOIN catalog_metadata_heads c ON c.steam_app_id=w.steam_app_id
WHERE w.run_id=sqlc.arg(run_id) AND w.steam_app_id=sqlc.arg(steam_app_id)
  AND (w.status='pending' OR (w.status='deferred' AND w.next_retry_at<=sqlc.arg(now_at))
    OR (w.status='reserved' AND EXISTS(SELECT 1 FROM catalog_store_reservations sr
        WHERE sr.steam_app_id=w.steam_app_id AND sr.status='reserved' AND sr.lease_until<=sqlc.arg(now_at))))
  AND (COALESCE(w.material_run_revision,w.run_revision),COALESCE(w.material_observation_ordinal,w.observation_ordinal))=(c.material_run_revision,c.material_observation_ordinal)
FOR UPDATE OF w;

-- name: LockMetadataApp :exec
SELECT pg_advisory_xact_lock(sqlc.arg(steam_app_id));

-- name: LockCurrentMetadataCandidate :one
SELECT * FROM catalog_search_current WHERE steam_app_id=$1 FOR UPDATE;

-- name: LockMetadataCandidateHead :one
SELECT * FROM catalog_metadata_heads WHERE steam_app_id=$1 FOR UPDATE;

-- name: MetadataReservationNeeded :one
SELECT CASE
    WHEN COALESCE((SELECT (c.material_run_revision,c.material_observation_ordinal)>
        (sqlc.arg(run_revision)::bigint,sqlc.arg(observation_ordinal)::bigint)
        FROM catalog_metadata_heads c WHERE c.steam_app_id=sqlc.arg(steam_app_id)),false) THEN false
    WHEN NOT EXISTS(SELECT 1 FROM catalog_store_reservations r WHERE r.steam_app_id=sqlc.arg(steam_app_id)) THEN true
    WHEN COALESCE((SELECT r.fingerprint<>sqlc.arg(candidate_fingerprint)
        FROM catalog_store_reservations r WHERE r.steam_app_id=sqlc.arg(steam_app_id)),false) THEN true
    WHEN COALESCE((SELECT r.status='reserved' AND r.lease_until<=sqlc.arg(now_at)
        FROM catalog_store_reservations r WHERE r.steam_app_id=sqlc.arg(steam_app_id)),false) THEN true
    WHEN COALESCE((SELECT r.status='released' FROM catalog_store_reservations r
        WHERE r.steam_app_id=sqlc.arg(steam_app_id)),false) THEN true
    WHEN COALESCE((SELECT r.run_revision=sqlc.arg(run_revision)::bigint
            AND r.observation_ordinal=sqlc.arg(observation_ordinal)::bigint
            AND r.fingerprint=sqlc.arg(candidate_fingerprint)
        FROM catalog_store_reservations r WHERE r.steam_app_id=sqlc.arg(steam_app_id)),false)
        AND NOT EXISTS(SELECT 1 FROM catalog_search_current c
            WHERE c.steam_app_id=sqlc.arg(steam_app_id) AND c.next_refresh_at<=sqlc.arg(now_at)) THEN false
    WHEN COALESCE((SELECT c.next_refresh_at IS NULL OR c.next_refresh_at<=sqlc.arg(now_at)
        FROM catalog_search_current c WHERE c.steam_app_id=sqlc.arg(steam_app_id)),true)
        AND COALESCE((SELECT r.status<>'reserved' FROM catalog_store_reservations r
            WHERE r.steam_app_id=sqlc.arg(steam_app_id)),false) THEN true
    ELSE false END AS needed;

-- name: UpsertMetadataIngressWork :one
INSERT INTO catalog_metadata_ingress_work(steam_app_id,ingress_kind,ingress_identity,run_revision,observation_ordinal,fingerprint,
    material_run_revision,material_observation_ordinal,material_fingerprint,priority,status,updated_at)
VALUES(sqlc.arg(steam_app_id),sqlc.arg(ingress_kind),sqlc.arg(ingress_identity),sqlc.arg(run_revision),sqlc.arg(observation_ordinal),sqlc.arg(fingerprint),
    sqlc.arg(material_run_revision),sqlc.arg(material_observation_ordinal),sqlc.arg(material_fingerprint),sqlc.arg(priority),'pending',sqlc.arg(updated_at))
ON CONFLICT(steam_app_id,ingress_kind,ingress_identity) DO UPDATE SET
    run_revision=CASE WHEN (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_metadata_ingress_work.run_revision,catalog_metadata_ingress_work.observation_ordinal)
        THEN EXCLUDED.run_revision ELSE catalog_metadata_ingress_work.run_revision END,
    observation_ordinal=CASE WHEN (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_metadata_ingress_work.run_revision,catalog_metadata_ingress_work.observation_ordinal)
        THEN EXCLUDED.observation_ordinal ELSE catalog_metadata_ingress_work.observation_ordinal END,
    fingerprint=CASE WHEN (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_metadata_ingress_work.run_revision,catalog_metadata_ingress_work.observation_ordinal)
        THEN EXCLUDED.fingerprint ELSE catalog_metadata_ingress_work.fingerprint END,
    material_run_revision=CASE WHEN (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_metadata_ingress_work.run_revision,catalog_metadata_ingress_work.observation_ordinal)
        THEN EXCLUDED.material_run_revision ELSE catalog_metadata_ingress_work.material_run_revision END,
    material_observation_ordinal=CASE WHEN (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_metadata_ingress_work.run_revision,catalog_metadata_ingress_work.observation_ordinal)
        THEN EXCLUDED.material_observation_ordinal ELSE catalog_metadata_ingress_work.material_observation_ordinal END,
    material_fingerprint=CASE WHEN (EXCLUDED.run_revision,EXCLUDED.observation_ordinal)>=
        (catalog_metadata_ingress_work.run_revision,catalog_metadata_ingress_work.observation_ordinal)
        THEN EXCLUDED.material_fingerprint ELSE catalog_metadata_ingress_work.material_fingerprint END,
    priority=GREATEST(catalog_metadata_ingress_work.priority,EXCLUDED.priority),
    status=CASE WHEN (EXCLUDED.material_run_revision,EXCLUDED.material_observation_ordinal)>
        (catalog_metadata_ingress_work.material_run_revision,catalog_metadata_ingress_work.material_observation_ordinal) THEN 'pending'
        ELSE catalog_metadata_ingress_work.status END,
    next_eligible_at=CASE WHEN (EXCLUDED.material_run_revision,EXCLUDED.material_observation_ordinal)>
        (catalog_metadata_ingress_work.material_run_revision,catalog_metadata_ingress_work.material_observation_ordinal) THEN NULL
        ELSE catalog_metadata_ingress_work.next_eligible_at END,
    updated_at=EXCLUDED.updated_at
RETURNING *;

-- name: ClaimMetadataIngressWork :one
SELECT w.* FROM catalog_metadata_ingress_work w
JOIN catalog_metadata_heads h USING(steam_app_id)
WHERE w.id=sqlc.arg(id) AND (w.status='pending'
    OR (w.status='deferred' AND w.next_eligible_at<=sqlc.arg(now_at))
    OR (w.status='reserved' AND EXISTS(SELECT 1 FROM catalog_store_reservations r
        WHERE r.steam_app_id=w.steam_app_id AND r.status='reserved'
          AND r.lease_until<=sqlc.arg(now_at))))
  AND (w.material_run_revision,w.material_observation_ordinal,w.material_fingerprint)=
      (h.material_run_revision,h.material_observation_ordinal,h.material_fingerprint)
FOR UPDATE;

-- name: PeekNextMetadataIngressWork :one
SELECT w.* FROM catalog_metadata_ingress_work w
JOIN catalog_metadata_heads h USING(steam_app_id)
WHERE ((w.status='pending' OR (w.status='deferred' AND w.next_eligible_at<=sqlc.arg(now_at))
    OR (w.status='reserved' AND EXISTS(SELECT 1 FROM catalog_store_reservations r
        WHERE r.steam_app_id=w.steam_app_id AND r.status='reserved'
          AND r.lease_until<=sqlc.arg(now_at))))
  AND (w.material_run_revision,w.material_observation_ordinal,w.material_fingerprint)=
      (h.material_run_revision,h.material_observation_ordinal,h.material_fingerprint))
ORDER BY CASE WHEN w.status IN ('deferred','reserved') THEN 0 ELSE 1 END,w.priority DESC,w.created_at
LIMIT 1;

-- name: SetMetadataIngressWorkState :execrows
UPDATE catalog_metadata_ingress_work SET status=sqlc.arg(status),next_eligible_at=sqlc.narg(next_eligible_at),updated_at=sqlc.arg(now_at)
WHERE id=sqlc.arg(id);

-- name: SupersedeStaleMetadataWork :execrows
UPDATE catalog_search_worklist w SET status='superseded',next_retry_at=NULL
FROM catalog_search_current c
WHERE c.steam_app_id=w.steam_app_id AND w.status IN ('pending','deferred')
  AND (COALESCE(w.material_run_revision,w.run_revision),COALESCE(w.material_observation_ordinal,w.observation_ordinal))<(COALESCE(c.material_run_revision,c.run_revision),COALESCE(c.material_observation_ordinal,c.observation_ordinal));

-- name: SupersedeStaleReservedMetadataWork :execrows
UPDATE catalog_search_worklist w SET status='superseded',next_retry_at=NULL
FROM catalog_search_current c
WHERE c.steam_app_id=w.steam_app_id AND w.status='reserved'
  AND (COALESCE(w.material_run_revision,w.run_revision),COALESCE(w.material_observation_ordinal,w.observation_ordinal))<(COALESCE(c.material_run_revision,c.run_revision),COALESCE(c.material_observation_ordinal,c.observation_ordinal));

-- name: SupersedeStaleMetadataWorkForApp :execrows
UPDATE catalog_search_worklist SET status='superseded',next_retry_at=NULL
WHERE steam_app_id=sqlc.arg(steam_app_id) AND status IN ('pending','reserved','deferred')
  AND (COALESCE(material_run_revision,run_revision),COALESCE(material_observation_ordinal,observation_ordinal))<(sqlc.arg(run_revision)::bigint,sqlc.arg(observation_ordinal)::bigint);

-- name: SupersedeStaleMetadataIngressWork :execrows
UPDATE catalog_metadata_ingress_work SET status='superseded',next_eligible_at=NULL,updated_at=sqlc.arg(now_at)
WHERE steam_app_id=sqlc.arg(steam_app_id) AND status IN ('pending','reserved','deferred')
  AND ((material_run_revision,material_observation_ordinal)<(sqlc.arg(run_revision)::bigint,sqlc.arg(observation_ordinal)::bigint)
    OR ((material_run_revision,material_observation_ordinal)=(sqlc.arg(run_revision)::bigint,sqlc.arg(observation_ordinal)::bigint)
      AND material_fingerprint<>sqlc.arg(fingerprint)));

-- name: MarkMetadataWorkReserved :execrows
UPDATE catalog_search_worklist SET status='reserved',next_retry_at=NULL
WHERE run_id=$1 AND steam_app_id=$2 AND status IN ('pending','deferred');

-- name: MarkMetadataWorkSuperseded :execrows
UPDATE catalog_search_worklist SET status='superseded',next_retry_at=NULL
WHERE run_id=$1 AND steam_app_id=$2 AND status IN ('pending','deferred');

-- name: CompleteSearchQualificationCandidate :execrows
UPDATE catalog_search_worklist SET status='complete',next_retry_at=NULL
WHERE run_id=$1 AND steam_app_id=$2 AND status IN ('pending','deferred');

-- name: DeferMetadataWork :execrows
UPDATE catalog_search_worklist SET status='deferred',next_retry_at=sqlc.arg(next_retry_at)
WHERE run_id=sqlc.arg(run_id) AND steam_app_id=sqlc.arg(steam_app_id)
    AND status IN ('pending','reserved','deferred');

-- name: RequeueMetadataWork :execrows
UPDATE catalog_search_worklist SET status='pending',next_retry_at=NULL
WHERE steam_app_id=sqlc.arg(steam_app_id)
  AND COALESCE(material_run_revision,run_revision)=sqlc.arg(run_revision)::bigint
  AND COALESCE(material_observation_ordinal,observation_ordinal)=sqlc.arg(observation_ordinal)::bigint AND status='reserved';

-- name: RequeueMetadataIngressWork :execrows
UPDATE catalog_metadata_ingress_work SET status='pending',next_eligible_at=NULL,updated_at=now()
WHERE steam_app_id=sqlc.arg(steam_app_id)
  AND material_run_revision=sqlc.arg(run_revision) AND material_observation_ordinal=sqlc.arg(observation_ordinal)
  AND material_fingerprint=sqlc.arg(fingerprint) AND status='reserved';

-- name: ReleaseStoreReservation :execrows
UPDATE catalog_store_reservations SET status='released',updated_at=sqlc.arg(now_at)
WHERE steam_app_id=sqlc.arg(steam_app_id) AND id=sqlc.arg(id) AND fencing_token=sqlc.arg(fencing_token)
    AND status='reserved' AND lease_until>sqlc.arg(now_at);

-- name: AttachStoreReservationBudget :execrows
UPDATE catalog_store_reservations SET budget_hour=sqlc.arg(budget_hour),budget_identity_hash=sqlc.arg(identity_hash),updated_at=sqlc.arg(now_at)
WHERE steam_app_id=sqlc.arg(steam_app_id) AND id=sqlc.arg(id) AND fencing_token=sqlc.arg(fencing_token)
  AND status='reserved';

-- name: EnsureCatalogHostThrottle :one
INSERT INTO catalog_host_throttles(host,concurrency_limit)
VALUES(sqlc.arg(host),sqlc.arg(default_limit))
ON CONFLICT(host) DO UPDATE SET concurrency_limit=LEAST(catalog_host_throttles.concurrency_limit,EXCLUDED.concurrency_limit)
RETURNING *;

-- name: AcquireCatalogHostSlot :one
WITH candidate AS (
    SELECT s.host,s.slot FROM catalog_host_throttle_slots s
    JOIN catalog_host_throttles h ON h.host=s.host
    WHERE s.host=sqlc.arg(host) AND s.slot<=h.concurrency_limit
      AND (s.fencing_token IS NULL OR s.lease_until<=sqlc.arg(now_at))
      AND (SELECT count(*) FROM catalog_host_throttle_slots active
           WHERE active.host=s.host AND active.fencing_token IS NOT NULL
             AND active.lease_until>sqlc.arg(now_at))<h.concurrency_limit
    ORDER BY s.slot FOR UPDATE OF s SKIP LOCKED LIMIT 1
)
UPDATE catalog_host_throttle_slots s SET fencing_token=sqlc.arg(fencing_token),
    lease_until=sqlc.arg(lease_until),updated_at=sqlc.arg(now_at)
FROM candidate c WHERE s.host=c.host AND s.slot=c.slot
RETURNING s.*;

-- name: ReleaseCatalogHostSlot :execrows
UPDATE catalog_host_throttle_slots SET fencing_token=NULL,lease_until=NULL,updated_at=sqlc.arg(now_at)
WHERE host=sqlc.arg(host) AND fencing_token=sqlc.arg(fencing_token);

-- name: PruneCatalogBudgets :execrows
DELETE FROM catalog_budget_reservations b WHERE b.hour_at<sqlc.arg(before_at)
  AND NOT EXISTS(SELECT 1 FROM catalog_store_reservations r
      WHERE r.budget_hour=b.hour_at AND (r.status='reserved'
        OR EXISTS(SELECT 1 FROM catalog_search_worklist w WHERE w.steam_app_id=r.steam_app_id
            AND w.status IN ('pending','reserved','deferred'))
        OR EXISTS(SELECT 1 FROM catalog_metadata_ingress_work w WHERE w.steam_app_id=r.steam_app_id
            AND w.status IN ('pending','reserved','deferred'))));

-- name: PruneCatalogReservations :execrows
DELETE FROM catalog_store_reservations r
WHERE r.updated_at<sqlc.arg(before_at) AND r.status<>'reserved'
  AND NOT EXISTS(SELECT 1 FROM catalog_search_current c WHERE c.steam_app_id=r.steam_app_id)
  AND NOT EXISTS(SELECT 1 FROM catalog_search_worklist w WHERE w.steam_app_id=r.steam_app_id
      AND w.status IN ('pending','reserved','deferred'))
  AND NOT EXISTS(SELECT 1 FROM catalog_metadata_ingress_work w WHERE w.steam_app_id=r.steam_app_id
      AND w.status IN ('pending','reserved','deferred'));

-- name: PruneCatalogHostSlots :execrows
UPDATE catalog_host_throttle_slots SET fencing_token=NULL,lease_until=NULL,updated_at=sqlc.arg(now_at)
WHERE fencing_token IS NOT NULL AND lease_until<=sqlc.arg(now_at);

-- name: GetCatalogHostThrottle :one
SELECT * FROM catalog_host_throttles WHERE host=$1;

-- name: LockCatalogHostThrottle :one
SELECT * FROM catalog_host_throttles WHERE host=$1 FOR UPDATE;

-- name: RecordCatalogThrottleFailure :one
INSERT INTO catalog_host_throttles(host,concurrency_limit,consecutive_failures,success_count,next_eligible_at,updated_at)
VALUES(sqlc.arg(host),GREATEST(1,sqlc.arg(default_limit)::integer-sqlc.arg(limit_decrement)::integer),1,0,sqlc.arg(next_eligible_at),sqlc.arg(now_at))
ON CONFLICT(host) DO UPDATE SET
    concurrency_limit=GREATEST(1,catalog_host_throttles.concurrency_limit-sqlc.arg(limit_decrement)::integer),
    consecutive_failures=catalog_host_throttles.consecutive_failures+1,success_count=0,
    next_eligible_at=GREATEST(catalog_host_throttles.next_eligible_at,EXCLUDED.next_eligible_at),updated_at=EXCLUDED.updated_at
RETURNING *;

-- name: RecordCatalogThrottleSuccess :one
UPDATE catalog_host_throttles SET
    concurrency_limit=CASE WHEN (next_eligible_at IS NULL OR next_eligible_at<=sqlc.arg(now_at))
        AND success_count+1>=sqlc.arg(successes_to_recover)::integer
        THEN LEAST(sqlc.arg(max_limit)::integer,concurrency_limit+1) ELSE concurrency_limit END,
    success_count=CASE
        WHEN next_eligible_at>sqlc.arg(now_at) THEN success_count
        WHEN success_count+1>=sqlc.arg(successes_to_recover)::integer THEN 0 ELSE success_count+1 END,
    consecutive_failures=CASE WHEN next_eligible_at>sqlc.arg(now_at) THEN consecutive_failures ELSE 0 END,
    next_eligible_at=CASE WHEN next_eligible_at<=sqlc.arg(now_at) THEN NULL ELSE next_eligible_at END,
    updated_at=sqlc.arg(now_at)
WHERE host=sqlc.arg(host)
RETURNING *;

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
