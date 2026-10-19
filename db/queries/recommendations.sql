-- name: GetRecommendationForUser :one
SELECT r.*, g.name FROM recommendations r JOIN games g ON g.id=r.game_id
WHERE r.id=$1 AND r.user_id=$2;

-- name: GetRecommendationFactors :one
SELECT factors, weights FROM recommendation_factors WHERE recommendation_id=$1;

-- name: GetLatestRecommendations :many
SELECT r.*, g.name FROM recommendations r JOIN games g ON g.id=r.game_id
WHERE r.user_id=$1 AND r.ranking_version=(SELECT max(ranking_version) FROM recommendation_runs WHERE user_id=$1 AND published)
ORDER BY r.score DESC, lower(g.name) COLLATE "C", g.name COLLATE "C", g.id;

-- name: GetRecommendationRunProvenance :one
SELECT profile_id,preference_overlay_id,ranking_version,algorithm_version,build_key,published
FROM recommendation_runs WHERE user_id=sqlc.arg(user_id) AND ranking_version=sqlc.arg(ranking_version);
