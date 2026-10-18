#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
deploy_script="$repo_root/deploy/deploy.sh"
valid_image="ghcr.io/shimmy8/next-game-crush:0123456789abcdef0123456789abcdef01234567"
previous_image="ghcr.io/shimmy8/next-game-crush:fedcba9876543210fedcba9876543210fedcba98"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local expected="$2"
  local message="$3"
  grep -Fq -- "$expected" "$file" || fail "$message"
}

assert_not_contains() {
  local file="$1"
  local unexpected="$2"
  local message="$3"
  if grep -Fq -- "$unexpected" "$file"; then
    fail "$message"
  fi
}

assert_before() {
  local file="$1"
  local earlier="$2"
  local later="$3"
  local message="$4"
  local earlier_line later_line
  earlier_line="$(grep -Fn -- "$earlier" "$file" | head -1 | cut -d: -f1 || true)"
  later_line="$(grep -Fn -- "$later" "$file" | head -1 | cut -d: -f1 || true)"
  if [[ -z "$earlier_line" || -z "$later_line" || "$earlier_line" -ge "$later_line" ]]; then
    fail "$message"
  fi
}

setup_case() {
  MOCK_LOCK_AVAILABLE=1
  MOCK_CURRENT_IMAGE=''
  MOCK_DUMP_EXIT=0
  MOCK_GZIP_EXIT=0
  MOCK_CONTAINER_HEALTH=0
  MOCK_ROLLBACK_HEALTH=0
  MOCK_PUBLIC_HEALTH_EXIT=0
  MOCK_PRUNE_EXIT=0
  MOCK_REPOSITORY_IMAGES=''
  PUBLIC_HEALTH_URL='https://nextgamecrush.site/api/health'
  case_dir="$(mktemp -d)"
  deploy_root="$case_dir/root"
  mock_bin="$case_dir/bin"
  command_log="$case_dir/commands.log"
  output_log="$case_dir/output.log"
  mkdir -p "$deploy_root/backups" "$mock_bin"
  cp "$repo_root/compose.prod.yml" "$deploy_root/compose.prod.yml"
  printf '%s\n' 'POSTGRES_PASSWORD=fixture-database-secret' >"$deploy_root/.env"
  chmod 600 "$deploy_root/.env"
  : >"$command_log"

  cat >"$mock_bin/flock" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'flock %s\n' "$*" >>"$MOCK_COMMAND_LOG"
[[ "${MOCK_LOCK_AVAILABLE:-1}" == "1" ]]
MOCK

  cat >"$mock_bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
{
  printf 'docker APP_IMAGE_REF=%q' "${APP_IMAGE_REF:-}"
  for argument in "$@"; do
    printf ' %q' "$argument"
  done
  printf '\n'
} >>"$MOCK_COMMAND_LOG"

if [[ "${1:-}" == "image" && "${2:-}" == "prune" ]]; then
  exit "${MOCK_PRUNE_EXIT:-0}"
fi

if [[ "${1:-}" == "image" && "${2:-}" == "ls" ]]; then
  printf '%s' "${MOCK_REPOSITORY_IMAGES:-}"
  exit 0
fi

if [[ "${1:-}" == "image" && "${2:-}" == "rm" ]]; then
  exit "${MOCK_PRUNE_EXIT:-0}"
fi

if [[ "${1:-}" == "inspect" ]]; then
  if [[ -n "${MOCK_CURRENT_IMAGE:-}" ]]; then
    printf '%s\n' "$MOCK_CURRENT_IMAGE"
    exit 0
  fi
  exit 1
fi

if [[ "${1:-}" == "compose" ]]; then
  for argument in "$@"; do
    if [[ "$argument" == "ps" ]]; then
      [[ -n "${MOCK_CURRENT_IMAGE:-}" ]] && printf '%s\n' app-container
      exit 0
    fi
    if [[ "$argument" == "exec" ]]; then
      if [[ "${MOCK_DUMP_EXIT:-0}" != "0" ]]; then
        exit "$MOCK_DUMP_EXIT"
      fi
      printf '%s\n' 'fixture database dump'
      exit 0
    fi
    if [[ "$argument" == "up" && " $* " == *' app '* ]]; then
      if [[ "${APP_IMAGE_REF:-}" == "${MOCK_REQUESTED_IMAGE:-}" && "${MOCK_CONTAINER_HEALTH:-0}" != "0" ]]; then
        exit "$MOCK_CONTAINER_HEALTH"
      fi
      if [[ "${APP_IMAGE_REF:-}" == "${MOCK_PREVIOUS_IMAGE:-}" && "${MOCK_ROLLBACK_HEALTH:-0}" != "0" ]]; then
        exit "$MOCK_ROLLBACK_HEALTH"
      fi
      exit 0
    fi
  done
fi

exit 0
MOCK

  cat >"$mock_bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
{
  printf 'curl'
  for argument in "$@"; do
    printf ' %q' "$argument"
  done
  printf '\n'
} >>"$MOCK_COMMAND_LOG"
exit "${MOCK_PUBLIC_HEALTH_EXIT:-0}"
MOCK

  cat >"$mock_bin/gzip" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'gzip' >>"$MOCK_COMMAND_LOG"
for argument in "$@"; do
  printf ' %q' "$argument" >>"$MOCK_COMMAND_LOG"
done
printf '\n' >>"$MOCK_COMMAND_LOG"
if [[ "${MOCK_GZIP_EXIT:-0}" != "0" ]]; then
  exit "$MOCK_GZIP_EXIT"
fi
exec /usr/bin/gzip "$@"
MOCK

  cat >"$mock_bin/mv" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
{
  printf 'mv'
  for argument in "$@"; do
    printf ' %q' "$argument"
  done
  printf '\n'
} >>"$MOCK_COMMAND_LOG"
if [[ "${1:-}" == '-fT' ]]; then
  shift
  exec /bin/mv -f "$@"
fi
exec /bin/mv "$@"
MOCK

  chmod +x "$mock_bin/flock" "$mock_bin/docker" "$mock_bin/curl" "$mock_bin/gzip" "$mock_bin/mv"
}

