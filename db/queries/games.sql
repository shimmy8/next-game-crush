-- name: UpsertGame :one
INSERT INTO games (id, steam_app_id, name, source, last_successful_fetch_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (steam_app_id) DO UPDATE SET
    name = EXCLUDED.name,
    source = EXCLUDED.source,
    last_successful_fetch_at = EXCLUDED.last_successful_fetch_at,
    updated_at = now()
RETURNING *;

-- name: PutGameEmbedding :exec
INSERT INTO game_embeddings (game_id, model, input_content_hash, embedding)
VALUES ($1, $2, $3, $4)
ON CONFLICT (game_id, model) DO UPDATE SET
    input_content_hash = EXCLUDED.input_content_hash,
    embedding = EXCLUDED.embedding,
    last_successful_fetch_at = now();

-- name: GetGameEmbedding :one
SELECT * FROM game_embeddings WHERE game_id = $1 AND model = $2;

-- name: NearestGameEmbeddings :many
SELECT game_id, model, (embedding <=> sqlc.arg(query_embedding)::vector)::double precision AS cosine_distance
FROM game_embeddings
WHERE model = sqlc.arg(model)
ORDER BY embedding <=> sqlc.arg(query_embedding)::vector
LIMIT sqlc.arg(result_limit);
