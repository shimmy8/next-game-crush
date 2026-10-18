# Production deployment operations

This runbook covers routine operation after the one-time Ubuntu setup in
[`docs/vps-setup.md`](vps-setup.md). Commands assume the deployment account is
`ngc-deploy` and the deployment root is `/opt/next-game-crush`.

## Normal deployment flow

Every pull request and every push to `main` starts `.github/workflows/deploy.yml`:

1. `verify` checks the deployment contracts, installs the pinned Go, Node.js,
   and pnpm versions, then runs the same backend, frontend, Docker, and browser
   gates as `GOWORK=off make verify` in separately reported steps.
2. On a push to `main` only, `publish` builds the repository `Dockerfile` and
   publishes `ghcr.io/shimmy8/next-game-crush:<40-character-git-sha>`.
3. `deploy` uses the GitHub `production` Environment, uploads
   `compose.prod.yml` and `deploy/deploy.sh`, then invokes the script with that
   exact image reference over strictly verified SSH.

The production concurrency group allows only one deploy job at a time and does
not cancel an in-progress deployment. After entering that serialized group,
the job refreshes `origin/main` and skips all SSH work unless its exact commit
is still the branch head. This prevents an older run whose verification or
image build finished late from overwriting a newer deployment. On the VPS, the workflow takes
`/opt/next-game-crush/deploy.lock` before installing either deployment
artifact and keeps file descriptor 9 open across `exec` for the complete
rollout. `deploy.sh` validates that the inherited descriptor names the same
lock file; a direct manual invocation opens and takes that lock itself. This
internal `DEPLOY_LOCK_FD=9` protocol is workflow-owned and must not be set for
operator commands. A contending workflow or manual invocation fails before
changing deployment artifacts, taking a dump, running a migration, or
attempting rollback.

GitHub concurrency preserves the running production deploy and one pending
deploy. If several commits reach `main` while a deploy is running, GitHub may
replace the older pending run with the newest one. This is intentional: the
server converges to the latest verified branch head rather than deploying
every intermediate commit. If the newest branch head has not passed
verification, superseded runs leave the currently deployed version unchanged.

The commit-addressed SHA tag identifies what the workflow requested. There is
no `latest` tag and no source build on the VPS. Registry tags are technically
mutable, so package write access remains tightly scoped. The normal way to
deploy is to merge or push a reviewed commit to `main` and follow its
**Verify and deploy** workflow run.

The VPS layout is:

```text
/opt/next-game-crush/
├── .env                   # server-only secrets, mode 0600
├── backups/latest.sql.gz  # one successful pre-deploy dump
├── compose.prod.yml       # atomically installed by the workflow
├── deploy.lock            # cross-process deployment lock
├── deploy.sh              # atomically installed by the workflow
└── incoming/              # mode-protected workflow staging directories
```

## Check status and logs

These commands do not require sourcing or printing `.env`:

```bash
sudo -iu ngc-deploy docker ps --filter label=com.docker.compose.project=next-game-crush
app_container="$(sudo -iu ngc-deploy docker ps -aq --filter label=com.docker.compose.project=next-game-crush --filter label=com.docker.compose.service=app)"
db_container="$(sudo -iu ngc-deploy docker ps -aq --filter label=com.docker.compose.project=next-game-crush --filter label=com.docker.compose.service=db)"
sudo -iu ngc-deploy docker inspect --format '{{.Config.Image}} {{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$app_container"
sudo -iu ngc-deploy docker logs --tail 200 "$app_container"
sudo -iu ngc-deploy docker logs --tail 100 "$db_container"
curl --fail --silent --show-error https://nextgamecrush.site/api/health
sudo journalctl -u nginx --since '30 minutes ago' --no-pager
```

Review logs before sharing them. Application diagnostics are designed to be
sanitized, but operator review is still required.

Do not blindly re-run a failed deploy. If the new app container started, its
embedded migrations may already have changed the database; another run would
replace the only pre-deploy dump with that changed state. First inspect the job
and container logs, verify `backups/latest.sql.gz`, and decide whether the
database needs the manual restore below. A re-run is safe without that recovery
decision only when failure happened before the requested app started, such as a
denied GHCR pull.