teardown_case() {
  rm -rf "$case_dir"
}

run_deploy() {
  local image="$1"
  local status

  set +e
  env \
    PATH="$mock_bin:$PATH" \
    DEPLOY_ROOT="$deploy_root" \
    PUBLIC_HEALTH_URL="${PUBLIC_HEALTH_URL:-https://nextgamecrush.site/api/health}" \
    MOCK_COMMAND_LOG="$command_log" \
    MOCK_LOCK_AVAILABLE="${MOCK_LOCK_AVAILABLE:-1}" \
    MOCK_CURRENT_IMAGE="${MOCK_CURRENT_IMAGE:-}" \
    MOCK_DUMP_EXIT="${MOCK_DUMP_EXIT:-0}" \
    MOCK_GZIP_EXIT="${MOCK_GZIP_EXIT:-0}" \
    MOCK_CONTAINER_HEALTH="${MOCK_CONTAINER_HEALTH:-0}" \
    MOCK_ROLLBACK_HEALTH="${MOCK_ROLLBACK_HEALTH:-0}" \
    MOCK_PUBLIC_HEALTH_EXIT="${MOCK_PUBLIC_HEALTH_EXIT:-0}" \
    MOCK_PRUNE_EXIT="${MOCK_PRUNE_EXIT:-0}" \
    MOCK_REPOSITORY_IMAGES="${MOCK_REPOSITORY_IMAGES:-}" \
    MOCK_REQUESTED_IMAGE="$valid_image" \
    MOCK_PREVIOUS_IMAGE="$previous_image" \
    bash "$deploy_script" "$image" >"$output_log" 2>&1
  status=$?
  set -e
  return "$status"
}

