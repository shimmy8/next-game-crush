#!/usr/bin/env bash
set -Eeuo pipefail

readonly repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly workflow="$repo_root/.github/workflows/deploy.yml"

fail() {
  printf 'workflow_test: %s\n' "$1" >&2
  exit 1
}

assert_file_contains() {
  local pattern="$1"
  local description="$2"
  grep -Eq -- "$pattern" "$workflow" || fail "$description"
}

assert_file_not_contains() {
  local pattern="$1"
  local description="$2"
  if grep -Eiq -- "$pattern" "$workflow"; then
    fail "$description"
  fi
}

assert_text_contains() {
  local text="$1"
  local pattern="$2"
  local description="$3"
  grep -Eq -- "$pattern" <<<"$text" || fail "$description"
}

assert_text_not_contains() {
  local text="$1"
  local pattern="$2"
  local description="$3"
  if grep -Eiq -- "$pattern" <<<"$text"; then
    fail "$description"
  fi
}

extract_job() {
  local job="$1"
  awk -v header="  $job:" '
    $0 == header { inside = 1 }
    inside && $0 != header && /^  [A-Za-z0-9_-]+:$/ { exit }
    inside { print }
  ' "$workflow"
}

extract_step() {
  local job_text="$1"
  local step_name="$2"
  awk -v header="      - name: $step_name" '
    $0 == header { inside = 1 }
    inside && $0 != header && /^      - name:/ { exit }
    inside { print }
  ' <<<"$job_text"
}

permission_keys() {
  local job_text="$1"
  awk '
    /^    permissions:$/ { inside = 1; next }
    inside && /^      [A-Za-z0-9_-]+:/ {
      key = $1
      sub(/:$/, "", key)
      print key
      next
    }
    inside && /^    [^ ]/ { exit }
  ' <<<"$job_text"
}

[[ -f "$workflow" ]] || fail 'missing .github/workflows/deploy.yml'

assert_file_contains '^  push:$' 'workflow must run for pushes'
assert_file_contains '^      - main$' 'push trigger must include main'
assert_file_contains '^  pull_request:$' 'workflow must run for pull requests'
assert_file_contains '^permissions: \{\}$' 'top-level permissions must default to none'

readonly verify_job="$(extract_job verify)"
readonly publish_job="$(extract_job publish)"
readonly deploy_job="$(extract_job deploy)"
[[ -n "$verify_job" ]] || fail 'verify job is missing'
[[ -n "$publish_job" ]] || fail 'publish job is missing'
[[ -n "$deploy_job" ]] || fail 'deploy job is missing'

assert_text_contains "$publish_job" '^    needs: verify$' 'publish must depend on verify'
assert_text_contains "$deploy_job" '^    needs: publish$' 'deploy must depend on publish'
assert_text_contains "$publish_job" "^    if: github\.event_name == 'push' && github\.ref == 'refs/heads/main'$" \
  'publish must be restricted to pushes on main'
assert_text_contains "$deploy_job" "^    if: github\.event_name == 'push' && github\.ref == 'refs/heads/main'$" \
  'deploy must be restricted to pushes on main'
assert_text_not_contains "$verify_job" '^    if:.*refs/heads/main' 'pull requests must be able to run verify'

[[ "$(permission_keys "$verify_job")" == 'contents' ]] || fail 'verify permissions must be exactly contents: read'
assert_text_contains "$verify_job" '^      contents: read$' 'verify must have contents: read permission'
[[ "$(permission_keys "$publish_job")" == $'contents\npackages' ]] ||
  fail 'publish permissions must be exactly contents: read and packages: write'
assert_text_contains "$publish_job" '^      contents: read$' 'publish must have contents: read permission'
assert_text_contains "$publish_job" '^      packages: write$' 'publish must have packages: write permission'
[[ "$(permission_keys "$deploy_job")" == 'contents' ]] || fail 'deploy permissions must be exactly contents: read'
assert_text_contains "$deploy_job" '^      contents: read$' 'deploy must have contents: read permission'
while IFS= read -r permission_key; do
  case "$permission_key" in
    contents | packages) ;;
    *) fail "unapproved job permission: $permission_key" ;;
  esac
