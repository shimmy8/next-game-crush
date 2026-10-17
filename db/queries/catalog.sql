-- name: GetGameByAppID :one
SELECT * FROM games WHERE steam_app_id = $1;

-- name: ListCatalogGames :many
SELECT g.* FROM games g JOIN catalog_entries ce ON ce.game_id = g.id ORDER BY g.steam_app_id;

-- name: UpsertCatalogGame :one
INSERT INTO games (id, steam_app_id, name, source, last_successful_fetch_at, source_snapshot, availability, last_attempt_at, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, 'available', $5, $7)
ON CONFLICT (steam_app_id) DO UPDATE SET
    name = EXCLUDED.name,
    source = EXCLUDED.source,
    last_successful_fetch_at = EXCLUDED.last_successful_fetch_at,
    source_snapshot = EXCLUDED.source_snapshot,
    content_hash = EXCLUDED.content_hash,
    availability = 'available',
    last_attempt_at = EXCLUDED.last_attempt_at,
    updated_at = now()
RETURNING *;

-- name: InsertReleaseObservationIfChanged :one
WITH latest AS (
    SELECT precision, release_date, raw_text
    FROM game_release_dates
    WHERE game_release_dates.game_id = sqlc.arg(target_game_id)
    ORDER BY observed_at DESC, id DESC
    LIMIT 1
), inserted AS (
    INSERT INTO game_release_dates (id, game_id, precision, release_date, raw_text, observed_at, source, last_successful_fetch_at)
    SELECT sqlc.arg(id), sqlc.arg(target_game_id), sqlc.arg(precision), sqlc.narg(release_date), sqlc.narg(raw_text),
           sqlc.arg(observed_at), sqlc.arg(source), sqlc.arg(last_successful_fetch_at)
    WHERE NOT EXISTS (
        SELECT 1 FROM latest
        WHERE latest.precision = sqlc.arg(precision)
          AND latest.release_date IS NOT DISTINCT FROM sqlc.narg(release_date)
          AND latest.raw_text IS NOT DISTINCT FROM sqlc.narg(raw_text)
    )
    RETURNING id
)
SELECT id FROM inserted;

-- name: GetReleaseHistory :many
SELECT * FROM game_release_dates WHERE game_id = $1 ORDER BY observed_at, id;

-- name: GetCurrentReleaseDate :one
SELECT * FROM game_release_dates WHERE game_id = $1 ORDER BY observed_at DESC, id DESC LIMIT 1;

-- name: UpsertCatalogFetchState :exec
INSERT INTO catalog_fetch_states (game_id, availability, last_attempt_at, last_successful_fetch_at, error)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (game_id) DO UPDATE SET
    availability = EXCLUDED.availability,
    last_attempt_at = EXCLUDED.last_attempt_at,
    last_successful_fetch_at = COALESCE(EXCLUDED.last_successful_fetch_at, catalog_fetch_states.last_successful_fetch_at),
    error = EXCLUDED.error;
