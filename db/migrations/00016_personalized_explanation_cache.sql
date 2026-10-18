-- +goose Up
-- Explanation inputs include user-specific profile and feedback evidence. Purge
-- the old shared cache before making ownership explicit so one user's generated
-- text can never be reused for another user.
TRUNCATE TABLE catalog_explanation_cache;
ALTER TABLE catalog_explanation_cache DROP CONSTRAINT catalog_explanation_cache_pkey;
ALTER TABLE catalog_explanation_cache
    ADD COLUMN user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE catalog_explanation_cache
    ADD PRIMARY KEY(user_id,game_id,model,input_hash);

-- +goose Down
TRUNCATE TABLE catalog_explanation_cache;
ALTER TABLE catalog_explanation_cache DROP CONSTRAINT catalog_explanation_cache_pkey;
ALTER TABLE catalog_explanation_cache DROP COLUMN user_id;
ALTER TABLE catalog_explanation_cache
    ADD PRIMARY KEY(game_id,model,input_hash);