done < <(awk '/^    permissions:$/ { inside = 1; next }
  inside && /^      [A-Za-z0-9_-]+:/ { key = $1; sub(/:$/, "", key); print key; next }
  inside && /^    [^ ]/ { inside = 0 }' "$workflow")

assert_text_contains "$deploy_job" '^    timeout-minutes: [0-9]+$' 'deploy must have a bounded job timeout'
assert_text_contains "$deploy_job" '^      name: production$' 'deploy must use the production environment'
assert_text_contains "$deploy_job" '^      url: https://nextgamecrush\.site$' 'production environment URL is missing'
assert_text_contains "$deploy_job" '^      group: production$' 'production concurrency group is missing'
assert_text_contains "$deploy_job" '^      cancel-in-progress: false$' 'production deploys must not cancel in progress'

readonly image_pattern='ghcr\.io/shimmy8/next-game-crush:\$\{\{ github\.sha \}\}'
assert_text_contains "$publish_job" "IMAGE_REF: $image_pattern" 'publish image tag must be the exact Git commit SHA'
assert_text_contains "$publish_job" '^          tags: \$\{\{ env\.IMAGE_REF \}\}$' 'publisher must push the exact IMAGE_REF'
assert_text_contains "$deploy_job" "IMAGE_REF: $image_pattern" 'deploy image tag must be the exact Git commit SHA'
assert_text_contains "$deploy_job" 'exec "\$deploy_root/deploy\.sh" "\$image_ref"' \
  'remote deployment must invoke deploy.sh with the exact image argument'
assert_text_contains "$deploy_job" '"\$target" bash -s -- "\$remote_incoming" "\$IMAGE_REF"' \
  'the exact SHA image must be passed to the remote install command'

[[ "$(grep -Fc 'StrictHostKeyChecking=yes' <<<"$deploy_job")" -eq 3 ]] ||
  fail 'the two SSH commands and SCP must each enable strict host checking'
[[ "$(grep -Fc 'UserKnownHostsFile="$known_hosts_file"' <<<"$deploy_job")" -eq 3 ]] ||
  fail 'the two SSH commands and SCP must each use the reviewed known_hosts file'
[[ "$(grep -Fc 'ConnectTimeout=10' <<<"$deploy_job")" -eq 3 ]] ||
  fail 'the two SSH commands and SCP must each have a connection timeout'
[[ "$(grep -Fc 'ServerAliveInterval=15' <<<"$deploy_job")" -eq 3 ]] ||
  fail 'the two SSH commands and SCP must each configure keepalive intervals'
[[ "$(grep -Fc 'ServerAliveCountMax=3' <<<"$deploy_job")" -eq 3 ]] ||
  fail 'the two SSH commands and SCP must each bound missed keepalives'
assert_file_not_contains 'ssh-keyscan' 'host keys must never be learned dynamically with ssh-keyscan'

readonly configure_step="$(extract_step "$deploy_job" 'Configure reviewed SSH credentials')"
readonly upload_step="$(extract_step "$deploy_job" 'Upload and deploy exact image')"
readonly cleanup_step="$(extract_step "$deploy_job" 'Remove SSH credentials')"
readonly freshness_step="$(extract_step "$deploy_job" 'Verify commit is current main')"
[[ -n "$freshness_step" ]] || fail 'main freshness check is missing from the serialized deploy job'
[[ -n "$configure_step" ]] || fail 'SSH credential configuration step is missing'
[[ -n "$upload_step" ]] || fail 'SSH upload step is missing'
[[ -n "$cleanup_step" ]] || fail 'SSH cleanup step is missing'
assert_text_contains "$freshness_step" '^        id: freshness$' 'freshness step must expose a stable output id'
assert_text_contains "$freshness_step" 'git fetch --no-tags --depth=1 origin' \
  'freshness step must read the current remote main after acquiring production concurrency'
assert_text_contains "$freshness_step" 'refs/heads/main:refs/remotes/origin/main' \
  'freshness step must refresh the main remote-tracking ref explicitly'
assert_text_contains "$freshness_step" 'git rev-parse refs/remotes/origin/main' \
  'freshness step must compare against the refreshed main commit'
assert_text_contains "$freshness_step" 'current=false.*GITHUB_OUTPUT' \
  'superseded commits must be marked without mutating production'
assert_text_contains "$freshness_step" 'current=true.*GITHUB_OUTPUT' \
  'the current main commit must be allowed to deploy'
assert_text_contains "$configure_step" "^        if: steps\.freshness\.outputs\.current == 'true'$" \
  'SSH setup must be skipped for a superseded commit'
assert_text_contains "$upload_step" "^        if: steps\.freshness\.outputs\.current == 'true'$" \
  'remote deployment must be skipped for a superseded commit'
assert_text_contains "$configure_step" '^          VPS_SSH_KEY: \$\{\{ secrets\.VPS_SSH_KEY \}\}$' \
  'SSH key secret must be scoped to the configuration step'
assert_text_contains "$configure_step" '^          VPS_KNOWN_HOSTS: \$\{\{ secrets\.VPS_KNOWN_HOSTS \}\}$' \
  'known_hosts secret must be scoped to the configuration step'
assert_text_not_contains "$deploy_job" '^      VPS_(SSH_KEY|KNOWN_HOSTS):' \
  'SSH secrets must not be exposed through the deploy job environment'
assert_text_not_contains "$upload_step" 'VPS_(SSH_KEY|KNOWN_HOSTS)' \
  'the upload step must use credential files, not secret environment variables'
assert_text_contains "$configure_step" 'chmod 0600 "\$HOME/\.ssh/id_ed25519" "\$HOME/\.ssh/known_hosts"' \
  'SSH key and known_hosts must both use mode 0600'
assert_text_contains "$cleanup_step" '^        if: always\(\)$' 'SSH credentials must always be cleaned up'
assert_text_contains "$cleanup_step" 'rm -f -- "\$HOME/\.ssh/id_ed25519" "\$HOME/\.ssh/known_hosts"' \
  'SSH cleanup must remove both credential files'

assert_text_contains "$upload_step" '^[[:space:]]*scp -i ' 'deployment artifacts must be uploaded with OpenSSH scp'
assert_text_contains "$upload_step" 'compose\.prod\.yml deploy/deploy\.sh ' 'both deployment artifacts must be uploaded together'
assert_text_contains "$upload_step" 'exec 9>"\$deploy_root/deploy\.lock"' \
  'remote artifact activation must open the deployment lock before installation'
assert_text_contains "$upload_step" 'flock -n 9' \
  'remote artifact activation must acquire the deployment lock before installation'
assert_text_contains "$upload_step" 'install -m 0644 .*compose\.prod\.yml\.new' 'Compose must be staged to a temporary file'
assert_text_contains "$upload_step" 'install -m 0755 .*deploy\.sh\.new' 'deploy.sh must be staged to a temporary file'
readonly lock_line="$(grep -n 'flock -n 9' <<<"$upload_step" | head -1 | cut -d: -f1)"
readonly first_install_line="$(grep -n 'install -m 0644 .*compose\.prod\.yml\.new' <<<"$upload_step" | cut -d: -f1)"
readonly second_install_line="$(grep -n 'install -m 0755 .*deploy\.sh\.new' <<<"$upload_step" | cut -d: -f1)"
readonly first_move_line="$(grep -n 'mv -f .*\.new' <<<"$upload_step" | head -1 | cut -d: -f1)"
[[ "$lock_line" -lt "$first_install_line" && "$lock_line" -lt "$second_install_line" ]] ||
  fail 'the deployment lock must be acquired before either artifact is installed'
[[ "$first_install_line" -lt "$first_move_line" && "$second_install_line" -lt "$first_move_line" ]] ||
  fail 'both deployment files must be staged before either atomic rename'
assert_text_contains "$upload_step" 'mv -f .*compose\.prod\.yml\.new.*compose\.prod\.yml' 'Compose installation must finish atomically'
assert_text_contains "$upload_step" 'mv -f .*deploy\.sh\.new.*deploy\.sh' 'deploy script installation must finish atomically'
assert_text_contains "$upload_step" 'DEPLOY_LOCK_FD=9 exec "\$deploy_root/deploy\.sh" "\$image_ref"' \
  'remote deployment must inherit the installer lock through exec'
assert_file_not_contains 'uses:.*(appleboy|ssh-action|scp-action)' 'third-party SSH actions are forbidden'

seen_checkout=0
seen_setup_go=0
seen_setup_node=0
seen_login=0
seen_buildx=0
seen_build_push=0
while IFS= read -r action_ref; do
  action_ref="${action_ref#*uses: }"
  case "$action_ref" in
    actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1) seen_checkout=1 ;;
    actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e) seen_setup_go=1 ;;
    actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1) seen_setup_node=1 ;;
    docker/login-action@dbcb813823bdd20940b903addbd779551569679f) seen_login=1 ;;
    docker/setup-buildx-action@d7f5e7f509e45cec5c76c4d5afdd7de93d0b3df5) seen_buildx=1 ;;
    docker/build-push-action@c3c9e263c25d99ce0380d002d59b67737d91b0dc) seen_build_push=1 ;;
    *) fail "external action is not approved: $action_ref" ;;
  esac
