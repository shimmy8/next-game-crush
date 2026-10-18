-- name: GetGameFeatureByHash :one
SELECT features
FROM game_features
WHERE game_id = $1 AND model = $2 AND input_content_hash = $3;

-- name: UpsertGameFeature :exec
INSERT INTO game_features (game_id, model, input_content_hash, features, source, last_successful_fetch_at)
VALUES ($1, $2, $3, $4, 'openai', now())
ON CONFLICT (game_id, model) DO UPDATE SET
    input_content_hash = EXCLUDED.input_content_hash,
    features = EXCLUDED.features,
    source = EXCLUDED.source,
    last_successful_fetch_at = EXCLUDED.last_successful_fetch_at;

-- name: GetLatestGameFeature :one
SELECT features, input_content_hash
FROM game_features
WHERE game_id = $1 AND model = $2;

-- name: LockGameEnrichmentEvidence :one
SELECT name,source_snapshot FROM games WHERE id=$1 FOR SHARE;

-- name: LockGameEnrichmentFeatures :one
SELECT features,input_content_hash FROM game_features WHERE game_id=$1 AND model=$2 FOR UPDATE;

-- name: GetProfileSummaryByHash :one
SELECT summary
FROM preference_profiles
WHERE id = $1 AND model = $2 AND input_content_hash = $3 AND summary IS NOT NULL;

-- name: UpdateProfileSummary :execrows
UPDATE preference_profiles
SET summary = $2, model = $3, input_content_hash = $4
WHERE id = $1;