run_deploy_with_inherited_lock() {
  local image="$1"
  local status

  set +e
  (
    exec 9>"$deploy_root/deploy.lock"
    env \
      PATH="$mock_bin:$PATH" \
      DEPLOY_ROOT="$deploy_root" \
      DEPLOY_LOCK_FD=9 \
      PUBLIC_HEALTH_URL="${PUBLIC_HEALTH_URL:-https://nextgamecrush.site/api/health}" \
      MOCK_COMMAND_LOG="$command_log" \
      MOCK_LOCK_AVAILABLE="${MOCK_LOCK_AVAILABLE:-1}" \
      MOCK_CURRENT_IMAGE="${MOCK_CURRENT_IMAGE:-}" \
      MOCK_DUMP_EXIT="${MOCK_DUMP_EXIT:-0}" \
      MOCK_GZIP_EXIT="${MOCK_GZIP_EXIT:-0}" \
      MOCK_CONTAINER_HEALTH="${MOCK_CONTAINER_HEALTH:-0}" \
      MOCK_ROLLBACK_HEALTH="${MOCK_ROLLBACK_HEALTH:-0}" \
      MOCK_PUBLIC_HEALTH_EXIT="${MOCK_PUBLIC_HEALTH_EXIT:-0}" \
      MOCK_PRUNE_EXIT="${MOCK_PRUNE_EXIT:-0}" \
      MOCK_REPOSITORY_IMAGES="${MOCK_REPOSITORY_IMAGES:-}" \
      MOCK_REQUESTED_IMAGE="$valid_image" \
      MOCK_PREVIOUS_IMAGE="$previous_image" \
      bash "$deploy_script" "$image" >"$output_log" 2>&1
  )
  status=$?
  set -e
  return "$status"
}

test_rejects_invalid_image_reference() {
  setup_case
  local invalid_images=(
    'ghcr.io/shimmy8/next-game-crush:latest'
    'ghcr.io/shimmy8/next-game-crush:0123456789ABCDEF0123456789ABCDEF01234567'
    'ghcr.io/shimmy8/other:0123456789abcdef0123456789abcdef01234567'
    'ghcr.io/shimmy8/next-game-crush:0123456789abcdef0123456789abcdef01234567;touch /tmp/nope'
  )
  local image

  for image in "${invalid_images[@]}"; do
    : >"$command_log"
    if run_deploy "$image"; then
      fail "accepted invalid image reference"
    fi
    [[ ! -s "$command_log" ]] || fail "invalid image reached an external command"
  done
  teardown_case
}

test_lock_contention_fails_without_changes() {
  setup_case
  MOCK_LOCK_AVAILABLE=0
  if run_deploy "$valid_image"; then
    fail "deployment succeeded while the lock was held"
  fi
  assert_contains "$command_log" 'flock -n 9' "deployment did not request the lock"
  assert_not_contains "$command_log" 'docker ' "lock contention invoked Docker"
  [[ ! -e "$deploy_root/backups/latest.sql.gz" ]] || fail "lock contention created a backup"
  teardown_case
}

test_inherited_lock_covers_rollout() {
  setup_case
  if ! run_deploy_with_inherited_lock "$valid_image"; then
    fail "deployment rejected the workflow's inherited deployment lock"
  fi
  [[ "$(grep -Fc 'flock -n 9' "$command_log")" -eq 1 ]] ||
    fail "inherited deployment lock was not validated exactly once"
  assert_contains "$command_log" ' pull app' "inherited lock deployment did not reach rollout"
  assert_contains "$command_log" 'curl --fail' "inherited lock deployment did not reach public verification"
  teardown_case
}

test_rejects_inherited_descriptor_for_another_file() {
  setup_case
  set +e
  (
    exec 9>"$case_dir/not-the-deploy-lock"
    env \
      PATH="$mock_bin:$PATH" \
      DEPLOY_ROOT="$deploy_root" \
      DEPLOY_LOCK_FD=9 \
      MOCK_COMMAND_LOG="$command_log" \
      bash "$deploy_script" "$valid_image" >"$output_log" 2>&1
  )
  status=$?
  set -e
  [[ "$status" -ne 0 ]] || fail "deployment accepted an unrelated inherited lock descriptor"
  assert_not_contains "$command_log" 'docker ' "invalid inherited descriptor reached Docker"
  teardown_case
}

