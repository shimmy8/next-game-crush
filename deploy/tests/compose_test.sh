#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose_file="$repo_root/compose.prod.yml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -f "$compose_file" ]] || fail "missing production Compose file: compose.prod.yml"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
empty_env_file="$tmp_dir/empty.env"
: >"$empty_env_file"

if grep -Eq '^[[:space:]]+env_file:' "$compose_file"; then
  fail "production services must use explicit environment entries, not env_file"
fi

common_env=(
  APP_BASE_URL=https://nextgamecrush.site
  ALLOWED_STEAM_ID=76561198000000000
  STEAM_WEB_API_KEY=fixture-steam-secret
  OPENAI_API_KEY=fixture-openai-secret
  OPENAI_MODEL=gpt-6-luna
  OPENAI_EMBEDDING_MODEL=text-embedding-3-small
  POSTGRES_DB=fixture_db
  POSTGRES_USER=fixture_user
  POSTGRES_PASSWORD=fixture-database-secret
  DATABASE_URL=postgres://fixture_user:fixture-database-secret@db:5432/fixture_db?sslmode=disable
  SMTP_URL=smtp://mailpit:1025
  EMAIL_ACTION_SECRET=fixture-email-secret
  SESSION_SECRET=fixture-session-secret
  WORKER_CONCURRENCY=10
  CATALOG_DISCOVERY_ENABLED=0
  CATALOG_HORIZON_DAYS=90
  CATALOG_DISCOVERY_INTERVAL_HOURS=1
  CATALOG_DISCOVERY_HOURLY_BUDGET=200
  CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS=180
  CATALOG_DISCOVERY_PAGE_SIZE=100
  STEAM_STORE_SERVICE_BASE_URL=
)

if env -i "PATH=$PATH" "${common_env[@]}" \
  docker compose --env-file "$empty_env_file" -f "$compose_file" config --quiet \
  >"$tmp_dir/missing-image.out" 2>"$tmp_dir/missing-image.err"; then
  fail "APP_IMAGE_REF is not required"
fi

if ! grep -q 'APP_IMAGE_REF is required' "$tmp_dir/missing-image.err"; then
  fail "missing APP_IMAGE_REF does not report the required-variable error"
fi

fixture_image=ghcr.io/shimmy8/next-game-crush:0123456789abcdef0123456789abcdef01234567
postgres_error_files=()
for required_name in POSTGRES_DB POSTGRES_USER POSTGRES_PASSWORD; do
  filtered_env=()
  for setting in "${common_env[@]}"; do
    [[ "${setting%%=*}" == "$required_name" ]] || filtered_env+=("$setting")
  done
  error_file="$tmp_dir/missing-${required_name}.err"
  output_file="$tmp_dir/missing-${required_name}.out"
  postgres_error_files+=("$output_file" "$error_file")
  if env -i "PATH=$PATH" "${filtered_env[@]}" "APP_IMAGE_REF=$fixture_image" \
    docker compose --env-file "$empty_env_file" -f "$compose_file" config --quiet \
    >"$output_file" 2>"$error_file"; then
    fail "$required_name is not required"
  fi
  if ! grep -q "$required_name is required" "$error_file"; then
    fail "missing $required_name does not report the required-variable error"
  fi
done

if ! env -i "PATH=$PATH" "${common_env[@]}" "APP_IMAGE_REF=$fixture_image" \
  docker compose --env-file "$empty_env_file" -f "$compose_file" config --format json \
  >"$tmp_dir/config.json" 2>"$tmp_dir/config.err"; then
  fail "production Compose file does not render with safe fixture values"
fi

python3 - "$tmp_dir/config.json" "$fixture_image" <<'PY'
import json
import sys


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


with open(sys.argv[1], encoding="utf-8") as config_file:
    model = json.load(config_file)

services = model.get("services", {})
if set(services) != {"app", "db", "mailpit"}:
    fail("service set must be exactly app, db, and mailpit")

app = services["app"]
db = services["db"]
mailpit = services["mailpit"]

if "build" in app:
    fail("app must use a published image, not a build section")
if app.get("image") != sys.argv[2]:
    fail("app image does not resolve to the exact APP_IMAGE_REF fixture")

ports = app.get("ports", [])
if len(ports) != 1:
    fail("app must publish exactly one port")
