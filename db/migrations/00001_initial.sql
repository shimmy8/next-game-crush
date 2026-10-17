-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;

CREATE TABLE users (
    id uuid PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE steam_accounts (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    steam_id text NOT NULL UNIQUE,
    display_name text,
    avatar_url text,
    source text NOT NULL DEFAULT 'steam',
    last_successful_fetch_at timestamptz,
    availability text NOT NULL DEFAULT 'unknown',
    raw_snapshot jsonb
);
CREATE TABLE games (
    id uuid PRIMARY KEY,
    steam_app_id bigint NOT NULL UNIQUE,
    name text NOT NULL,
    source text NOT NULL,
    last_successful_fetch_at timestamptz NOT NULL,
    source_snapshot jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE game_features (
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    model text NOT NULL,
    input_content_hash text NOT NULL,
    features jsonb NOT NULL,
    source text NOT NULL,
    last_successful_fetch_at timestamptz NOT NULL,
    PRIMARY KEY (game_id, model)
);
CREATE TABLE game_embeddings (
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    model text NOT NULL,
    input_content_hash text NOT NULL,
    embedding vector(1536) NOT NULL,
    source text NOT NULL DEFAULT 'openai',
    last_successful_fetch_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (game_id, model)
);
CREATE INDEX game_embeddings_cosine_idx ON game_embeddings USING hnsw (embedding vector_cosine_ops);
CREATE TABLE game_release_dates (
    id uuid PRIMARY KEY,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    precision text NOT NULL CHECK (precision IN ('day','month','quarter','year','coming_soon','unknown')),
    release_date date,
    raw_text text,
    observed_at timestamptz NOT NULL,
    source text NOT NULL,
    last_successful_fetch_at timestamptz NOT NULL
);
CREATE TABLE user_games (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    playtime_minutes bigint NOT NULL DEFAULT 0,
    recently_played_minutes bigint NOT NULL DEFAULT 0,
    source text NOT NULL,
    last_successful_fetch_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, game_id)
);
CREATE TABLE wishlist_entries (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    priority integer,
    source text NOT NULL,
    last_successful_fetch_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, game_id)
);
CREATE TABLE achievement_summaries (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    achieved integer NOT NULL DEFAULT 0,
    total integer NOT NULL DEFAULT 0,
    source text NOT NULL,
    last_successful_fetch_at timestamptz NOT NULL,
    raw_snapshot jsonb,
    PRIMARY KEY (user_id, game_id)
);
CREATE TABLE preference_profiles (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version bigint NOT NULL,
    summary text,
    model text,
    input_content_hash text,
    algorithm_metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, version)
);
CREATE TABLE profile_embeddings (
    profile_id uuid NOT NULL REFERENCES preference_profiles(id) ON DELETE CASCADE,
    polarity text NOT NULL CHECK (polarity IN ('positive','negative')),
    model text NOT NULL,
    input_content_hash text NOT NULL,
    embedding vector(1536) NOT NULL,
    source text NOT NULL DEFAULT 'openai',
    last_successful_fetch_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (profile_id, polarity, model)
);
CREATE TABLE recommendations (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    profile_id uuid NOT NULL REFERENCES preference_profiles(id) ON DELETE CASCADE,
    algorithm_version text NOT NULL,
    score double precision NOT NULL,
    text_model text,
    embedding_model text,
    explanation text,
    explanation_input_content_hash text,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, game_id, profile_id, algorithm_version)
);
CREATE TABLE recommendation_factors (
    recommendation_id uuid PRIMARY KEY REFERENCES recommendations(id) ON DELETE CASCADE,
    factors jsonb NOT NULL,
    weights jsonb NOT NULL
);
CREATE TABLE feedback_events (
    id uuid PRIMARY KEY,
    recommendation_id uuid NOT NULL REFERENCES recommendations(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action text NOT NULL,
    reason text,
    free_text text CHECK (char_length(free_text) <= 500),
    idempotency_key text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE feedback_classifications (
    feedback_id uuid PRIMARY KEY REFERENCES feedback_events(id) ON DELETE CASCADE,
    taxonomy text NOT NULL,
    polarity text NOT NULL,
    confidence double precision NOT NULL,
    actionable boolean NOT NULL,
    model text NOT NULL,
    input_content_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE notification_preferences (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    digest_enabled boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE email_deliveries (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    idempotency_key text NOT NULL UNIQUE,
    status text NOT NULL,
    accepted_at timestamptz,
    error text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sync_jobs (
    id uuid PRIMARY KEY,
    kind text NOT NULL,
    idempotency_key text NOT NULL UNIQUE,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','retryable','succeeded','dead')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts integer NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    available_at timestamptz NOT NULL DEFAULT now(),
    worker_id text,
    fencing_token uuid,
    lease_until timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'running') = (worker_id IS NOT NULL AND fencing_token IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE INDEX sync_jobs_claim_idx ON sync_jobs (available_at, created_at)
    WHERE status IN ('queued', 'retryable', 'running');
CREATE TABLE job_attempts (
    id uuid PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES sync_jobs(id) ON DELETE CASCADE,
    attempt integer NOT NULL,
    worker_id text NOT NULL,
    fencing_token uuid NOT NULL,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    status text NOT NULL,
    error text,
    UNIQUE (job_id, fencing_token)
);

-- +goose Down
DROP TABLE IF EXISTS job_attempts, sync_jobs, email_deliveries, notification_preferences,
    feedback_classifications, feedback_events, recommendation_factors, recommendations,
    profile_embeddings, preference_profiles, achievement_summaries, wishlist_entries,
    user_games, game_release_dates, game_embeddings, game_features, games,
    steam_accounts, sessions, users CASCADE;
