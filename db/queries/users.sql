-- name: UpsertUser :one
INSERT INTO users (id) VALUES ($1)
ON CONFLICT (id) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;