port = ports[0]
if not (
    port.get("host_ip") == "127.0.0.1"
    and str(port.get("published")) == "8080"
    and port.get("target") == 8080
):
    fail("app must bind only 127.0.0.1:8080:8080")

if db.get("ports"):
    fail("db must not publish host ports")
if mailpit.get("ports"):
    fail("mailpit must not publish host ports")
if db.get("image") != "pgvector/pgvector:0.8.7-pg16-bookworm":
    fail("db image is not pinned to pgvector/pgvector:0.8.7-pg16-bookworm")
if mailpit.get("image") != "axllent/mailpit:v1.27":
    fail("mailpit image is not pinned to axllent/mailpit:v1.27")
if db.get("environment") != {
    "POSTGRES_DB": "fixture_db",
    "POSTGRES_PASSWORD": "fixture-database-secret",
    "POSTGRES_USER": "fixture_user",
}:
    fail("db environment does not resolve the required PostgreSQL settings")
db_health_test = db.get("healthcheck", {}).get("test", [])
if not db_health_test or "pg_isready" not in db_health_test[-1]:
    fail("db must define a PostgreSQL readiness health check")

for name, service in services.items():
    if service.get("restart") != "unless-stopped":
        fail(f"{name} must use restart: unless-stopped")

health_test = app.get("healthcheck", {}).get("test", [])
if health_test != [
    "CMD",
    "wget",
    "-q",
    "-O",
    "/dev/null",
    "http://127.0.0.1:8080/api/health",
]:
    fail("app health check must request the loopback /api/health endpoint")
if app.get("stop_grace_period") != "15s":
    fail("app stop_grace_period must be 15s")

depends_on = app.get("depends_on", {})
if depends_on.get("db", {}).get("condition") != "service_healthy":
    fail("app must wait for healthy PostgreSQL")
if depends_on.get("mailpit", {}).get("condition") != "service_started":
    fail("app must wait for started Mailpit")

db_mounts = db.get("volumes", [])
if not any(
    mount.get("type") == "volume"
    and mount.get("source") == "postgres_data"
    and mount.get("target") == "/var/lib/postgresql/data"
    for mount in db_mounts
):
    fail("db must persist data in the postgres_data volume")
if "postgres_data" not in model.get("volumes", {}):
    fail("top-level postgres_data volume is missing")

app_environment = app.get("environment", {})
expected_environment = {
    "APP_BASE_URL",
    "ALLOWED_STEAM_ID",
    "STEAM_WEB_API_KEY",
    "OPENAI_API_KEY",
    "OPENAI_MODEL",
    "OPENAI_EMBEDDING_MODEL",
    "DATABASE_URL",
    "SMTP_URL",
    "EMAIL_ACTION_SECRET",
    "SESSION_SECRET",
    "WORKER_CONCURRENCY",
    "CATALOG_DISCOVERY_ENABLED",
    "CATALOG_HORIZON_DAYS",
    "CATALOG_DISCOVERY_INTERVAL_HOURS",
    "CATALOG_DISCOVERY_HOURLY_BUDGET",
    "CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS",
    "CATALOG_DISCOVERY_PAGE_SIZE",
    "TEST_HARNESS_MODE",
}
missing_environment = expected_environment.difference(app_environment)
if missing_environment:
    fail("app is missing explicit production environment settings")
if app_environment.get("TEST_HARNESS_MODE") != "0":
    fail("TEST_HARNESS_MODE must be fixed to 0")
for unsafe_name in (
    "FIXTURE_AUTH_ENABLED",
    "STEAM_WEB_API_BASE_URL",
    "STEAM_STORE_BASE_URL",
    "STEAM_STORE_SERVICE_BASE_URL",
    "OPENAI_BASE_URL",
):
    if unsafe_name in app_environment:
        fail(f"production app must not pass test override {unsafe_name}")
PY

if grep -Eq 'fixture-(steam|openai|database|email|session)-secret' \
  "$tmp_dir/missing-image.out" "$tmp_dir/missing-image.err" \
  "${postgres_error_files[@]}" \
  "$tmp_dir/config.err"; then
  fail "Compose diagnostics exposed a secret fixture value"
fi

printf 'PASS: production Compose topology\n'