test_rejects_unprotected_inputs() {
  setup_case
  /bin/mv "$deploy_root/.env" "$deploy_root/.env.absent"
  if run_deploy "$valid_image"; then
    fail "deployment accepted a missing .env"
  fi
  [[ ! -s "$command_log" ]] || fail "missing .env reached an external deployment command"
  teardown_case

  setup_case
  /bin/mv "$deploy_root/.env" "$deploy_root/.env.target"
  ln -s .env.target "$deploy_root/.env"
  if run_deploy "$valid_image"; then
    fail "deployment accepted a symlinked .env"
  fi
  [[ ! -s "$command_log" ]] || fail "symlinked .env reached an external deployment command"
  teardown_case

  setup_case
  chmod 640 "$deploy_root/.env"
  if run_deploy "$valid_image"; then
    fail "deployment accepted an .env with the wrong mode"
  fi
  [[ ! -s "$command_log" ]] || fail "wrong .env mode reached an external deployment command"
  teardown_case

  setup_case
  /bin/mv "$deploy_root/compose.prod.yml" "$deploy_root/compose.prod.target.yml"
  ln -s compose.prod.target.yml "$deploy_root/compose.prod.yml"
  if run_deploy "$valid_image"; then
    fail "deployment accepted a symlinked Compose file"
  fi
  [[ ! -s "$command_log" ]] || fail "symlinked Compose file reached an external deployment command"
  teardown_case
}

test_rejects_unsafe_backup_targets() {
  setup_case
  mkdir "$deploy_root/backups/latest.sql.gz"
  if run_deploy "$valid_image"; then
    fail "deployment accepted a directory as the backup target"
  fi
  assert_not_contains "$command_log" ' pull app' "directory backup target pulled the app"
  assert_not_contains "$command_log" ' up -d --no-deps' "directory backup target updated the app"
  teardown_case

  setup_case
  mkdir "$case_dir/backup-target-directory"
  ln -s "$case_dir/backup-target-directory" "$deploy_root/backups/latest.sql.gz"
  if run_deploy "$valid_image"; then
    fail "deployment accepted a directory symlink as the backup target"
  fi
  assert_not_contains "$command_log" ' pull app' "directory symlink backup target pulled the app"
  assert_not_contains "$command_log" ' up -d --no-deps' "directory symlink backup target updated the app"
  teardown_case
}

test_dump_failure_stops_before_pull() {
  setup_case
  printf '%s\n' 'known-good-backup' >"$deploy_root/backups/latest.sql.gz"
  cp "$deploy_root/backups/latest.sql.gz" "$case_dir/original-backup"
  MOCK_DUMP_EXIT=23

  if run_deploy "$valid_image"; then
    fail "deployment succeeded after pg_dump failed"
  fi
  cmp -s "$case_dir/original-backup" "$deploy_root/backups/latest.sql.gz" || \
    fail "failed dump replaced the last successful backup"
  assert_contains "$command_log" ' up -d --wait --wait-timeout 90 db mailpit' \
    "database and Mailpit were not started before the dump"
  assert_contains "$command_log" ' exec -T db sh -c ' \
    "deployment did not execute the database dump inside the database container"
  assert_contains "$command_log" 'pg_dump' \
    "deployment did not invoke pg_dump"
  assert_not_contains "$command_log" ' pull app' "failed dump pulled the app image"
  assert_not_contains "$command_log" ' up -d --no-deps' "failed dump updated the app"
  if find "$deploy_root/backups" -name 'latest.sql.gz.tmp.*' -print -quit | grep -q .; then
    fail "failed dump left a temporary backup"
  fi
  teardown_case
}

