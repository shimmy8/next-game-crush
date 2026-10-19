#!/usr/bin/env bash
set -Eeuo pipefail

readonly repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly nginx_file="$repo_root/deploy/nginx.conf.example"
readonly setup_guide="$repo_root/docs/vps-setup.md"
readonly operations_guide="$repo_root/docs/deployment.md"
readonly readme="$repo_root/README.md"

fail() {
  printf 'docs_test: %s\n' "$1" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local pattern="$2"
  local description="$3"
  grep -Eiq -- "$pattern" "$file" || fail "$description"
}

assert_not_contains() {
  local file="$1"
  local pattern="$2"
  local description="$3"
  if grep -Eiq -- "$pattern" "$file"; then
    fail "$description"
  fi
}

assert_ordered() {
  local file="$1"
  shift
  local previous=0
  local pattern line
  for pattern in "$@"; do
    line="$(grep -Ein -- "$pattern" "$file" | awk -F: -v previous="$previous" '$1 > previous { print $1; exit }' || true)"
    [[ -n "$line" ]] || fail "$file is missing ordered section matching: $pattern"
    previous="$line"
  done
}

for required_file in "$nginx_file" "$setup_guide" "$operations_guide"; do
  [[ -f "$required_file" ]] || fail "missing ${required_file#"$repo_root/"}"
done

# The checked-in fragment must preserve the existing certificate lifecycle and expose only the app.
[[ "$(grep -Ec '^[[:space:]]*server_name[[:space:]]+' "$nginx_file")" -eq 1 ]] ||
  fail 'Nginx example must contain exactly one server_name directive'
assert_contains "$nginx_file" '^[[:space:]]*server_name[[:space:]]+nextgamecrush\.site;' \
  'Nginx example must name only nextgamecrush.site'
assert_contains "$nginx_file" 'proxy_pass[[:space:]]+http://127\.0\.0\.1:18080;' \
  'Nginx example must proxy to the loopback app port'
assert_contains "$operations_guide" '127\.0\.0\.1:18080' \
  'operations guide must use the production host port'
assert_contains "$setup_guide" 'Do not add `18080`' \
  'VPS guide must keep the production app port off the firewall'
assert_contains "$nginx_file" 'proxy_set_header[[:space:]]+Host[[:space:]]+\$host;' \
  'Nginx example must forward Host'
assert_contains "$nginx_file" 'proxy_set_header[[:space:]]+X-Real-IP[[:space:]]+\$remote_addr;' \
  'Nginx example must forward the client address'
assert_contains "$nginx_file" 'proxy_set_header[[:space:]]+X-Forwarded-For[[:space:]]+\$proxy_add_x_forwarded_for;' \
  'Nginx example must forward the proxy chain'
assert_contains "$nginx_file" 'proxy_set_header[[:space:]]+X-Forwarded-Proto[[:space:]]+\$scheme;' \
  'Nginx example must forward the protocol'
assert_not_contains "$nginx_file" '(mailpit|postgres|5432|8025|1025)' \
  'Nginx example must not expose Mailpit or PostgreSQL'

# A new operator must be able to follow the bootstrap guide from top to bottom.
assert_ordered "$setup_guide" \
  '^## 1\..*deploy user' \
  '^## 2\..*Docker' \
  '^## 3\..*fingerprint' \
  '^## 4\..*SSH' \
  '^## 5\..*UFW' \
  '^## 6\..*director' \
  '^## 7\..*\.env' \
  '^## 8\..*Nginx' \
  '^## 9\..*GitHub.*production' \
  '^## 10\..*GHCR' \
  '^## 11\..*first deployment' \
  '^## 12\..*logs' \
  '^## 13\..*reboot'
assert_contains "$setup_guide" 'download\.docker\.com/linux/ubuntu' \
  'VPS guide must install Docker from the official Ubuntu repository'
assert_contains "$setup_guide" 'ssh-keygen -t ed25519' 'VPS guide must create an Ed25519 deployment key'
assert_contains "$setup_guide" "ssh-keygen -t ed25519 -N ''" \
  'VPS guide must create the automation key without an interactive passphrase'
assert_contains "$setup_guide" 'ssh-keygen -lf' 'VPS guide must verify the host-key fingerprint'
assert_contains "$setup_guide" 'scp .*StrictHostKeyChecking=yes.*UserKnownHostsFile=\./next-game-crush-known-hosts.*VPS_HOST_OR_IP' \
  'VPS guide must pin the reviewed host key for the first scp connection'
assert_contains "$setup_guide" 'ssh .*StrictHostKeyChecking=yes.*UserKnownHostsFile=\./next-game-crush-known-hosts.*VPS_HOST_OR_IP' \
  'VPS guide must pin the reviewed host key for the first deploy-user SSH connection'
assert_contains "$setup_guide" 'ufw allow (22/tcp|OpenSSH)' 'VPS guide must allow SSH before enabling UFW'
assert_contains "$setup_guide" 'chmod 600 "\$env_tmp"' 'VPS guide must protect .env before atomic install'
assert_contains "$setup_guide" 'openssl rand -hex' 'VPS guide must generate hex-safe independent secrets'
assert_contains "$setup_guide" 'APP_BASE_URL=https://nextgamecrush\.site' 'VPS guide must set the public base URL'
assert_contains "$setup_guide" 'DATABASE_URL=postgres://[^ ]+@db:5432/' 'VPS guide must use the internal db service'
assert_contains "$setup_guide" 'SMTP_URL=smtp://mailpit:1025' 'VPS guide must use the internal Mailpit service'
assert_contains "$setup_guide" 'TEST_HARNESS_MODE=0' 'VPS guide must disable the test harness'
assert_contains "$setup_guide" 'DEMO_MODE_ENABLED=1' 'VPS guide must enable the approved public demo mode'
assert_contains "$setup_guide" "readonly VPS_HOST_OR_IP='nextgamecrush\.site'" \
  'VPS guide must define its host placeholder before using it'
