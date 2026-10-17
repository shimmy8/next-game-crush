-- +goose Up
ALTER TABLE feedback_events
    ADD COLUMN recommendation_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN previous_ranking_version bigint NOT NULL DEFAULT 0,
    ADD COLUMN resulting_ranking_version bigint,
    ADD COLUMN rebuild_job_id uuid REFERENCES sync_jobs(id) ON DELETE SET NULL;

ALTER TABLE feedback_classifications
    ADD COLUMN schema_version integer NOT NULL DEFAULT 1,
    ADD CONSTRAINT feedback_classifications_taxonomy_check CHECK (taxonomy IN ('genre','setting','multiplayer','single-player','difficulty','length','early-access','already-known','not-now','other')),
    ADD CONSTRAINT feedback_classifications_polarity_check CHECK (polarity IN ('positive','negative','neutral')),
    ADD CONSTRAINT feedback_classifications_confidence_check CHECK (confidence >= 0 AND confidence <= 1);

CREATE INDEX feedback_events_user_ranking_idx ON feedback_events(user_id, previous_ranking_version, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS feedback_events_user_ranking_idx;
ALTER TABLE feedback_classifications
    DROP CONSTRAINT feedback_classifications_confidence_check,
    DROP CONSTRAINT feedback_classifications_polarity_check,
    DROP CONSTRAINT feedback_classifications_taxonomy_check,
    DROP COLUMN schema_version;
ALTER TABLE feedback_events
    DROP COLUMN rebuild_job_id,
    DROP COLUMN resulting_ranking_version,
    DROP COLUMN previous_ranking_version,
    DROP COLUMN recommendation_snapshot;