done < <(grep -E '^[[:space:]]*uses:' "$workflow")
[[ "$seen_checkout" -eq 1 ]] || fail 'approved checkout action is missing'
[[ "$seen_setup_go" -eq 1 ]] || fail 'approved setup-go action is missing'
[[ "$seen_setup_node" -eq 1 ]] || fail 'approved setup-node action is missing'
[[ "$seen_login" -eq 1 ]] || fail 'approved login action is missing'
[[ "$seen_buildx" -eq 1 ]] || fail 'approved Buildx action is missing'
[[ "$seen_build_push" -eq 1 ]] || fail 'approved build-push action is missing'

assert_text_contains "$verify_job" 'go-version: .1\.27\.2.' 'verify must use Go 1.27.2'
assert_text_contains "$verify_job" 'node-version: .26.' 'verify must use Node.js 26'
assert_text_contains "$verify_job" 'pnpm@11\.25\.0' 'verify must install pnpm 11.25.0'
assert_text_contains "$verify_job" 'pnpm --dir web install --frozen-lockfile' 'verify must install locked frontend dependencies'
assert_text_contains "$verify_job" '\[\[ -x /usr/bin/google-chrome \]\]' 'system Chrome fallback must check executable Chrome'
assert_text_contains "$verify_job" 'PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/google-chrome' 'system Chrome fallback path is incorrect'
assert_text_contains "$verify_job" "GOWORK=off TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127\\.0\\.0\\.1:55432/next_game_crush_test\\?sslmode=disable' go test -race \\./\\.\\.\\." \
  'verify must run all backend tests with race detection against the isolated test database'
