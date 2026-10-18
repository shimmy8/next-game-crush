# GitHub Actions VPS Deployment Design

**Date:** 2026-10-18

**Status:** Approved

## 1. Goal

Deploy Next Game Crush automatically to the existing Ubuntu VPS whenever a
commit reaches `main` and passes the repository's verification suite. The
application must be served at `https://nextgamecrush.site` through the VPS's
existing Nginx and TLS configuration.

The deployment remains appropriate for a single-user study project. It does
not introduce a multi-user platform, an orchestrator, or a managed deployment
service.

## 2. Current State

- The repository contains a production multi-stage `Dockerfile` that builds
  the React frontend and Go application into one runtime image.
- The Go process applies embedded PostgreSQL migrations during startup.
- The current `docker-compose.yml` is for local development: it builds locally,
  uses development database credentials, and exposes Mailpit on loopback.
- No GitHub Actions workflow or production Compose definition exists.
- `nextgamecrush.site` resolves to the VPS and the VPS currently serves
  HTTPS through Nginx.

## 3. Selected Approach

Use GitHub Actions, GitHub Container Registry (GHCR), SSH, and Docker Compose:

1. A push to `main` starts the workflow.
2. The workflow runs `make verify`.
3. A successful verification builds the existing `Dockerfile` and publishes
   `ghcr.io/shimmy8/next-game-crush:<commit-sha>`.
4. The deploy job connects to the VPS over SSH and deploys that exact image
   tag with the production Compose file.
5. Nginx continues to terminate TLS and proxies the application to
   `127.0.0.1:8080`.

This is preferred over building source on the VPS because it produces an
immutable, identifiable artifact and makes application rollback simple. A
self-hosted GitHub runner is intentionally excluded because it adds
maintenance and security exposure without providing useful value at this
scale.

## 4. Repository Artifacts

Implementation will add the following files:

- `.github/workflows/deploy.yml`: verification, image publication, and
  production deployment jobs.
- `compose.prod.yml`: production services, networks, volumes, health checks,
  and image selection by commit tag.
- `deploy/deploy.sh`: serialized remote deployment, database dump, health
  checks, and image rollback.
- `deploy/nginx.conf.example`: the expected reverse-proxy configuration for
  `nextgamecrush.site`.
- `docs/deployment.md`: workflow behavior, GitHub configuration, operations,
  logs, rollback, and database recovery.
- `docs/vps-setup.md`: a separate first-time VPS bootstrap runbook.

The existing local `docker-compose.yml` remains the local-development entry
point and is not repurposed for production.

## 5. Production Topology

The production Compose project contains:

- `app`: the GHCR image for one exact Git commit, bound only to
  `127.0.0.1:8080` and configured with `restart: unless-stopped`.
- `db`: the pinned PostgreSQL 16 plus pgvector image, reachable only on the
  internal Compose network and backed by a named volume.
- `mailpit`: an internal SMTP sink required by the current application
  configuration. It has no host ports and is not exposed through Nginx.

Only Nginx accepts public HTTP and HTTPS traffic. PostgreSQL and Mailpit have no
public or host-bound ports. The app is reachable from the host only over
loopback.

The application uses:

```text
APP_BASE_URL=https://nextgamecrush.site
DATABASE_URL=postgres://<user>:<password>@db:5432/<database>?sslmode=disable
SMTP_URL=smtp://mailpit:1025
```

The internal database connection does not use TLS because traffic remains in
the private Docker network. Browser traffic is always HTTPS through Nginx.

## 6. Configuration and Secrets

Application secrets live only in `/opt/next-game-crush/.env` on the VPS, owned
by the deployment user with mode `0600`. They are not copied into GitHub and
are not logged by the workflow.

The VPS file contains at least:

- `APP_BASE_URL`
- `ALLOWED_STEAM_ID`
- `STEAM_WEB_API_KEY`
- `OPENAI_API_KEY` when enrichment is enabled
- `OPENAI_MODEL`
- `OPENAI_EMBEDDING_MODEL`
- `POSTGRES_DB`
- `POSTGRES_USER`
- `POSTGRES_PASSWORD`
- `DATABASE_URL`
- `EMAIL_ACTION_SECRET`
- `SESSION_SECRET`
- worker and catalog-discovery settings when their defaults are changed

GitHub uses a `production` Environment containing:

- secret `VPS_SSH_KEY`
- secret `VPS_KNOWN_HOSTS`
- variable `VPS_HOST` with the VPS hostname or IP address
- variable `VPS_USER` naming the dedicated VPS deployment account

The workflow authenticates to GHCR with its short-lived `GITHUB_TOKEN` and
requests only the permissions needed to read repository contents and publish
packages. The first published container package must be changed to public
visibility so that the VPS can pull it without a long-lived registry token.

## 7. Workflow and Concurrency

The workflow runs for pull requests and pushes so verification results are
visible before merge. Publication and deployment run only for a push to
`main`.