A manual rollout, when needed after diagnosis, accepts only this repository's
lowercase 40-character SHA image:

```bash
sudo -iu ngc-deploy /opt/next-game-crush/deploy.sh ghcr.io/shimmy8/next-game-crush:0123456789abcdef0123456789abcdef01234567
```

Replace the example SHA with a commit whose published package is known to
exist. The same lock, dump, health checks, and rollback behavior apply.

## Backup semantics

Before changing the app, `deploy.sh` starts or retains PostgreSQL and Mailpit,
waits for the database, and streams `pg_dump` through gzip into a temporary
file. Only a successful dump atomically replaces:

```text
/opt/next-game-crush/backups/latest.sql.gz
```

Exactly one successful pre-deploy dump is retained; there is no history or
rotation. If `pg_dump` or gzip fails, the temporary file is removed, the
previous `latest.sql.gz` remains intact, and the app is not changed. Check its
integrity without displaying its contents:

```bash
sudo -iu ngc-deploy test -s /opt/next-game-crush/backups/latest.sql.gz
sudo -iu ngc-deploy gzip -t /opt/next-game-crush/backups/latest.sql.gz
sudo -iu ngc-deploy stat /opt/next-game-crush/backups/latest.sql.gz
```

This single local dump is a rollback aid for a study project, not an off-host
backup strategy. A disk loss removes both the database and the dump.

After a healthy rollout, image cleanup keeps the active app image and the
immediately previous rollback image, and removes older SHA-tagged images from
this repository plus dangling layers. Cleanup failure is reported but does not
turn an otherwise healthy rollout into a failure.

## Automatic image rollback

The deploy script records the running app image before pulling the requested
SHA. If the new container does not become Docker-healthy within the bounded
wait, or if `https://nextgamecrush.site/api/health` fails, it attempts an
automatic application-image rollback to the recorded image. The workflow
still fails so the operator investigates. A first deployment has no previous
image and therefore cannot roll back.

Only the image is rolled back automatically. The Go process runs embedded
migrations during startup; the database may already use the new schema. An old
binary can therefore be incompatible even when its container starts. Never
interpret an image rollback message as proof that the database was restored or
that the site is healthy.

## Diagnose a failed deployment

Start with the failed GitHub job and the first error from `deploy.sh`, then use
the matching check below:

- **Verification failed:** fix the reported repository test; no image was
  published or deployed.
- **GHCR denied the pull:** confirm the `next-game-crush` package exists, is
  public, and contains the exact workflow commit SHA.
- **SSH failed:** confirm `VPS_HOST`, `VPS_USER`, the dedicated public key, and
  the reviewed `VPS_KNOWN_HOSTS` entry. A changed host key requires independent
  fingerprint verification; never disable strict checking.
- **Another deployment is running:** inspect the active workflow and VPS
  processes. Do not delete `deploy.lock`; `flock` releases it automatically
  when the owning process exits.
- **Dump failed:** check free disk space, database health, and database logs.
  The app was not updated and the preceding successful dump was preserved.
- **Container health failed:** inspect the app and database logs plus
  `docker inspect` health output. Common causes include configuration errors or
  a failed startup migration.
- **Public health failed:** run `sudo nginx -t`, inspect the Nginx journal, and
  test the loopback endpoint on the VPS with
  `curl --fail http://127.0.0.1:8080/api/health` before testing HTTPS again.

Useful host checks:

```bash
df -h / /var/lib/docker /opt/next-game-crush
sudo systemctl status docker nginx --no-pager
sudo -iu ngc-deploy docker ps -a --filter label=com.docker.compose.project=next-game-crush
sudo ss -lntp
curl --fail --silent --show-error http://127.0.0.1:8080/api/health
curl --fail --silent --show-error https://nextgamecrush.site/api/health
```

Port `8080` must listen only on `127.0.0.1`; ports `5432`, `8025`, and `1025`
must not be published on the host.

## Manual database restore

