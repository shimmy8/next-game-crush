-- +goose Up
ALTER TABLE feedback_events
    ADD COLUMN reasons text[] NOT NULL DEFAULT '{}'::text[];

UPDATE feedback_events
SET reasons = ARRAY[btrim(reason)]
WHERE btrim(reason) = ANY(ARRAY[
    'genre', 'setting', 'multiplayer', 'single-player', 'difficulty',
    'length', 'early-access', 'already-known', 'not-now', 'other'
]::text[]);

-- +goose StatementBegin
CREATE FUNCTION feedback_reasons_are_canonical(input text[])
RETURNS boolean
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
SET search_path = pg_catalog
AS $$
    SELECT input = ARRAY(
        SELECT allowed.reason
        FROM unnest(ARRAY[
            'genre', 'setting', 'multiplayer', 'single-player', 'difficulty',
            'length', 'early-access', 'already-known', 'not-now', 'other'
        ]::text[]) WITH ORDINALITY AS allowed(reason, position)
        WHERE allowed.reason = ANY(input)
        ORDER BY allowed.position
    );
$$;
-- +goose StatementEnd

ALTER TABLE feedback_events
    ADD CONSTRAINT feedback_events_reasons_count_check
        CHECK (cardinality(reasons) <= 10),
    ADD CONSTRAINT feedback_events_reasons_values_check
        CHECK (
            array_position(reasons, NULL) IS NULL
            AND reasons <@ ARRAY[
                'genre', 'setting', 'multiplayer', 'single-player', 'difficulty',
                'length', 'early-access', 'already-known', 'not-now', 'other'
            ]::text[]
        ),
    ADD CONSTRAINT feedback_events_reasons_canonical_check
        CHECK (feedback_reasons_are_canonical(reasons));

-- +goose Down
ALTER TABLE feedback_events
    DROP CONSTRAINT feedback_events_reasons_canonical_check,
    DROP CONSTRAINT feedback_events_reasons_values_check,
    DROP CONSTRAINT feedback_events_reasons_count_check,
    DROP COLUMN reasons;

DROP FUNCTION feedback_reasons_are_canonical(text[]);
