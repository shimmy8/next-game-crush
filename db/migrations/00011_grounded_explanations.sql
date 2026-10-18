-- +goose Up
ALTER TABLE recommendations
    ADD COLUMN explanation_source text,
    ADD COLUMN explanation_failure_reason text;

UPDATE recommendations
SET explanation_source = CASE WHEN explanation_generated THEN 'ai' ELSE 'deterministic' END;

ALTER TABLE recommendations
    ALTER COLUMN explanation_source SET DEFAULT 'deterministic',
    ALTER COLUMN explanation_source SET NOT NULL,
    ADD CONSTRAINT recommendations_explanation_source_check
        CHECK (explanation_source IN ('ai','deterministic','none')),
    ADD CONSTRAINT recommendations_explanation_failure_reason_check
        CHECK (explanation_failure_reason IN (
            'disabled',
            'rate_limited',
            'timeout',
            'provider_unavailable',
            'refusal',
            'invalid_output',
            'invalid_citation',
            'unsafe_text',
            'unknown'
        ));

-- +goose Down
ALTER TABLE recommendations
    DROP CONSTRAINT recommendations_explanation_failure_reason_check,
    DROP CONSTRAINT recommendations_explanation_source_check,
    DROP COLUMN explanation_failure_reason,
    DROP COLUMN explanation_source;
