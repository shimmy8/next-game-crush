-- +goose Up
CREATE TABLE game_compatibility (
    game_id uuid PRIMARY KEY REFERENCES games(id) ON DELETE CASCADE,
    windows boolean NOT NULL,
    mac boolean NOT NULL,
    linux boolean NOT NULL,
    platforms_last_successful_fetch_at timestamptz NOT NULL,
    deck_status text NOT NULL DEFAULT 'unknown'
        CHECK (deck_status IN ('verified', 'playable', 'unsupported', 'unknown')),
    deck_availability text NOT NULL DEFAULT 'unknown',
    deck_last_attempt_at timestamptz,
    deck_last_successful_fetch_at timestamptz
);

-- +goose Down
DROP TABLE game_compatibility;
