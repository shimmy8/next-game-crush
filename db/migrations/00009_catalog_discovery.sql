-- +goose Up
ALTER TABLE catalog_entries ALTER COLUMN origin DROP NOT NULL;

CREATE TABLE catalog_entry_sources (
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    source text NOT NULL CHECK (source IN ('seed','steam_discovery')),
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    active boolean NOT NULL DEFAULT true,
    PRIMARY KEY(game_id,source)
);
INSERT INTO catalog_entry_sources(game_id,source,first_seen_at,last_seen_at)
SELECT game_id,'seed',added_at,added_at FROM catalog_entries WHERE origin='seed';
CREATE INDEX catalog_entry_sources_active_idx ON catalog_entry_sources(source,game_id) WHERE active;

CREATE TABLE catalog_discovery_state (
    source text NOT NULL CHECK (source='steam'),
    mode text NOT NULL CHECK (mode IN ('incremental','backfill')),
    cursor_app_id bigint NOT NULL DEFAULT 0 CHECK (cursor_app_id BETWEEN 0 AND 4294967295),
    watermark_at timestamptz,
    scan_started_at timestamptz,
    scan_completed_at timestamptz,
    availability text NOT NULL DEFAULT 'unknown' CHECK (availability IN ('unknown','available','unavailable','request_failed')),
    last_error text NOT NULL DEFAULT '' CHECK (last_error IN ('','unavailable','request_failed','invalid_response','rate_limited','retry_exhausted')),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(source,mode)
);
INSERT INTO catalog_discovery_state(source,mode) VALUES('steam','incremental'),('steam','backfill');

CREATE TABLE catalog_discovery_observations (
    source text NOT NULL CHECK (source='steam'),
    steam_app_id bigint NOT NULL CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    last_modified timestamptz NOT NULL CHECK (last_modified >= '1970-01-01 00:00:01+00' AND last_modified <= '2106-02-07 06:28:15+00'),
    status text NOT NULL CHECK (status IN ('admitted','outside_horizon','released','not_game','unavailable','failed')),
    precision text CHECK (precision IN ('day','month','quarter','year','coming_soon','unknown')),
    release_date date CHECK (release_date IS NULL OR (precision IS NOT NULL AND precision='day')),
    raw_release_text text NOT NULL DEFAULT '' CHECK (char_length(raw_release_text)<=512),
    first_observed_at timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempts integer NOT NULL DEFAULT 1 CHECK (attempts>=1),
    retry_generation integer NOT NULL DEFAULT 0 CHECK (retry_generation>=0),
    next_retry_at timestamptz,
    safe_error text NOT NULL DEFAULT '' CHECK (safe_error IN ('','unavailable','request_failed','invalid_response','rate_limited','retry_exhausted')),
    PRIMARY KEY(source,steam_app_id,last_modified),
    CHECK (status='failed' OR (next_retry_at IS NULL AND safe_error=''))
);
CREATE INDEX catalog_discovery_due_idx ON catalog_discovery_observations(next_retry_at,steam_app_id) WHERE status='failed' AND next_retry_at IS NOT NULL;

CREATE TABLE catalog_discovery_budget (
    hour_at timestamptz PRIMARY KEY CHECK (hour_at=date_trunc('hour',hour_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
    reservations jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(reservations)='object' AND octet_length(reservations::text)<=100000),
    used integer NOT NULL DEFAULT 0 CHECK (used BETWEEN 0 AND 1000),
    counts jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(counts)='object'
        AND NOT jsonb_path_exists(counts,'$.* ? (@.type() != "number" || @ < 0 || @.floor() != @)'))
);

CREATE TABLE catalog_rebuild_state (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    dirty_revision bigint NOT NULL DEFAULT 0 CHECK (dirty_revision>=0),
    enqueued_revision bigint NOT NULL DEFAULT 0 CHECK (enqueued_revision>=0 AND enqueued_revision<=dirty_revision),
    dirty_since timestamptz,
    last_enqueued_at timestamptz,
    last_scheduled_at timestamptz,
    pending_job_id uuid REFERENCES sync_jobs(id) ON DELETE SET NULL
);
CREATE TABLE catalog_explanation_cache (
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    model text NOT NULL,
    input_hash text NOT NULL,
    explanation jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(game_id,model,input_hash)
);
ALTER TABLE sync_jobs ADD COLUMN priority smallint NOT NULL DEFAULT 50 CHECK (priority BETWEEN 1 AND 100);
DROP INDEX sync_jobs_claim_idx;
CREATE INDEX sync_jobs_claim_idx ON sync_jobs(priority DESC,available_at,created_at,id) WHERE status IN ('queued','retryable','running');

-- +goose Down
-- Populated installations roll back by disabling discovery and retaining schema.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM catalog_entry_sources WHERE source='steam_discovery')
       OR EXISTS(SELECT 1 FROM catalog_discovery_observations)
       OR EXISTS(SELECT 1 FROM catalog_entries WHERE origin IS NULL)
       OR EXISTS(SELECT 1 FROM catalog_discovery_state WHERE scan_started_at IS NOT NULL OR watermark_at IS NOT NULL OR cursor_app_id<>0)
    THEN RAISE EXCEPTION 'populated catalog discovery cannot be rolled back; disable discovery instead'; END IF;
END $$;
-- +goose StatementEnd
DROP INDEX sync_jobs_claim_idx;
ALTER TABLE sync_jobs DROP COLUMN priority;
CREATE INDEX sync_jobs_claim_idx ON sync_jobs(available_at,created_at) WHERE status IN ('queued','retryable','running');
DROP TABLE catalog_explanation_cache,catalog_rebuild_state,catalog_discovery_budget,catalog_discovery_observations,catalog_discovery_state,catalog_entry_sources;
ALTER TABLE catalog_entries ALTER COLUMN origin SET NOT NULL;