Jobs execute in this order:

1. `verify`: check out the exact commit and run the repository's required
   release verification.
2. `publish`: build the existing Dockerfile and push a commit-SHA tag to GHCR.
3. `deploy`: copy the versioned production artifacts, connect over SSH, and
   deploy the published SHA.

The production deploy job uses a single concurrency group with cancellation
disabled. A later deploy waits rather than interrupting a database dump,
migration, or health check already in progress.

Official GitHub and Docker actions are pinned to reviewed immutable commit
SHAs rather than floating third-party tags. The SSH deployment uses the
runner's OpenSSH client; no third-party SSH action is required.

## 8. Deployment Procedure

The remote script performs these steps under `/opt/next-game-crush`:

1. Validate that the image tag, Compose file, and protected `.env` exist.
2. Start or retain the database and internal Mailpit services.
3. Wait until PostgreSQL reports healthy.
4. Create a compressed dump in a temporary file with the PostgreSQL container's
   `pg_dump`.
5. Replace `backups/latest.sql.gz` only after `pg_dump` succeeds. No historical
   backups are retained.
6. Record the currently configured app image for possible rollback.
7. Pull the new commit-SHA image and update the app service.
8. Wait for the app container health check.
9. Verify the public `https://nextgamecrush.site/api/health` endpoint.
10. Remove unreferenced old application images conservatively, without
    deleting the image required for the current rollback.

The script exits before changing the application when the dump fails. It never
prints `.env`, connection strings, or application secrets.

## 9. Failure and Rollback Behavior

If the new app container does not become healthy or the public endpoint fails,
the deploy script restores the previous app image tag and starts the app again.
It then reports a failed GitHub Actions deployment even if application rollback
succeeds.

Database restore is never automatic. A startup migration might have changed
the schema, and automatically loading a dump could destroy valid writes or
hide the original failure. `docs/deployment.md` documents an explicit manual
restore from `backups/latest.sql.gz` for the operator to use after inspecting
the failure.

The design therefore provides best-effort application rollback, not zero-risk
transactional rollback across both image and schema. This trade-off is
acceptable for the single-user study deployment.

## 10. Nginx and Network Security

The existing Nginx and TLS certificate remain in place. The site configuration
proxies all paths for `nextgamecrush.site` to `http://127.0.0.1:8080` and sets
the normal `Host`, client-address, and forwarded-protocol headers.

The VPS firewall permits only SSH, HTTP, and HTTPS. Docker does not publish the
database or Mailpit ports. SSH host verification is mandatory: the workflow
constructs `known_hosts` from the reviewed `VPS_KNOWN_HOSTS` secret and uses
strict host-key checking.

The deployment account uses a dedicated SSH key. Because membership in the
Docker group is effectively privileged host access, the account is used only
for deployment and is not shared with application users.

## 11. VPS Bootstrap Runbook

`docs/vps-setup.md` is a standalone, ordered procedure covering:

1. OS updates and creation of the deployment account.
2. SSH key installation and host-key fingerprint capture.
3. Docker Engine and Compose plugin installation from Docker's official Ubuntu
   repository.
4. Firewall rules for ports 22, 80, and 443.
5. Creation and ownership of `/opt/next-game-crush` and its `backups`
   directory.
6. Creation of the protected application `.env`, including generation of
   independent session, email-action, and database secrets.
7. Nginx proxy configuration and validation without replacing the existing TLS
   certificate unexpectedly.
8. GitHub Environment secrets and variables.
9. First image publication, changing the linked GHCR package to public, and
   re-running the deploy job if its first anonymous pull was blocked.
10. First health check, login verification, container log inspection, and
    reboot-survival check.

The runbook uses placeholders for secrets and never instructs the operator to
paste secret values into Git history or workflow logs.

## 12. Verification and Acceptance Criteria

Implementation verification includes:

- `make verify` for application, migration, frontend, Docker, and end-to-end
  regression coverage.
- `docker compose -f compose.prod.yml config --quiet` with non-secret fixture
  environment values.
- a shell syntax check for `deploy/deploy.sh`.
- tests or a local harness for image selection, dump failure, unhealthy app,
  successful deployment, and application-image rollback behavior.
- `git diff --check` for all added deployment and documentation files.

The deployment is accepted when:

1. A push to `main` cannot publish or deploy after failed verification.
2. A successful push publishes a SHA-tagged image and deploys it automatically.
3. `https://nextgamecrush.site/api/health` responds through Nginx after deploy.
4. PostgreSQL, the app port, and Mailpit are not publicly exposed except for
   the app's loopback binding consumed by Nginx.
5. The VPS contains exactly one successful pre-deploy dump at
   `backups/latest.sql.gz`.
6. A failed application rollout attempts to restore the previous image and
   leaves the workflow failed for operator attention.
7. A new operator can prepare the Ubuntu VPS by following
   `docs/vps-setup.md` without relying on undocumented steps.
