-- name: UpsertUser :one
INSERT INTO users (id) VALUES ($1)
ON CONFLICT (id) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetSteamAccountBySteamID :one
SELECT * FROM steam_accounts WHERE steam_id = $1;

-- name: UpsertSteamAccount :one
INSERT INTO steam_accounts (user_id, steam_id, source, availability)
VALUES ($1, $2, 'steam', 'unknown')
ON CONFLICT (steam_id) DO UPDATE SET steam_id = EXCLUDED.steam_id
RETURNING *;

-- name: CreateSession :exec
INSERT INTO sessions (id, user_id, csrf_token, expires_at) VALUES ($1, $2, $3, $4);

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = $1;
