-- +goose Up
ALTER TABLE user_source_states
    ADD COLUMN visibility text CHECK (visibility IN ('public','private','unknown'));

-- +goose Down
ALTER TABLE user_source_states DROP COLUMN visibility;
