-- +goose Up
ALTER TABLE games
    ADD COLUMN availability text NOT NULL DEFAULT 'unknown',
    ADD COLUMN last_attempt_at timestamptz,
    ADD COLUMN content_hash text;

ALTER TABLE user_games ADD COLUMN last_played_at timestamptz;

CREATE TABLE user_source_states (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source text NOT NULL,
    resource_key text NOT NULL DEFAULT '',
    availability text NOT NULL,
    last_attempt_at timestamptz NOT NULL,
    last_successful_fetch_at timestamptz,
    error text,
    PRIMARY KEY (user_id, source, resource_key)
);

CREATE TABLE catalog_fetch_states (
    game_id uuid PRIMARY KEY REFERENCES games(id) ON DELETE CASCADE,
    availability text NOT NULL,
    last_attempt_at timestamptz NOT NULL,
    last_successful_fetch_at timestamptz,
    error text
);

CREATE TABLE catalog_entries (
    game_id uuid PRIMARY KEY REFERENCES games(id) ON DELETE CASCADE,
    origin text NOT NULL CHECK (origin IN ('seed','wishlist')),
    added_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE sync_jobs DROP CONSTRAINT sync_jobs_status_check;
ALTER TABLE sync_jobs
    ADD COLUMN parent_job_id uuid REFERENCES sync_jobs(id) ON DELETE CASCADE,
    ADD COLUMN required boolean NOT NULL DEFAULT true,
    ADD COLUMN stage text NOT NULL DEFAULT '',
    ADD CONSTRAINT sync_jobs_status_check CHECK (status IN ('queued','running','waiting','retryable','succeeded','dead'));
CREATE INDEX sync_jobs_parent_idx ON sync_jobs(parent_job_id);

-- +goose Down
DROP INDEX IF EXISTS sync_jobs_parent_idx;
ALTER TABLE sync_jobs DROP CONSTRAINT sync_jobs_status_check;
ALTER TABLE sync_jobs DROP COLUMN stage, DROP COLUMN required, DROP COLUMN parent_job_id;
ALTER TABLE sync_jobs ADD CONSTRAINT sync_jobs_status_check CHECK (status IN ('queued','running','retryable','succeeded','dead'));
DROP TABLE IF EXISTS catalog_entries, catalog_fetch_states, user_source_states;
ALTER TABLE user_games DROP COLUMN last_played_at;
ALTER TABLE games DROP COLUMN content_hash, DROP COLUMN last_attempt_at, DROP COLUMN availability;