assert_contains "$setup_guide" 'x86_64.*amd64|amd64.*x86_64' \
  'VPS guide must document the supported VPS architecture'
assert_contains "$setup_guide" 'SSH.*(port|TCP).*22|(port|TCP).*22.*SSH' \
  'VPS guide must document the port 22 deployment assumption'
assert_contains "$setup_guide" 'ISPmanager' \
  'VPS guide must explain how the detected control panel affects Nginx changes'
for catalog_setting in \
  CATALOG_HORIZON_DAYS \
  CATALOG_REFRESH_CRON \
  CATALOG_RECONCILIATION_CRON \
  CATALOG_RECONCILIATION_PAGE_SIZE
do
  assert_contains "$setup_guide" "${catalog_setting}=" \
    "VPS guide must write ${catalog_setting} to the production environment"
done
assert_contains "$setup_guide" 'VPS_SSH_KEY' 'VPS guide must configure the SSH key secret'
assert_contains "$setup_guide" 'VPS_KNOWN_HOSTS' 'VPS guide must configure the reviewed host-key secret'
assert_contains "$setup_guide" 'VPS_HOST' 'VPS guide must configure the VPS host variable'
assert_contains "$setup_guide" 'VPS_USER' 'VPS guide must configure the deployment user variable'
assert_ordered "$setup_guide" \
  '^set -Eeuo pipefail$' \
  '\[\[ ! -e "\$env_file" && ! -L "\$env_file" \]\]' \
  'trap cleanup_env_tmp EXIT' \
  'postgres_password="\$\(openssl rand -hex 32\)"' \
  '\[\[ "\$postgres_password" =~ \^\[0-9a-f\]\{64\}\$' \
  '\[\[ "\$allowed_steam_id" =~ \^\[0-9\]\{17\}\$ \]\]' \
  'env_tmp="\$\(mktemp /opt/next-game-crush/\.env\.tmp\.XXXXXX\)"' \
  'chmod 600 "\$env_tmp"' \
  '} > "\$env_tmp"' \
  'mv -fT -- "\$env_tmp" "\$env_file"' \
  '^env_tmp=$' \
  'trap - EXIT'
assert_contains "$setup_guide" 'refus(e|ing).*(existing|symlink).*(\.env)|existing or symlinked.*\.env' \
  'VPS guide must explain that existing or symlinked .env files are refused'
assert_not_contains "$setup_guide" '>[[:space:]]*/opt/next-game-crush/\.env([[:space:]]|$)' \
  'VPS guide must never write directly to the final .env path'
assert_not_contains "$setup_guide" '(\.env\.(bak|backup|old)|cp .*\.env|mv .*\.env .*\.env\.)' \
  'VPS guide must not add an .env overwrite or rotation path'

# Routine operations and destructive database recovery stay separate from bootstrap.
assert_ordered "$operations_guide" \
  '^## Normal deployment flow' \
  '^## Check status and logs' \
  '^## Backup semantics' \
  '^## Automatic image rollback' \
  '^## Diagnose a failed deployment' \
  '^## Manual database restore'
assert_contains "$operations_guide" 'backups/latest\.sql\.gz' 'operations guide must describe the single dump'
assert_contains "$operations_guide" 'automatic.*(image|application).*rollback|rolls back.*image' \
  'operations guide must describe automatic image rollback'
assert_contains "$operations_guide" '(WARNING|DANGER|DESTRUCTIVE)' \
  'manual database restore must carry an explicit warning'
assert_contains "$operations_guide" '(confirm|confirmation).*(second|again)|(second|again).*(confirm|confirmation)' \
  'manual database restore must require a second operator confirmation'
assert_contains "$operations_guide" 'compose.*stop app' 'restore procedure must stop the app'
assert_contains "$operations_guide" 'DROP DATABASE|dropdb' 'restore procedure must recreate the application database'
assert_contains "$operations_guide" 'gunzip|gzip -d' 'restore procedure must decompress latest.sql.gz'
assert_contains "$operations_guide" 'psql' 'restore procedure must load the database dump'
assert_contains "$operations_guide" 'api/health' 'restore procedure must verify public health'
assert_ordered "$operations_guide" \
  'exec 9>/opt/next-game-crush/deploy\.lock' \
  'flock -n 9' \
  'gzip -t backups/latest\.sql\.gz' \
  '\[\[ "\$app_image" =~ \^ghcr\\\.io/shimmy8/next-game-crush:' \
  'docker pull "\$app_image"' \
  "Type RESTORE DATABASE to continue" \
  'second confirmation' \
  'compose .* stop app' \
  'pg_terminate_backend' \
  'dropdb ' \
  'createdb ' \
  'gunzip -c backups/latest\.sql\.gz' \
  'compose .* up -d --no-deps --wait --wait-timeout 90 app' \
  'curl --fail --silent --show-error https://nextgamecrush\.site/api/health' \
  '^exit$'
assert_contains "$operations_guide" 'flock -n 9 \|\|' \
  'manual restore must fail closed when the deployment lock is busy'
assert_contains "$operations_guide" 'do not blindly re-run|never blindly re-run|do not use.*Re-run failed jobs.*until' \
  'operations guide must forbid a blind retry after a possible migration'

assert_contains "$readme" 'docs/vps-setup\.md' 'README must link the first-time VPS guide'
assert_contains "$readme" 'docs/deployment\.md' 'README must link the operations guide'

printf '%s\n' 'docs_test: PASS'
