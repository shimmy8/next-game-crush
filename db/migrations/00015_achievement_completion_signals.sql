-- +goose Up
ALTER TABLE achievement_summaries
    ADD COLUMN snapshot_generation bigint NOT NULL DEFAULT 0
        CHECK (snapshot_generation >= 0);

CREATE TABLE game_achievement_definitions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    api_name text NOT NULL CHECK (btrim(api_name) <> ''),
    display_name text CHECK (display_name IS NULL OR btrim(display_name) <> ''),
    description text CHECK (description IS NULL OR btrim(description) <> ''),
    metadata_source text NOT NULL
        CHECK (metadata_source IN ('player_achievements','game_schema')),
    definition_content_hash text NOT NULL CHECK (btrim(definition_content_hash) <> ''),
    last_successful_fetch_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (game_id, api_name)
);

CREATE TABLE user_achievement_unlocks (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    api_name text NOT NULL CHECK (btrim(api_name) <> ''),
    present_in_latest boolean NOT NULL,
    achieved_in_latest boolean NOT NULL,
    ever_achieved boolean NOT NULL,
    unlocked_at timestamptz,
    snapshot_generation bigint NOT NULL CHECK (snapshot_generation >= 1),
    source text NOT NULL CHECK (source = 'player_achievements'),
    last_successful_fetch_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, game_id, api_name),
    CHECK (NOT achieved_in_latest OR (present_in_latest AND ever_achieved))
);
CREATE INDEX user_achievement_unlocks_game_user_idx
    ON user_achievement_unlocks(game_id, user_id);

CREATE TABLE achievement_semantic_classification_generations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    model text NOT NULL CHECK (btrim(model) <> ''),
    prompt_version text NOT NULL CHECK (btrim(prompt_version) <> ''),
    schema_version text NOT NULL CHECK (btrim(schema_version) <> ''),
    definition_set_hash text NOT NULL CHECK (btrim(definition_set_hash) <> ''),
    lexical_rule_version text NOT NULL CHECK (btrim(lexical_rule_version) <> ''),
    status text NOT NULL CHECK (status IN ('staged','active','superseded')),
    activated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, game_id),
    UNIQUE (id, game_id, model, prompt_version, schema_version, lexical_rule_version),
    UNIQUE (game_id, model, prompt_version, schema_version, lexical_rule_version, definition_set_hash),
    CHECK ((status = 'staged' AND activated_at IS NULL) OR
           (status IN ('active','superseded') AND activated_at IS NOT NULL))
);
CREATE UNIQUE INDEX achievement_semantic_one_active_generation_idx
    ON achievement_semantic_classification_generations(
        game_id, model, prompt_version, schema_version, lexical_rule_version
    ) WHERE status = 'active';

