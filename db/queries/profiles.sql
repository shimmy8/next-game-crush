-- name: GetLatestActivityProfileState :one
SELECT * FROM preference_profiles WHERE user_id=$1 ORDER BY version DESC LIMIT 1;

-- name: GetLatestPublishedActivityProfile :one
SELECT * FROM preference_profiles
WHERE user_id=$1 AND publication_state='published'
ORDER BY version DESC LIMIT 1;

-- name: GetActivityProfileByID :one
SELECT * FROM preference_profiles WHERE id=sqlc.arg(id) AND user_id=sqlc.arg(user_id);

-- name: GetActivityProfileByBuildKey :one
SELECT * FROM preference_profiles WHERE user_id=sqlc.arg(user_id) AND build_key=sqlc.arg(build_key);

-- name: InsertActivityProfile :one
INSERT INTO preference_profiles(
    id,user_id,version,summary,model,input_content_hash,algorithm_metadata,created_at,
    build_key,publication_state,published_at,summary_source
) VALUES(
    sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(version),sqlc.narg(summary),sqlc.narg(model),
    sqlc.narg(input_content_hash),sqlc.arg(algorithm_metadata),sqlc.arg(created_at),
    sqlc.narg(build_key),sqlc.arg(publication_state),sqlc.narg(published_at),sqlc.narg(summary_source)
) RETURNING *;

-- name: InsertActivityProfileEmbedding :exec
INSERT INTO profile_embeddings(profile_id,polarity,model,input_content_hash,embedding,source,last_successful_fetch_at)
VALUES(sqlc.arg(profile_id),sqlc.arg(polarity),sqlc.arg(model),sqlc.arg(input_content_hash),sqlc.arg(embedding),sqlc.arg(source),sqlc.arg(last_successful_fetch_at));

-- name: InsertActivityProfileEvidence :exec
INSERT INTO activity_profile_evidence(
    profile_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,achieved,total,
    completion_ending_count,completion_hundred_percent_basis,completion_weight,completion_at,
    completion_basis,completion_algorithm_version
) VALUES(
    sqlc.arg(profile_id),sqlc.arg(game_id),sqlc.arg(playtime_minutes),sqlc.arg(recently_played_minutes),
    sqlc.narg(last_played_at),sqlc.arg(achieved),sqlc.arg(total),sqlc.arg(completion_ending_count),
    sqlc.arg(completion_hundred_percent_basis),sqlc.arg(completion_weight),sqlc.narg(completion_at),
    sqlc.narg(completion_basis),sqlc.narg(completion_algorithm_version)
);

-- name: PublishActivityProfile :one
UPDATE preference_profiles SET
    summary=sqlc.arg(summary),model=sqlc.narg(model),input_content_hash=sqlc.narg(input_content_hash),
    summary_source=sqlc.arg(summary_source),publication_state='published',published_at=sqlc.arg(published_at)
WHERE preference_profiles.id=sqlc.arg(id) AND preference_profiles.user_id=sqlc.arg(user_id)
  AND preference_profiles.publication_state='building'
  AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones tombstone WHERE tombstone.user_id=preference_profiles.user_id)
RETURNING *;

-- name: FailBuildingActivityProfile :execrows
UPDATE preference_profiles SET publication_state='failed'
WHERE preference_profiles.id=sqlc.arg(id) AND preference_profiles.user_id=sqlc.arg(user_id)
  AND preference_profiles.publication_state='building'
  AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones tombstone WHERE tombstone.user_id=preference_profiles.user_id);

-- name: GetProfileEmbeddings :many
SELECT * FROM profile_embeddings WHERE profile_id=$1 ORDER BY polarity;
