#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly expected_image_pattern='^ghcr\.io/shimmy8/next-game-crush:[0-9a-f]{40}$'
readonly deploy_root="${DEPLOY_ROOT:-/opt/next-game-crush}"
readonly public_health_url="${PUBLIC_HEALTH_URL:-https://nextgamecrush.site/api/health}"
readonly env_file="$deploy_root/.env"
readonly compose_file="$deploy_root/compose.prod.yml"

fail() {
  printf 'deploy: %s\n' "$1" >&2
  exit 1
}

[[ $# -eq 1 ]] || fail 'expected exactly one image reference'
readonly requested_image="$1"
[[ "$requested_image" =~ $expected_image_pattern ]] || fail 'invalid image reference'

[[ -d "$deploy_root" ]] || fail 'deployment root is not a directory'
[[ -f "$env_file" && ! -L "$env_file" ]] || fail 'protected .env is not a regular file'
[[ -f "$compose_file" && ! -L "$compose_file" ]] || fail 'compose.prod.yml is not a regular file'
if env_mode="$(stat -c '%a' "$env_file" 2>/dev/null)"; then
  :
else
  env_mode="$(stat -f '%Lp' "$env_file")"
fi
[[ "$env_mode" == '600' ]] || fail 'protected .env must have mode 0600'

exec 9>"$deploy_root/deploy.lock"
flock -n 9 || fail 'another deployment is already running'

compose() {
  env APP_IMAGE_REF="$1" docker compose --env-file "$env_file" -f "$compose_file" "${@:2}"
}

backup_tmp=''
cleanup() {
  if [[ -n "$backup_tmp" ]]; then
    rm -f -- "$backup_tmp"
  fi
}
trap cleanup EXIT

readonly backup_dir="$deploy_root/backups"
readonly latest_backup="$backup_dir/latest.sql.gz"
if [[ -e "$backup_dir" || -L "$backup_dir" ]]; then
  [[ -d "$backup_dir" && ! -L "$backup_dir" ]] || fail 'backup path is not a regular directory'
else
  mkdir "$backup_dir"
fi
if [[ -L "$latest_backup" ]]; then
  fail 'latest backup must not be a symlink'
fi
if [[ -e "$latest_backup" && ! -f "$latest_backup" ]]; then
  fail 'latest backup target is not a regular file'
fi

compose "$requested_image" up -d --wait --wait-timeout 90 db mailpit

backup_tmp="$(mktemp "$backup_dir/latest.sql.gz.tmp.XXXXXX")"
if ! compose "$requested_image" exec -T db sh -c \
  'exec pg_dump --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"' \
  | gzip -c >"$backup_tmp"; then
  fail 'database dump failed; application was not changed'
fi
mv -fT -- "$backup_tmp" "$latest_backup"
backup_tmp=''

previous_image=''
app_container="$(compose "$requested_image" ps -q app)"
if [[ -n "$app_container" ]]; then
  inspected_image="$(docker inspect --format '{{.Config.Image}}' "$app_container")"
  if [[ "$inspected_image" =~ $expected_image_pattern ]]; then
    previous_image="$inspected_image"
  else
    printf '%s\n' 'deploy: current app image is not an eligible rollback image' >&2
  fi
fi

rollback_previous_image() {
  if [[ -z "$previous_image" ]]; then
    printf '%s\n' 'deploy: no previous app image is available for rollback' >&2
    return 0
  fi

  printf '%s\n' 'deploy: attempting to restore the previous app image' >&2
  if compose "$previous_image" up -d --no-deps --wait --wait-timeout 90 app; then
    printf '%s\n' 'deploy: previous app image restored' >&2
  else
    printf '%s\n' 'deploy: previous app image rollback failed' >&2
  fi
}

compose "$requested_image" pull app
if ! compose "$requested_image" up -d --no-deps --wait --wait-timeout 90 app; then
  printf '%s\n' 'deploy: requested app image did not become healthy' >&2
  rollback_previous_image
  exit 1
fi

if ! curl --fail --silent --show-error --output /dev/null --connect-timeout 10 --max-time 30 -- \
  "$public_health_url"; then
  printf '%s\n' 'deploy: public health check failed' >&2
  rollback_previous_image
  exit 1
fi

if ! docker image prune -f --filter dangling=true; then
  printf '%s\n' 'deploy: dangling-image prune failed; rollout remains healthy' >&2
fi
