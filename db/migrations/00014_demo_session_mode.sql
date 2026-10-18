-- +goose Up
ALTER TABLE sessions
    ADD COLUMN access_mode text NOT NULL DEFAULT 'owner',
    ADD CONSTRAINT sessions_access_mode_check CHECK (access_mode IN ('owner', 'demo'));

-- +goose Down
ALTER TABLE sessions
    DROP CONSTRAINT sessions_access_mode_check,
    DROP COLUMN access_mode;