assert_text_contains "$verify_job" 'pnpm --dir web run test' 'verify must run frontend unit tests'
assert_text_contains "$verify_job" 'pnpm --dir web run typecheck' 'verify must run frontend type checks'
assert_text_contains "$verify_job" 'pnpm --dir web run lint' 'verify must run frontend lint'
assert_text_contains "$verify_job" 'pnpm --dir web run build' 'verify must build the frontend'
assert_text_contains "$verify_job" 'docker compose build app' 'verify must build the production image'
assert_text_contains "$verify_job" 'playwright test tests/e2e/smoke\.spec\.ts' 'verify must run browser acceptance tests'
assert_text_contains "$verify_job" 'E2E_OPENAI_DISABLED=1.*completion-signals\.spec\.ts' \
  'verify must run completion acceptance with OpenAI disabled'
assert_text_contains "$verify_job" 'bash -n deploy/deploy\.sh deploy/tests/\*\.sh' 'verify must syntax-check deployment scripts'
for test_script in compose deploy docs workflow; do
  assert_text_contains "$verify_job" "bash deploy/tests/${test_script}_test\\.sh" \
    "verify must run ${test_script}_test.sh in its own diagnosable step"
done
[[ "$(grep -Ec '^    runs-on: ubuntu-24\.04$' "$workflow")" -eq 3 ]] ||
  fail 'every job must use the pinned Ubuntu 24.04 runner image'

assert_text_contains "$publish_job" '^          push: true$' 'container image publication must be enabled'
assert_text_contains "$publish_job" '^          cache-from: type=gha$' 'BuildKit cache restore is missing'
assert_text_contains "$publish_job" '^          cache-to: type=gha,mode=max$' 'BuildKit cache publication is missing'
assert_text_contains "$publish_job" 'org\.opencontainers\.image\.source=' 'source repository OCI label is missing'

printf '%s\n' 'workflow_test: PASS'