test_gzip_failure_preserves_backup() {
  setup_case
  printf '%s\n' 'known-good-backup' >"$deploy_root/backups/latest.sql.gz"
  cp "$deploy_root/backups/latest.sql.gz" "$case_dir/original-backup"
  MOCK_GZIP_EXIT=31

  if run_deploy "$valid_image"; then
    fail "deployment succeeded after gzip failed"
  fi
  cmp -s "$case_dir/original-backup" "$deploy_root/backups/latest.sql.gz" || \
    fail "gzip failure replaced the last successful backup"
  assert_contains "$command_log" 'gzip -c' "deployment did not run the gzip side of the dump pipeline"
  assert_not_contains "$command_log" ' pull app' "gzip failure pulled the app image"
  assert_not_contains "$command_log" ' up -d --no-deps' "gzip failure updated the app"
  if find "$deploy_root/backups" -name 'latest.sql.gz.tmp.*' -print -quit | grep -q .; then
    fail "gzip failure left a temporary backup"
  fi
  teardown_case
}

test_first_deploy_without_previous_image() {
  setup_case
  if ! run_deploy "$valid_image"; then
    fail "first deployment failed"
  fi
  assert_contains "$command_log" ' compose --env-file ' "deployment did not use Docker Compose"
  assert_contains "$command_log" ' ps -q app' "deployment did not inspect the current app container"
  assert_contains "$command_log" "APP_IMAGE_REF=$valid_image" "deployment did not select the requested image"
  assert_contains "$command_log" ' pull app' "deployment did not pull the requested app image"
  assert_contains "$command_log" ' up -d --no-deps --wait --wait-timeout 90 app' \
    "deployment did not start and wait for the app"
  assert_contains "$command_log" \
    'curl --fail --silent --show-error --output /dev/null --connect-timeout 10 --max-time 30 -- https://nextgamecrush.site/api/health' \
    "deployment did not perform the bounded public health check"
  assert_contains "$command_log" ' image prune -f --filter dangling=true' \
    "successful deployment did not prune only dangling images"
  assert_not_contains "$command_log" "APP_IMAGE_REF=$previous_image" \
    "first deployment attempted an image rollback"
  assert_before "$command_log" ' ps -q app' ' pull app' \
    "deployment did not inspect the current app before pulling its replacement"
  [[ -s "$deploy_root/backups/latest.sql.gz" ]] || fail "first deployment did not create the backup"
  [[ "$(gzip -dc "$deploy_root/backups/latest.sql.gz")" == 'fixture database dump' ]] || \
    fail "backup does not contain the complete pg_dump output"
  assert_contains "$command_log" 'mv -fT ' "backup replacement is not no-target-directory safe"
  assert_not_contains "$output_log" 'fixture-database-secret' "deployment output exposed a fixture secret"
  teardown_case
}

test_successful_upgrade_uses_exact_image() {
  setup_case
  MOCK_CURRENT_IMAGE="$previous_image"
  if ! run_deploy "$valid_image"; then
    fail "upgrade deployment failed"
  fi
  assert_contains "$command_log" ' inspect --format ' \
    "deployment did not inspect the previous full image reference"
  assert_contains "$command_log" "docker APP_IMAGE_REF=$valid_image compose" \
    "upgrade did not pass the exact requested image to Compose"
  assert_contains "$command_log" "APP_IMAGE_REF=$valid_image compose --env-file" \
    "upgrade did not use the exact requested image"
  assert_not_contains "$command_log" "APP_IMAGE_REF=$previous_image compose" \
    "successful upgrade unexpectedly rolled back"
  teardown_case
}

