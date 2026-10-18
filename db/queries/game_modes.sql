-- name: DeleteSteamGameModes :exec
DELETE FROM game_modes WHERE game_id = $1 AND source = 'steam_category';

-- name: InsertSteamGameMode :exec
INSERT INTO game_modes(game_id,mode,source,source_content_hash,last_successful_fetch_at)
VALUES($1,$2,'steam_category',$3,$4);

-- name: ListGameModes :many
SELECT * FROM game_modes WHERE game_id = ANY(sqlc.arg(game_ids)::uuid[]) ORDER BY game_id,mode,source;

-- Snapshots are decoded and fully validated by the Steam adapter before use.
-- Failed refreshes retain this last successful source snapshot and fetch time.
-- name: ListStoredSteamCategorySnapshots :many
SELECT id AS game_id, steam_app_id, source_snapshot, content_hash, last_successful_fetch_at
FROM games
WHERE source = 'steam_store' AND source_snapshot IS NOT NULL AND content_hash IS NOT NULL
  AND jsonb_typeof(source_snapshot) = 'object'
ORDER BY steam_app_id;
