-- +goose Up
ALTER TABLE sync_jobs ADD COLUMN lane text NOT NULL DEFAULT 'interactive'
    CHECK (lane IN ('interactive','catalog'));
CREATE INDEX sync_jobs_lane_claim_idx ON sync_jobs(lane,priority DESC,available_at,created_at,id)
    WHERE status IN ('queued','retryable','running');

-- Include descendants, so a late optional child cannot promote a retired parent.
WITH RECURSIVE legacy AS (
    SELECT id FROM sync_jobs WHERE kind IN ('catalog.discovery.tick','catalog.discover.incremental',
        'catalog.discover.backfill','catalog.qualify','catalog.rebuild_wakeup')
        AND status IN ('queued','running','waiting','retryable')
    UNION
    SELECT child.id FROM sync_jobs child JOIN legacy parent ON child.parent_job_id=parent.id
), retired AS (
    UPDATE sync_jobs SET lane='catalog',status='dead',worker_id=NULL,fencing_token=NULL,
        lease_until=NULL,last_error='superseded_catalog_workflow',updated_at=now()
    WHERE id IN (SELECT id FROM legacy) AND status IN ('queued','running','waiting','retryable')
    RETURNING id
)
UPDATE job_attempts SET status='dead',finished_at=now(),error='superseded_catalog_workflow'
WHERE job_id IN (SELECT id FROM retired) AND finished_at IS NULL;

CREATE TABLE catalog_workflow_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    legacy_enqueue_enabled boolean NOT NULL DEFAULT true,
    legacy_cutover_at timestamptz
);
INSERT INTO catalog_workflow_state(singleton) VALUES(true);

