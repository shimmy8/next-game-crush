-- name: GetLatestPreferenceOverlay :one
SELECT * FROM preference_overlays WHERE user_id=$1 ORDER BY version DESC LIMIT 1;

-- name: GetPreferenceOverlayByBuildKey :one
SELECT * FROM preference_overlays WHERE user_id=sqlc.arg(user_id) AND build_key=sqlc.arg(build_key);

-- name: InsertPreferenceOverlay :one
INSERT INTO preference_overlays(id,user_id,version,algorithm_version,build_key,input_content_hash,algorithm_metadata,created_at)
VALUES(sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(version),sqlc.arg(algorithm_version),sqlc.arg(build_key),sqlc.arg(input_content_hash),sqlc.arg(algorithm_metadata),sqlc.arg(created_at))
RETURNING *;

-- name: InsertPreferenceOverlayEmbedding :exec
INSERT INTO preference_overlay_embeddings(overlay_id,polarity,model,input_content_hash,embedding,source,last_successful_fetch_at)
VALUES(sqlc.arg(overlay_id),sqlc.arg(polarity),sqlc.arg(model),sqlc.arg(input_content_hash),sqlc.arg(embedding),sqlc.arg(source),sqlc.arg(last_successful_fetch_at));

-- name: GetPreferenceOverlayEmbeddings :many
SELECT * FROM preference_overlay_embeddings WHERE overlay_id=$1 ORDER BY polarity,model;