test_public_health_failure_rolls_back_image() {
  setup_case
  MOCK_CURRENT_IMAGE="$previous_image"
  MOCK_PUBLIC_HEALTH_EXIT=22
  if run_deploy "$valid_image"; then
    fail "deployment succeeded after public health failed"
  fi
  assert_contains "$command_log" "docker APP_IMAGE_REF=$valid_image compose" \
    "failed rollout did not deploy the requested image"
  assert_contains "$command_log" "docker APP_IMAGE_REF=$previous_image compose" \
    "public health failure did not restore the previous image"
  assert_contains "$command_log" ' up -d --no-deps --wait --wait-timeout 90 app' \
    "rollback did not wait for previous app health"
  assert_before "$command_log" "APP_IMAGE_REF=$valid_image compose" "APP_IMAGE_REF=$previous_image compose" \
    "rollback ran before the requested image rollout"
  assert_not_contains "$command_log" ' image prune ' "failed rollout pruned images"
  teardown_case
}

test_container_health_failure_rolls_back_image() {
  setup_case
  MOCK_CURRENT_IMAGE="$previous_image"
  MOCK_CONTAINER_HEALTH=1
  if run_deploy "$valid_image"; then
    fail "deployment succeeded after container health failed"
  fi
  assert_contains "$command_log" "docker APP_IMAGE_REF=$previous_image compose" \
    "container health failure did not restore the previous image"
  assert_not_contains "$command_log" 'curl ' \
    "deployment called public health before container health succeeded"
  assert_not_contains "$command_log" ' image prune ' "failed rollout pruned images"
  teardown_case
}

test_rollback_failure_preserves_rollout_failure() {
  setup_case
  MOCK_CURRENT_IMAGE="$previous_image"
  MOCK_PUBLIC_HEALTH_EXIT=22
  MOCK_ROLLBACK_HEALTH=9
  if run_deploy "$valid_image"; then
    fail "deployment succeeded after rollout and rollback both failed"
  fi
  assert_contains "$command_log" "docker APP_IMAGE_REF=$previous_image compose" \
    "failed rollout did not attempt the previous image"
  assert_contains "$output_log" 'deploy: previous app image rollback failed' \
    "rollback failure diagnostic is missing"
  teardown_case
}

test_prune_failure_is_best_effort() {
  setup_case
  MOCK_PRUNE_EXIT=17
  if ! run_deploy "$valid_image"; then
    fail "dangling-image prune failure marked a healthy rollout failed"
  fi
  assert_contains "$command_log" ' image prune -f --filter dangling=true' \
    "healthy rollout did not attempt dangling-image pruning"
  assert_contains "$output_log" 'deploy: old-image prune failed' \
    "best-effort prune failure diagnostic is missing"
  teardown_case
}

test_successful_upgrade_prunes_only_older_repository_images() {
  setup_case
  local old_image='ghcr.io/shimmy8/next-game-crush:1111111111111111111111111111111111111111'
  MOCK_CURRENT_IMAGE="$previous_image"
  MOCK_REPOSITORY_IMAGES="$valid_image"$'\n'"$previous_image"$'\n'"$old_image"$'\n'

  if ! run_deploy "$valid_image"; then
    fail "healthy upgrade failed while pruning old repository images"
  fi
  assert_contains "$command_log" " image rm $old_image" \
    "healthy rollout did not remove the image older than the rollback image"
  assert_not_contains "$command_log" " image rm $valid_image" \
    "healthy rollout removed the active image"
  assert_not_contains "$command_log" " image rm $previous_image" \
    "healthy rollout removed the previous rollback image"
  teardown_case
}

[[ -f "$deploy_script" ]] || fail "missing deployment script: deploy/deploy.sh"

test_rejects_invalid_image_reference
test_lock_contention_fails_without_changes
test_inherited_lock_covers_rollout
test_rejects_inherited_descriptor_for_another_file
test_rejects_unprotected_inputs
test_rejects_unsafe_backup_targets
test_dump_failure_stops_before_pull
test_gzip_failure_preserves_backup
test_first_deploy_without_previous_image
test_successful_upgrade_uses_exact_image
test_public_health_failure_rolls_back_image
test_container_health_failure_rolls_back_image
test_rollback_failure_preserves_rollout_failure
test_prune_failure_is_best_effort
test_successful_upgrade_prunes_only_older_repository_images

printf 'PASS: guarded deployment and rollback behavior\n'
