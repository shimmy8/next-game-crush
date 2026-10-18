-- +goose Up
CREATE TABLE game_modes (
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    mode text NOT NULL CHECK (mode IN ('co-op','competitive-multiplayer','massively-multiplayer','local-multiplayer','online-multiplayer','single-player','multiplayer')),
    source text NOT NULL CHECK (source = 'steam_category'),
    source_content_hash text NOT NULL,
    last_successful_fetch_at timestamptz NOT NULL,
    PRIMARY KEY (game_id, mode, source)
);

ALTER TABLE feedback_events ADD CONSTRAINT feedback_events_id_user_key UNIQUE(id,user_id);

CREATE TABLE preference_adjustments (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_feedback_id uuid NOT NULL UNIQUE,
    source_game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    dimension text NOT NULL CHECK (dimension = 'mode'),
    target text NOT NULL CHECK (target IN ('co-op','competitive-multiplayer','massively-multiplayer','local-multiplayer','online-multiplayer','single-player','multiplayer')),
    direction text NOT NULL CHECK (direction = 'avoid'),
    confidence double precision NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    strength double precision NOT NULL DEFAULT 1 CHECK (strength >= 0 AND strength <= 1),
    active boolean NOT NULL DEFAULT true,
    disable_rebuild_job_id uuid REFERENCES sync_jobs(id),
    disable_previous_ranking_version bigint CHECK (disable_previous_ranking_version >= 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz,
    FOREIGN KEY (source_feedback_id,user_id) REFERENCES feedback_events(id,user_id) ON DELETE CASCADE,
    CONSTRAINT preference_adjustments_disable_state_check CHECK (
        (active AND disabled_at IS NULL AND disable_rebuild_job_id IS NULL AND disable_previous_ranking_version IS NULL)
        OR (NOT active AND disabled_at IS NOT NULL AND disable_rebuild_job_id IS NOT NULL AND disable_previous_ranking_version IS NOT NULL)
    )
);
CREATE INDEX preference_adjustments_user_active_target_idx ON preference_adjustments(user_id,active,target);

ALTER TABLE feedback_classifications
    ADD COLUMN preference_dimension text,
    ADD COLUMN preference_target text,
    ADD COLUMN preference_direction text,
    ADD CONSTRAINT feedback_classifications_preference_check CHECK (
        (preference_dimension IS NULL AND preference_target IS NULL AND preference_direction IS NULL)
        OR (preference_dimension IS NOT NULL AND preference_target IS NOT NULL AND preference_direction IS NOT NULL
            AND preference_dimension = 'mode' AND preference_direction = 'avoid'
            AND preference_target IN ('co-op','competitive-multiplayer','massively-multiplayer','local-multiplayer','online-multiplayer','single-player','multiplayer')
            AND actionable AND polarity = 'negative' AND confidence >= 0.70
            AND ((taxonomy = 'single-player' AND preference_target = 'single-player')
                OR (taxonomy = 'multiplayer' AND preference_target <> 'single-player')))
    );

-- +goose Down
ALTER TABLE feedback_classifications DROP CONSTRAINT feedback_classifications_preference_check,
    DROP COLUMN preference_dimension, DROP COLUMN preference_target, DROP COLUMN preference_direction;
DROP TABLE preference_adjustments;
ALTER TABLE feedback_events DROP CONSTRAINT feedback_events_id_user_key;
DROP TABLE game_modes;
