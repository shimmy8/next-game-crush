-- +goose Up
CREATE TABLE deleted_user_tombstones (
    user_id uuid PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE sync_jobs ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX sync_jobs_user_idx ON sync_jobs(user_id, created_at DESC);

-- Jobs have several versioned payload shapes. Resolve their owner at insertion
-- time so account deletion can atomically fence the complete workflow tree.
-- +goose StatementBegin
CREATE FUNCTION assign_sync_job_user() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE candidate uuid;
BEGIN
    candidate := NEW.user_id;
    IF candidate IS NULL AND NEW.parent_job_id IS NOT NULL THEN
        SELECT user_id INTO candidate FROM sync_jobs WHERE id=NEW.parent_job_id;
    END IF;
    IF candidate IS NULL AND NEW.payload ? 'user_id' THEN
        BEGIN candidate := (NEW.payload->>'user_id')::uuid; EXCEPTION WHEN invalid_text_representation THEN candidate := NULL; END;
    END IF;
    IF candidate IS NULL AND NEW.payload ? 'event_id' THEN
        BEGIN SELECT user_id INTO candidate FROM feedback_events WHERE id=(NEW.payload->>'event_id')::uuid; EXCEPTION WHEN invalid_text_representation THEN candidate := NULL; END;
    END IF;
    IF candidate IS NULL AND NEW.payload ? 'delivery_id' THEN
        BEGIN SELECT user_id INTO candidate FROM email_deliveries WHERE id=(NEW.payload->>'delivery_id')::uuid; EXCEPTION WHEN invalid_text_representation THEN candidate := NULL; END;
    END IF;
    IF candidate IS NULL AND NEW.payload ? 'profile_id' THEN
        BEGIN SELECT user_id INTO candidate FROM preference_profiles WHERE id=(NEW.payload->>'profile_id')::uuid; EXCEPTION WHEN invalid_text_representation THEN candidate := NULL; END;
    END IF;
    IF candidate IS NOT NULL AND EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=candidate) THEN
        RAISE EXCEPTION 'user data has been deleted' USING ERRCODE='55000';
    END IF;
    NEW.user_id := candidate;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER sync_jobs_assign_user BEFORE INSERT OR UPDATE OF parent_job_id,payload,user_id
ON sync_jobs FOR EACH ROW EXECUTE FUNCTION assign_sync_job_user();

UPDATE sync_jobs SET user_id=(payload->>'user_id')::uuid
WHERE payload ? 'user_id' AND (payload->>'user_id') ~ '^[0-9a-fA-F-]{36}$';

UPDATE sync_jobs job SET user_id=event.user_id
FROM feedback_events event
WHERE job.user_id IS NULL AND job.payload ? 'event_id'
  AND (job.payload->>'event_id') ~ '^[0-9a-fA-F-]{36}$'
  AND event.id=(job.payload->>'event_id')::uuid;

UPDATE sync_jobs job SET user_id=delivery.user_id
FROM email_deliveries delivery
WHERE job.user_id IS NULL AND job.payload ? 'delivery_id'
  AND (job.payload->>'delivery_id') ~ '^[0-9a-fA-F-]{36}$'
  AND delivery.id=(job.payload->>'delivery_id')::uuid;

UPDATE sync_jobs job SET user_id=profile.user_id
FROM preference_profiles profile
WHERE job.user_id IS NULL AND job.payload ? 'profile_id'
  AND (job.payload->>'profile_id') ~ '^[0-9a-fA-F-]{36}$'
  AND profile.id=(job.payload->>'profile_id')::uuid;

-- +goose StatementBegin
DO $$
DECLARE changed integer := 1;
BEGIN
    WHILE changed > 0 LOOP
        UPDATE sync_jobs child SET user_id=parent.user_id
        FROM sync_jobs parent
        WHERE child.parent_job_id=parent.id AND child.user_id IS NULL AND parent.user_id IS NOT NULL;
        GET DIAGNOSTICS changed = ROW_COUNT;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS sync_jobs_assign_user ON sync_jobs;
DROP FUNCTION IF EXISTS assign_sync_job_user();
DROP INDEX IF EXISTS sync_jobs_user_idx;
ALTER TABLE sync_jobs DROP COLUMN user_id;
DROP TABLE IF EXISTS deleted_user_tombstones;
