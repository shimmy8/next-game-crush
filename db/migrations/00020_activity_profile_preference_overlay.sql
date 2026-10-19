-- +goose Up
ALTER TABLE preference_profiles
    ADD COLUMN publication_state text,
    ADD COLUMN published_at timestamptz,
    ADD COLUMN summary_source text;

UPDATE preference_profiles
SET publication_state = CASE WHEN summary IS NOT NULL AND summary !~ '^[[:space:]]*$' THEN 'published' ELSE 'failed' END,
    published_at = CASE WHEN summary IS NOT NULL AND summary !~ '^[[:space:]]*$' THEN created_at ELSE NULL END,
    summary_source = CASE WHEN summary IS NOT NULL AND summary !~ '^[[:space:]]*$' THEN 'legacy' ELSE NULL END;

ALTER TABLE preference_profiles
    ALTER COLUMN publication_state SET DEFAULT 'building',
    ALTER COLUMN publication_state SET NOT NULL,
    ADD CONSTRAINT preference_profiles_publication_state_check
        CHECK (publication_state IN ('building','published','failed','insufficient')),
    ADD CONSTRAINT preference_profiles_summary_source_check
        CHECK (summary_source IS NULL OR summary_source IN ('openai','deterministic','legacy')),
    ADD CONSTRAINT preference_profiles_publication_check CHECK (
        (publication_state = 'published'
            AND summary IS NOT NULL
            AND summary !~ '^[[:space:]]*$'
            AND published_at IS NOT NULL
            AND summary_source IS NOT NULL)
        OR
        (publication_state <> 'published'
            AND published_at IS NULL
            AND summary_source IS NULL)
    );

CREATE TABLE activity_profile_evidence (
    profile_id uuid NOT NULL REFERENCES preference_profiles(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    playtime_minutes bigint NOT NULL DEFAULT 0 CHECK (playtime_minutes >= 0),
    recently_played_minutes bigint NOT NULL DEFAULT 0 CHECK (recently_played_minutes >= 0),
    last_played_at timestamptz,
    achieved integer NOT NULL DEFAULT 0 CHECK (achieved >= 0),
    total integer NOT NULL DEFAULT 0 CHECK (total >= 0 AND achieved <= total),
    completion_ending_count integer NOT NULL DEFAULT 0 CHECK (completion_ending_count >= 0),
    completion_hundred_percent_basis boolean NOT NULL DEFAULT false,
    completion_weight double precision NOT NULL DEFAULT 0 CHECK (completion_weight >= 0 AND completion_weight <= 2.25),
    completion_at timestamptz,
    completion_basis text CHECK (completion_basis IS NULL OR completion_basis IN ('hundred_percent','ending','both')),
    completion_algorithm_version text,
    PRIMARY KEY (profile_id, game_id),
    CHECK ((completion_weight = 0 AND completion_basis IS NULL AND completion_algorithm_version IS NULL)
        OR (completion_weight > 0 AND completion_basis IS NOT NULL AND btrim(completion_algorithm_version) <> ''))
);
CREATE INDEX activity_profile_evidence_game_idx ON activity_profile_evidence(game_id, profile_id);

CREATE TABLE preference_overlays (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version bigint NOT NULL CHECK (version > 0),
    algorithm_version text NOT NULL CHECK (btrim(algorithm_version) <> ''),
    build_key text NOT NULL CHECK (btrim(build_key) <> ''),
    input_content_hash text NOT NULL CHECK (btrim(input_content_hash) <> ''),
    algorithm_metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, version),
    UNIQUE (user_id, build_key)
);
CREATE INDEX preference_overlays_user_created_idx ON preference_overlays(user_id, created_at DESC, id);

CREATE TABLE preference_overlay_embeddings (
    overlay_id uuid NOT NULL REFERENCES preference_overlays(id) ON DELETE CASCADE,
    polarity text NOT NULL CHECK (polarity IN ('positive','negative')),
    model text NOT NULL CHECK (btrim(model) <> ''),
    input_content_hash text NOT NULL CHECK (btrim(input_content_hash) <> ''),
    embedding vector(1536) NOT NULL,
    source text NOT NULL DEFAULT 'deterministic' CHECK (btrim(source) <> ''),
    last_successful_fetch_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (overlay_id, polarity, model)
);
CREATE INDEX preference_overlay_embeddings_cosine_idx
    ON preference_overlay_embeddings USING hnsw (embedding vector_cosine_ops);

ALTER TABLE recommendation_runs
    ADD COLUMN preference_overlay_id uuid REFERENCES preference_overlays(id) ON DELETE CASCADE;
ALTER TABLE recommendations
    ADD COLUMN preference_overlay_id uuid REFERENCES preference_overlays(id) ON DELETE CASCADE;

-- +goose StatementBegin
CREATE FUNCTION guard_preference_overlay_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=NEW.user_id) THEN
        RAISE EXCEPTION 'user data has been deleted' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION guard_preference_overlay_embedding_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner_id uuid;
BEGIN
    SELECT user_id INTO owner_id FROM preference_overlays WHERE id=NEW.overlay_id;
    IF owner_id IS NOT NULL AND EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=owner_id) THEN
        RAISE EXCEPTION 'user data has been deleted' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER preference_overlays_deletion_guard
    BEFORE INSERT OR UPDATE ON preference_overlays
    FOR EACH ROW EXECUTE FUNCTION guard_preference_overlay_insert();
CREATE TRIGGER preference_overlay_embeddings_deletion_guard
    BEFORE INSERT OR UPDATE ON preference_overlay_embeddings
    FOR EACH ROW EXECUTE FUNCTION guard_preference_overlay_embedding_insert();

-- +goose Down
DROP TRIGGER IF EXISTS preference_overlay_embeddings_deletion_guard ON preference_overlay_embeddings;
DROP TRIGGER IF EXISTS preference_overlays_deletion_guard ON preference_overlays;
DROP FUNCTION IF EXISTS guard_preference_overlay_embedding_insert();
DROP FUNCTION IF EXISTS guard_preference_overlay_insert();
ALTER TABLE recommendations DROP COLUMN preference_overlay_id;
ALTER TABLE recommendation_runs DROP COLUMN preference_overlay_id;
DROP TABLE preference_overlay_embeddings;
DROP TABLE preference_overlays;
DROP TABLE activity_profile_evidence;
ALTER TABLE preference_profiles
    DROP CONSTRAINT preference_profiles_publication_check,
    DROP CONSTRAINT preference_profiles_summary_source_check,
    DROP CONSTRAINT preference_profiles_publication_state_check,
    DROP COLUMN summary_source,
    DROP COLUMN published_at,
    DROP COLUMN publication_state;
