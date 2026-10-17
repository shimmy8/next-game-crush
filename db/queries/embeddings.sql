-- name: GetGameEmbeddingByHash :one
SELECT embedding
FROM game_embeddings
WHERE game_id = $1 AND model = $2 AND input_content_hash = $3;

