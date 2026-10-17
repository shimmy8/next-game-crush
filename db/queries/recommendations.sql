-- name: GetRecommendationForUser :one
SELECT r.*, g.name FROM recommendations r JOIN games g ON g.id=r.game_id
WHERE r.id=$1 AND r.user_id=$2;

-- name: GetRecommendationFactors :one
SELECT factors, weights FROM recommendation_factors WHERE recommendation_id=$1;

-- name: GetLatestRecommendations :many
SELECT r.*, g.name FROM recommendations r JOIN games g ON g.id=r.game_id
WHERE r.user_id=$1 AND r.ranking_version=(SELECT max(ranking_version) FROM recommendation_runs WHERE user_id=$1 AND published)
ORDER BY r.score DESC, lower(g.name), g.id;