CREATE TABLE catalog_search_runs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_revision bigint GENERATED ALWAYS AS IDENTITY NOT NULL UNIQUE CHECK (run_revision>0),
    schedule_name text NOT NULL CHECK (schedule_name IN ('daily','manual','initial','reconciliation')),
    occurrence_at timestamptz NOT NULL,
    kind text NOT NULL CHECK (kind IN ('scheduled','manual','initial','reconciliation')),
    config_snapshot jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config_snapshot)='object'),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','complete','partial','failed')),
    retry_generation integer NOT NULL DEFAULT 0 CHECK (retry_generation>=0),
    next_retry_at timestamptz,
    observation_ordinal bigint NOT NULL DEFAULT 0 CHECK (observation_ordinal>=0),
    new_app_count integer NOT NULL DEFAULT 0 CHECK (new_app_count>=0),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    last_successful_at timestamptz,
    safe_error text NOT NULL DEFAULT '' CHECK (safe_error IN ('','unavailable','request_failed','invalid_response','rate_limited','retry_exhausted','malformed')),
    UNIQUE(schedule_name,occurrence_at), UNIQUE(id,run_revision)
);
CREATE TABLE catalog_search_feeds (
    run_id uuid NOT NULL REFERENCES catalog_search_runs(id),
    feed text NOT NULL CHECK (feed IN ('popular_upcoming','popular_new','recent','coming_soon')),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','complete','partial','failed')),
    cursor_offset integer NOT NULL DEFAULT 0 CHECK (cursor_offset>=0),
    page_generation integer NOT NULL DEFAULT 0 CHECK (page_generation>=0),
    first_page_at timestamptz, last_page_at timestamptz, completed_at timestamptz,
    pages integer NOT NULL DEFAULT 0 CHECK (pages>=0),
    observed_count integer NOT NULL DEFAULT 0 CHECK (observed_count>=0),
    natural_end boolean NOT NULL DEFAULT false,
    monotonic boolean NOT NULL DEFAULT true,
    boundary_stop boolean NOT NULL DEFAULT false,
    last_ordering_date date,
    safety_cap_reason text NOT NULL DEFAULT '' CHECK (safety_cap_reason IN ('','page_limit','new_app_limit')),
    safe_error text NOT NULL DEFAULT '' CHECK (safe_error IN ('','unavailable','request_failed','invalid_response','rate_limited','retry_exhausted','malformed')),
    PRIMARY KEY(run_id,feed),
    CHECK (status<>'complete' OR (safety_cap_reason='' AND
        ((feed IN ('popular_upcoming','popular_new') AND natural_end AND NOT boundary_stop)
        OR (feed IN ('recent','coming_soon') AND (natural_end OR (boundary_stop AND monotonic))))))
);
CREATE TABLE catalog_search_pages (
    run_id uuid NOT NULL,
    feed text NOT NULL,
    start_offset integer NOT NULL CHECK (start_offset>=0),
    page_generation integer NOT NULL CHECK (page_generation>=0),
    observation_ordinal bigint NOT NULL CHECK (observation_ordinal>0),
    next_offset integer NOT NULL DEFAULT 0 CHECK (next_offset>=0),
    result_count integer NOT NULL DEFAULT 0 CHECK (result_count>=0),
    has_more boolean NOT NULL DEFAULT false,
    committed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(run_id,feed,start_offset,page_generation),
    FOREIGN KEY(run_id,feed) REFERENCES catalog_search_feeds(run_id,feed),
    UNIQUE(run_id,observation_ordinal),
    CHECK (NOT has_more OR next_offset>start_offset)
);
CREATE TABLE catalog_search_observations (
    run_id uuid NOT NULL,
    feed text NOT NULL,
    start_offset integer NOT NULL,
    page_generation integer NOT NULL,
    steam_app_id bigint NOT NULL CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    run_revision bigint NOT NULL CHECK (run_revision>0),
    observation_ordinal bigint NOT NULL CHECK (observation_ordinal>0),
    position integer NOT NULL DEFAULT 0 CHECK (position>=0),
    release_text text NOT NULL DEFAULT '' CHECK (char_length(release_text)<=512),
    precision text NOT NULL DEFAULT 'unknown' CHECK (precision IN ('day','month','quarter','year','coming_soon','unknown')),
    fingerprint text NOT NULL DEFAULT '' CHECK (char_length(fingerprint)<=128),
    review_band text NOT NULL DEFAULT 'unknown' CHECK (review_band IN ('positive','mixed_or_below','unrated','unknown')),
    review_label text NOT NULL DEFAULT '' CHECK (char_length(review_label)<=64),
    positive_percent integer CHECK (positive_percent BETWEEN 0 AND 100),
    review_total bigint NOT NULL DEFAULT 0 CHECK (review_total>=0),
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(run_id,feed,start_offset,page_generation,steam_app_id),
    FOREIGN KEY(run_id,feed,start_offset,page_generation) REFERENCES catalog_search_pages(run_id,feed,start_offset,page_generation),
    FOREIGN KEY(run_id,run_revision) REFERENCES catalog_search_runs(id,run_revision),
    FOREIGN KEY(run_id,observation_ordinal) REFERENCES catalog_search_pages(run_id,observation_ordinal)
);
CREATE TABLE catalog_search_worklist (
    run_id uuid NOT NULL REFERENCES catalog_search_runs(id),
    steam_app_id bigint NOT NULL CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    run_revision bigint NOT NULL CHECK (run_revision>0),
    observation_ordinal bigint NOT NULL CHECK (observation_ordinal>0),
    priority smallint NOT NULL DEFAULT 50 CHECK (priority BETWEEN 1 AND 100),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','reserved','complete','deferred','failed','superseded')),
    next_retry_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(run_id,steam_app_id),
    FOREIGN KEY(run_id,run_revision) REFERENCES catalog_search_runs(id,run_revision)
);
CREATE TABLE catalog_search_feed_publications (
    feed text PRIMARY KEY CHECK (feed IN ('popular_upcoming','popular_new','recent','coming_soon')),
    published_run_revision bigint NOT NULL DEFAULT 0 CHECK (published_run_revision>=0),
    published_at timestamptz
);
INSERT INTO catalog_search_feed_publications(feed) VALUES('popular_upcoming'),('popular_new'),('recent'),('coming_soon');
CREATE TABLE catalog_search_memberships (
    feed text NOT NULL REFERENCES catalog_search_feed_publications(feed),
    steam_app_id bigint NOT NULL CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    published_run_revision bigint NOT NULL CHECK (published_run_revision>0),
    active boolean NOT NULL DEFAULT true,
    omission_count integer NOT NULL DEFAULT 0 CHECK (omission_count>=0),
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    latest_complete_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(feed,steam_app_id)
);
CREATE INDEX catalog_search_memberships_active_idx ON catalog_search_memberships(steam_app_id,feed) WHERE active;
CREATE TABLE catalog_search_current (
    steam_app_id bigint PRIMARY KEY CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    run_revision bigint NOT NULL CHECK (run_revision>0),
    observation_ordinal bigint NOT NULL CHECK (observation_ordinal>0),
    first_seen_at timestamptz NOT NULL DEFAULT now(), last_seen_at timestamptz NOT NULL DEFAULT now(),
    priority smallint NOT NULL DEFAULT 50 CHECK (priority BETWEEN 1 AND 100),
    release_text text NOT NULL DEFAULT '' CHECK (char_length(release_text)<=512),
    precision text NOT NULL DEFAULT 'unknown' CHECK (precision IN ('day','month','quarter','year','coming_soon','unknown')),
    release_date date, release_year integer CHECK (release_year BETWEEN 1970 AND 9999),
    release_month integer CHECK (release_month BETWEEN 1 AND 12), release_quarter integer CHECK (release_quarter BETWEEN 1 AND 4),
    coming_soon boolean,
    fingerprint text NOT NULL DEFAULT '' CHECK (char_length(fingerprint)<=128),
    review_band text NOT NULL DEFAULT 'unknown' CHECK (review_band IN ('positive','mixed_or_below','unrated','unknown')),
    review_label text NOT NULL DEFAULT '' CHECK (char_length(review_label)<=64),
    positive_percent integer CHECK (positive_percent BETWEEN 0 AND 100),
    review_total bigint NOT NULL DEFAULT 0 CHECK (review_total>=0),
    review_observed_at timestamptz,
    last_successful_appdetails_at timestamptz, next_refresh_at timestamptz,
    semantic_content_hash text NOT NULL DEFAULT '' CHECK (char_length(semantic_content_hash)<=128),
    active boolean NOT NULL DEFAULT false,
    tombstone_reason text NOT NULL DEFAULT '' CHECK (tombstone_reason IN ('','outside_window','mixed_or_below','unrated_grace_expired','unrated_not_popular_new','review_unknown','not_game','unavailable','malformed')),
    retry_generation integer NOT NULL DEFAULT 0 CHECK (retry_generation>=0),
    retry_attempts integer NOT NULL DEFAULT 0 CHECK (retry_attempts>=0),
    next_retry_at timestamptz, next_eligibility_at timestamptz,
    safe_error text NOT NULL DEFAULT '' CHECK (safe_error IN ('','unavailable','request_failed','invalid_response','rate_limited','retry_exhausted','malformed')),
    CHECK (release_date IS NULL OR precision='day')
);
CREATE INDEX catalog_search_due_eligibility_idx ON catalog_search_current(next_eligibility_at,steam_app_id) WHERE next_eligibility_at IS NOT NULL;
CREATE INDEX catalog_search_due_retry_idx ON catalog_search_current(next_retry_at,steam_app_id) WHERE next_retry_at IS NOT NULL;
CREATE TABLE catalog_budget_reservations (
    kind text NOT NULL CHECK (kind IN ('appdetails','ai')),
    hour_at timestamptz NOT NULL CHECK (hour_at=date_trunc('hour',hour_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
    reservations jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(reservations)='object'),
    used integer NOT NULL DEFAULT 0 CHECK (used BETWEEN 0 AND 100000),
    PRIMARY KEY(kind,hour_at)
);
CREATE TABLE catalog_host_throttles (
    host text PRIMARY KEY CHECK (host IN ('store.steampowered.com','api.steampowered.com','api.openai.com')),
    concurrency_limit integer NOT NULL DEFAULT 4 CHECK (concurrency_limit BETWEEN 1 AND 16),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures>=0),
    success_count integer NOT NULL DEFAULT 0 CHECK (success_count>=0),
    next_eligible_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE catalog_store_reservations (
    steam_app_id bigint PRIMARY KEY CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    id uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    run_revision bigint NOT NULL CHECK (run_revision>0),
    observation_ordinal bigint NOT NULL CHECK (observation_ordinal>0),
    fingerprint text NOT NULL DEFAULT '' CHECK (char_length(fingerprint)<=128),
    fencing_token uuid NOT NULL DEFAULT gen_random_uuid(),
    status text NOT NULL DEFAULT 'reserved' CHECK (status IN ('reserved','released','complete','failed')),
    lease_until timestamptz NOT NULL,
    retry_generation integer NOT NULL DEFAULT 0 CHECK (retry_generation>=0),
    next_retry_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE catalog_rebuild_state
    ADD COLUMN due_at timestamptz,
    ADD COLUMN retry_generation integer NOT NULL DEFAULT 0 CHECK (retry_generation>=0),
    ADD COLUMN first_dirty_at timestamptz,
    ADD COLUMN last_completed_revision bigint NOT NULL DEFAULT 0 CHECK (last_completed_revision>=0);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM catalog_search_runs) OR EXISTS(SELECT 1 FROM catalog_search_current)
       OR EXISTS(SELECT 1 FROM catalog_budget_reservations) OR EXISTS(SELECT 1 FROM catalog_store_reservations)
       OR EXISTS(SELECT 1 FROM sync_jobs WHERE lane='catalog')
    THEN RAISE EXCEPTION 'populated search catalog cannot be rolled back; disable discovery instead'; END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE catalog_rebuild_state DROP COLUMN due_at,DROP COLUMN retry_generation,DROP COLUMN first_dirty_at,DROP COLUMN last_completed_revision;
DROP TABLE catalog_store_reservations,catalog_host_throttles,catalog_budget_reservations,catalog_search_current,
    catalog_search_memberships,catalog_search_feed_publications,catalog_search_worklist,catalog_search_observations,
    catalog_search_pages,catalog_search_feeds,catalog_search_runs,catalog_workflow_state;
DROP INDEX sync_jobs_lane_claim_idx;
ALTER TABLE sync_jobs DROP COLUMN lane;