CREATE TABLE achievement_semantic_classifications (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    generation_id uuid NOT NULL,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    api_name text NOT NULL CHECK (btrim(api_name) <> ''),
    definition_content_hash text NOT NULL CHECK (btrim(definition_content_hash) <> ''),
    class text NOT NULL CHECK (class IN (
        'base_game_ending','base_game_completion','dlc_ending_or_completion',
        'meta_ending_collection','partial_progress','optional_challenge','other','unknown'
    )),
    model text NOT NULL CHECK (btrim(model) <> ''),
    prompt_version text NOT NULL CHECK (btrim(prompt_version) <> ''),
    schema_version text NOT NULL CHECK (btrim(schema_version) <> ''),
    input_content_hash text NOT NULL CHECK (btrim(input_content_hash) <> ''),
    classification_source text NOT NULL
        CHECK (classification_source IN ('reused','lexical','openai','unknown')),
    lexical_rule_version text NOT NULL CHECK (btrim(lexical_rule_version) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (
        generation_id, game_id, model, prompt_version, schema_version,
        lexical_rule_version
    ) REFERENCES achievement_semantic_classification_generations(
        id, game_id, model, prompt_version, schema_version, lexical_rule_version
    )
        ON DELETE CASCADE,
    UNIQUE (id, game_id),
    UNIQUE (id, game_id, api_name, definition_content_hash),
    UNIQUE (generation_id, api_name)
);
CREATE INDEX achievement_semantic_classification_reuse_idx
    ON achievement_semantic_classifications(
        game_id, definition_content_hash, model, prompt_version, schema_version,
        lexical_rule_version, created_at DESC, id
    );

CREATE TABLE user_game_completion_evidence (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('hundred_percent','ending_marker')),
    api_name text,
    definition_content_hash text,
    semantic_classification_id uuid,
    unlocked_at timestamptz,
    observed_at timestamptz NOT NULL,
    algorithm_version text NOT NULL CHECK (btrim(algorithm_version) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (
        semantic_classification_id, game_id, api_name, definition_content_hash
    ) REFERENCES achievement_semantic_classifications(
        id, game_id, api_name, definition_content_hash
    ) ON DELETE CASCADE,
    CHECK (
        (kind = 'hundred_percent' AND api_name IS NULL
            AND definition_content_hash IS NULL AND semantic_classification_id IS NULL)
        OR
        (kind = 'ending_marker' AND btrim(api_name) <> ''
            AND btrim(definition_content_hash) <> '' AND semantic_classification_id IS NOT NULL)
    )
);
CREATE UNIQUE INDEX user_game_completion_hundred_percent_idx
    ON user_game_completion_evidence(user_id, game_id, kind, algorithm_version)
    WHERE kind = 'hundred_percent';
CREATE UNIQUE INDEX user_game_completion_ending_marker_idx
    ON user_game_completion_evidence(
        user_id, game_id, api_name, semantic_classification_id, algorithm_version
    ) WHERE kind = 'ending_marker';

CREATE TABLE user_game_completion_projections (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    ending_count integer NOT NULL CHECK (ending_count >= 0),
    hundred_percent_basis boolean NOT NULL,
    weight double precision NOT NULL CHECK (weight > 0 AND weight <= 2.25),
    completion_at timestamptz,
    basis text NOT NULL CHECK (basis IN ('hundred_percent','ending','both')),
    algorithm_version text NOT NULL CHECK (btrim(algorithm_version) <> ''),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, game_id),
    CHECK (
        (basis = 'hundred_percent' AND hundred_percent_basis AND ending_count = 0)
        OR (basis = 'ending' AND NOT hundred_percent_basis AND ending_count > 0)
        OR (basis = 'both' AND hundred_percent_basis AND ending_count > 0)
    )
);
CREATE INDEX user_game_completion_projections_game_user_idx
    ON user_game_completion_projections(game_id, user_id);

INSERT INTO user_game_completion_evidence(
    id, user_id, game_id, kind, observed_at, algorithm_version
)
SELECT md5(s.user_id::text || ':' || s.game_id::text || ':hundred_percent:completion-v1')::uuid,
       s.user_id, s.game_id, 'hundred_percent', s.last_successful_fetch_at, 'completion-v1'
FROM achievement_summaries s
WHERE s.total > 0 AND s.achieved = s.total
ON CONFLICT DO NOTHING;

INSERT INTO user_game_completion_projections(
    user_id, game_id, ending_count, hundred_percent_basis, weight,
    completion_at, basis, algorithm_version, updated_at
)
SELECT s.user_id, s.game_id, 0, true, 1.5, NULL, 'hundred_percent',
       'completion-v1', s.last_successful_fetch_at
FROM achievement_summaries s
WHERE s.total > 0 AND s.achieved = s.total
ON CONFLICT (user_id, game_id) DO NOTHING;

-- +goose Down
DROP TABLE user_game_completion_projections;
DROP TABLE user_game_completion_evidence;
DROP TABLE achievement_semantic_classifications;
DROP INDEX IF EXISTS achievement_semantic_one_active_generation_idx;
DROP TABLE achievement_semantic_classification_generations;
DROP TABLE user_achievement_unlocks;
DROP TABLE game_achievement_definitions;
ALTER TABLE achievement_summaries DROP COLUMN snapshot_generation;
