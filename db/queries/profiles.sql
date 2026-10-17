-- name: GetLatestPreferenceProfile :one
SELECT * FROM preference_profiles WHERE user_id=$1 ORDER BY version DESC LIMIT 1;

-- name: GetProfileEmbeddings :many
SELECT * FROM profile_embeddings WHERE profile_id=$1 ORDER BY polarity;