> **DANGER — DESTRUCTIVE OPERATION:** This procedure drops and recreates only
> the `next_game_crush` application database. It permanently discards every
> database change made after `backups/latest.sql.gz`. It does not restore other
> PostgreSQL databases, `.env`, Docker images, or Nginx configuration.

Use this only after diagnosing a migration/schema incompatibility, selecting a
known compatible published image, and accepting the data-loss boundary. An
automatic image rollback does not authorize an automatic database restore.

First verify the dump. Print the current container image only as a candidate,
then enter the exact known-compatible image associated with the pre-deploy
state. The validation deliberately accepts only a commit-addressed image from this
repository, and the pull proves that it is available before any data is
destroyed:

```bash
sudo -iu ngc-deploy
```

At the new `ngc-deploy` prompt, continue with all remaining restore blocks in
the same shell so `app_image` cannot be lost or changed between steps:

```bash
cd /opt/next-game-crush
set -Eeuo pipefail
exec 9>/opt/next-game-crush/deploy.lock
flock -n 9 || {
  printf '%s\n' 'Another deployment or restore holds deploy.lock; no restore changes were made' >&2
  exit 1
}
gzip -t backups/latest.sql.gz
app_container="$(docker ps -aq --filter label=com.docker.compose.project=next-game-crush --filter label=com.docker.compose.service=app)"
if [[ -n "$app_container" ]]; then
  docker inspect --format 'Current candidate: {{.Config.Image}}' "$app_container"
fi
read -r -p 'Exact compatible ghcr.io image reference: ' app_image
[[ "$app_image" =~ ^ghcr\.io/shimmy8/next-game-crush:[0-9a-f]{40}$ ]]
docker pull "$app_image"
printf 'Verified restore image: %s\n' "$app_image"
```

File descriptor 9 stays open in this shell, so the lock is held from before
dump validation through the final public health check and `exit`. If `flock`
reports contention, stop: it fails closed before inspecting or changing the
database. Keep this shell open and do not start a workflow or manual deploy in
another session during the restore.

Now require two explicit operator confirmations. Read the warning again before
entering either phrase:

```bash
read -r -p 'Type RESTORE DATABASE to continue: ' confirm
[[ "$confirm" == 'RESTORE DATABASE' ]]
read -r -p 'Type the database name next_game_crush as second confirmation: ' confirm_again
[[ "$confirm_again" == 'next_game_crush' ]]
unset confirm confirm_again
```

Stop only the app, terminate its remaining connections, recreate only the
application database, load the dump, and restart the selected image:

```bash
APP_IMAGE_REF="$app_image" docker compose --env-file .env -f compose.prod.yml stop app
APP_IMAGE_REF="$app_image" docker compose --env-file .env -f compose.prod.yml exec -T db sh -c 'exec psql --username="$POSTGRES_USER" --dbname=postgres --set=ON_ERROR_STOP=1 --command="SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '\''$POSTGRES_DB'\'' AND pid <> pg_backend_pid()"'
APP_IMAGE_REF="$app_image" docker compose --env-file .env -f compose.prod.yml exec -T db sh -c 'exec dropdb --username="$POSTGRES_USER" --if-exists "$POSTGRES_DB"'
APP_IMAGE_REF="$app_image" docker compose --env-file .env -f compose.prod.yml exec -T db sh -c 'exec createdb --username="$POSTGRES_USER" "$POSTGRES_DB"'
gunzip -c backups/latest.sql.gz | APP_IMAGE_REF="$app_image" docker compose --env-file .env -f compose.prod.yml exec -T db sh -c 'exec psql --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --set=ON_ERROR_STOP=1'
APP_IMAGE_REF="$app_image" docker compose --env-file .env -f compose.prod.yml up -d --no-deps --wait --wait-timeout 90 app
curl --fail --silent --show-error https://nextgamecrush.site/api/health
exit
```

If any destructive or restore command fails, stop. Preserve its output and
inspect database/app logs before deciding whether to retry. Do not trigger a
new deployment blindly: its pre-deploy step could replace the only dump with
the database's current state.
