-- +goose Up
ALTER TABLE email_deliveries
    ADD COLUMN kind text NOT NULL DEFAULT 'test',
    ADD COLUMN content_hash text NOT NULL DEFAULT '',
    ADD COLUMN recipient text NOT NULL DEFAULT 'next-game-crush@localhost',
    ADD COLUMN subject text NOT NULL DEFAULT '',
    ADD COLUMN text_body text NOT NULL DEFAULT '',
    ADD COLUMN html_body text NOT NULL DEFAULT '',
    ADD COLUMN attempt_worker_id text,
    ADD COLUMN attempt_token uuid,
	ADD COLUMN confirmation_token uuid,
    ADD COLUMN attempt_started_at timestamptz,
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT email_deliveries_status_check CHECK (status IN ('pending','sending','sent','failed','ambiguous')),
    ADD CONSTRAINT email_deliveries_attempt_check CHECK ((status = 'sending') = (attempt_worker_id IS NOT NULL AND attempt_token IS NOT NULL AND attempt_started_at IS NOT NULL));

CREATE TABLE email_delivery_recommendations (
    delivery_id uuid NOT NULL REFERENCES email_deliveries(id) ON DELETE CASCADE,
    recommendation_id uuid NOT NULL REFERENCES recommendations(id) ON DELETE CASCADE,
    game_id uuid NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    content_hash text NOT NULL,
    PRIMARY KEY(delivery_id,recommendation_id)
);

CREATE TABLE email_action_nonces (
    nonce uuid PRIMARY KEY,
    delivery_id uuid NOT NULL REFERENCES email_deliveries(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    recommendation_id uuid NOT NULL REFERENCES recommendations(id) ON DELETE CASCADE,
    action text NOT NULL CHECK(action IN ('like','dislike','remind_me')),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    feedback_event_id uuid REFERENCES feedback_events(id) ON DELETE SET NULL
);

-- +goose Down
DROP TABLE IF EXISTS email_action_nonces, email_delivery_recommendations;
ALTER TABLE email_deliveries DROP CONSTRAINT email_deliveries_attempt_check, DROP CONSTRAINT email_deliveries_status_check,
    DROP COLUMN updated_at, DROP COLUMN attempt_started_at, DROP COLUMN confirmation_token, DROP COLUMN attempt_token, DROP COLUMN attempt_worker_id,
    DROP COLUMN html_body, DROP COLUMN text_body, DROP COLUMN subject, DROP COLUMN recipient, DROP COLUMN content_hash, DROP COLUMN kind;
