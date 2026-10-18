-- +goose Up
-- A selected Deck observation may arrive before any successful platform fetch.
-- Keep unknown platform support and freshness genuinely unknown.
ALTER TABLE game_compatibility
    ALTER COLUMN windows DROP NOT NULL,
    ALTER COLUMN mac DROP NOT NULL,
    ALTER COLUMN linux DROP NOT NULL,
    ALTER COLUMN platforms_last_successful_fetch_at DROP NOT NULL,
    ADD CONSTRAINT game_compatibility_platform_observation CHECK (
        (windows IS NULL AND mac IS NULL AND linux IS NULL AND platforms_last_successful_fetch_at IS NULL)
        OR
        (windows IS NOT NULL AND mac IS NOT NULL AND linux IS NOT NULL AND platforms_last_successful_fetch_at IS NOT NULL)
    );

-- +goose Down
-- Refuse rollback while Deck-only observations exist rather than delete them
-- or fabricate a platform observation to satisfy the previous schema.
ALTER TABLE game_compatibility
    DROP CONSTRAINT game_compatibility_platform_observation,
    ALTER COLUMN windows SET NOT NULL,
    ALTER COLUMN mac SET NOT NULL,
    ALTER COLUMN linux SET NOT NULL,
    ALTER COLUMN platforms_last_successful_fetch_at SET NOT NULL;
