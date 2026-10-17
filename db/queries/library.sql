-- name: GetSteamAccountForUser :one
SELECT * FROM steam_accounts WHERE user_id = $1;

-- name: ListUserGamesForAchievements :many
SELECT g.steam_app_id, g.name, ug.playtime_minutes, ug.recently_played_minutes, ug.last_played_at
FROM user_games ug JOIN games g ON g.id = ug.game_id
WHERE ug.user_id = $1
ORDER BY g.steam_app_id;

-- name: UpsertUserSourceState :exec
INSERT INTO user_source_states (user_id, source, resource_key, availability, last_attempt_at, last_successful_fetch_at, error)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, source, resource_key) DO UPDATE SET
    availability = EXCLUDED.availability,
    last_attempt_at = EXCLUDED.last_attempt_at,
    last_successful_fetch_at = COALESCE(EXCLUDED.last_successful_fetch_at, user_source_states.last_successful_fetch_at),
    error = EXCLUDED.error;

-- name: UpsertAchievementSummary :exec
INSERT INTO achievement_summaries (user_id, game_id, achieved, total, source, last_successful_fetch_at, raw_snapshot)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, game_id) DO UPDATE SET
    achieved = EXCLUDED.achieved,
    total = EXCLUDED.total,
    source = EXCLUDED.source,
    last_successful_fetch_at = EXCLUDED.last_successful_fetch_at,
    raw_snapshot = EXCLUDED.raw_snapshot;
