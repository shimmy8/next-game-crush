-- +goose Up
ALTER TABLE sync_jobs ADD COLUMN result jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE preference_profiles ADD COLUMN build_key text;
CREATE UNIQUE INDEX preference_profiles_build_key_idx ON preference_profiles(user_id,build_key) WHERE build_key IS NOT NULL;
ALTER TABLE recommendations ADD COLUMN ranking_version bigint NOT NULL DEFAULT 1;
ALTER TABLE recommendations ADD COLUMN explanation_evidence jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE recommendations ADD COLUMN explanation_generated boolean NOT NULL DEFAULT false;
-- +goose StatementBegin
DO $$ DECLARE constraint_name text; BEGIN
    SELECT conname INTO constraint_name FROM pg_constraint WHERE conrelid='recommendations'::regclass AND contype='u' LIMIT 1;
    IF constraint_name IS NOT NULL THEN EXECUTE format('ALTER TABLE recommendations DROP CONSTRAINT %I',constraint_name); END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE recommendations ADD CONSTRAINT recommendations_version_key UNIQUE(user_id,game_id,profile_id,algorithm_version,ranking_version);
CREATE TABLE recommendation_runs (
    id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id uuid NOT NULL REFERENCES preference_profiles(id) ON DELETE CASCADE,
    ranking_version bigint NOT NULL, algorithm_version text NOT NULL, build_key text NOT NULL, published boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(user_id,ranking_version), UNIQUE(user_id,build_key)
);

-- +goose Down
ALTER TABLE recommendations DROP CONSTRAINT recommendations_version_key;
DROP TABLE recommendation_runs;
ALTER TABLE recommendations ADD CONSTRAINT recommendations_user_id_game_id_profile_id_algorithm_version_key UNIQUE(user_id,game_id,profile_id,algorithm_version);
ALTER TABLE recommendations DROP COLUMN explanation_generated, DROP COLUMN explanation_evidence, DROP COLUMN ranking_version;
DROP INDEX preference_profiles_build_key_idx;
ALTER TABLE preference_profiles DROP COLUMN build_key;
ALTER TABLE sync_jobs DROP COLUMN result;
