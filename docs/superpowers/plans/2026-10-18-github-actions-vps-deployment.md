# GitHub Actions VPS Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically verify, package, and deploy each successful `main` commit to the existing Ubuntu VPS serving `https://nextgamecrush.site`.

**Architecture:** GitHub Actions runs the repository verification suite, publishes one immutable commit-SHA image to GHCR, then copies versioned deployment files to the VPS and invokes a guarded SSH deployment. Docker Compose runs the app, PostgreSQL, and an internal Mailpit; existing Nginx terminates TLS and proxies to the app's loopback port.

**Tech Stack:** GitHub Actions, Docker Buildx, GHCR, Bash, Docker Compose, PostgreSQL 16 with pgvector, Nginx, OpenSSH

**Spec:** `docs/superpowers/specs/2026-10-18-github-actions-vps-deployment-design.md`

## Global Constraints

- Preserve the local `docker-compose.yml`; production uses a separate `compose.prod.yml`.
- Publish `ghcr.io/shimmy8/next-game-crush:<40-character-git-sha>` and deploy only that exact image.
- Use Go 1.27.2 with `GOWORK=off`, Node.js 26, and pnpm 11.25.0.
- Run `make verify` before publication or deployment.
- Keep `/opt/next-game-crush/.env` only on the VPS with mode `0600`; never copy application secrets to GitHub.
- Expose only Nginx publicly; bind the app to `127.0.0.1:8080` and publish no PostgreSQL or Mailpit ports.
- Keep exactly one successful pre-deploy dump at `/opt/next-game-crush/backups/latest.sql.gz`.
- Automatically roll back only the application image; database restore is always manual.
- Preserve `TEST_HARNESS_MODE=0` and never configure test upstream overrides in production.
- Do not edit generated `internal/store/sqlc/` files or existing migrations.
- Preserve all unrelated tracked and untracked user changes.

## Review Focus

- First deployment has no current app container: create the dump from the initialized database, deploy successfully, and skip image rollback because no previous image exists; covered by Task 2's `test_first_deploy_without_previous_image`.
- A failed `pg_dump` must leave both `backups/latest.sql.gz` and the running app unchanged; covered by Task 2's `test_dump_failure_stops_before_pull`.
- A new container can be Docker-healthy while the public Nginx route is broken: treat public health failure as a failed rollout and restore the previous image; covered by Task 2's `test_public_health_failure_rolls_back_image`.
- Concurrent workflow or manual deployments must not overlap dump, migration, or rollback work; covered by Task 2's `test_lock_contention_fails_without_changes` and Task 3's concurrency assertions.
- A malformed or attacker-controlled image argument must never reach Docker or the shell: reject every value outside the exact GHCR repository plus 40 lowercase hexadecimal SHA; covered by Task 2's `test_rejects_invalid_image_reference`.

---

### Task 1: Production Compose topology

**Files:**
- Create: `compose.prod.yml`
- Create: `deploy/tests/compose_test.sh`

**Interfaces:**
- Consumes: `/opt/next-game-crush/.env` values named in the spec and `APP_IMAGE_REF` supplied by the deploy script.
- Produces: Compose services `app`, `db`, and `mailpit`; named volume `postgres_data`; app health check at `http://127.0.0.1:8080/api/health`.

- [ ] **Step 1: Write `deploy/tests/compose_test.sh` with failing topology assertions**

  Render the model with safe fixture values and assert that it requires `APP_IMAGE_REF`, resolves the exact fixture image, binds only `127.0.0.1:8080`, publishes no `db` or `mailpit` ports, uses the pinned `pgvector/pgvector:0.8.7-pg16-bookworm` and `axllent/mailpit:v1.27` images, defines the persistent database volume, and configures app health plus `restart: unless-stopped`.

- [ ] **Step 2: Run the topology test and verify it fails because `compose.prod.yml` is absent**

  Run: `bash deploy/tests/compose_test.sh`

  Expected: FAIL naming the missing production Compose file.

- [ ] **Step 3: Create `compose.prod.yml`**

  Define `app`, `db`, and `mailpit` without a `build` section. Require `${APP_IMAGE_REF:?APP_IMAGE_REF is required}` for `app.image`; pass application settings explicitly instead of using `env_file` on the app container; bind `127.0.0.1:8080:8080`; retain the existing app health command; set `stop_grace_period: 15s`; and wait for healthy PostgreSQL plus started Mailpit. Configure PostgreSQL from required `POSTGRES_DB`, `POSTGRES_USER`, and `POSTGRES_PASSWORD` interpolations, use the named volume, and publish no database or Mailpit ports.

- [ ] **Step 4: Run the topology test and Compose validation**

  Run: `bash deploy/tests/compose_test.sh`

  Expected: PASS with no output containing a secret fixture value.

- [ ] **Step 5: Commit the production topology**

```bash
git add compose.prod.yml deploy/tests/compose_test.sh
git commit -m "deploy: add production Compose topology"
```

### Task 2: Guarded remote deployment and rollback

**Files:**
- Create: `deploy/deploy.sh`
- Create: `deploy/tests/deploy_test.sh`

**Interfaces:**
- Consumes: one positional full image reference matching `ghcr.io/shimmy8/next-game-crush:[0-9a-f]{40}`, `DEPLOY_ROOT` defaulting to `/opt/next-game-crush`, `PUBLIC_HEALTH_URL` defaulting to `https://nextgamecrush.site/api/health`, and the Task 1 Compose file.
- Produces: atomic `backups/latest.sql.gz`, a running app on the requested image, nonzero exit after any failed rollout, and best-effort restoration of the previously running image.

- [ ] **Step 1: Write a command-mocking deployment test harness**

  In `deploy/tests/deploy_test.sh`, create a temporary deploy root and prepend mock `docker`, `curl`, and `flock` programs to `PATH`. Capture commands in a log without printing fixture secrets. Add helpers to configure the current image, dump exit status, container health outcome, public health outcome, and lock availability.

- [ ] **Step 2: Add and run the input and concurrency tests**

  Add `test_rejects_invalid_image_reference` and `test_lock_contention_fails_without_changes`. Assert nonzero status, no Docker invocation for invalid input, and no dump, pull, or Compose update while the lock is held.

  Run: `bash deploy/tests/deploy_test.sh`

  Expected: FAIL because `deploy/deploy.sh` is absent.

- [ ] **Step 3: Implement validation, strict mode, and locking in `deploy/deploy.sh`**

  Use Bash with `set -Eeuo pipefail`, `umask 077`, exact image-reference validation, validated regular-file checks for `.env` and `compose.prod.yml`, and `flock` on `$DEPLOY_ROOT/deploy.lock`. Never source `.env`; pass it only through Docker Compose's `--env-file` option.

- [ ] **Step 4: Run the input and concurrency tests**

  Run: `bash deploy/tests/deploy_test.sh`

  Expected: the two implemented tests PASS.

- [ ] **Step 5: Add `test_dump_failure_stops_before_pull` and atomic backup assertions**

  Seed an existing `backups/latest.sql.gz`, force mocked `pg_dump` failure, and assert the original file remains byte-identical and the log contains no app pull or update.

- [ ] **Step 6: Run the new dump test and verify it fails**

  Run: `bash deploy/tests/deploy_test.sh`

  Expected: FAIL because the script does not yet create a guarded dump.

- [ ] **Step 7: Implement database startup and the single atomic dump**

  Start only `db` and `mailpit` with the requested image supplied for Compose interpolation, wait for PostgreSQL health, run the container's `pg_dump` through gzip into `backups/latest.sql.gz.tmp.*`, and rename it to `latest.sql.gz` only after the complete pipeline succeeds. Remove the temporary file on every exit path.

- [ ] **Step 8: Run the deployment tests**

  Run: `bash deploy/tests/deploy_test.sh`

  Expected: input, lock, and dump tests PASS.

- [ ] **Step 9: Add rollout behavior tests**

  Add `test_first_deploy_without_previous_image`, `test_successful_upgrade_uses_exact_image`, and `test_public_health_failure_rolls_back_image`. Assert the script inspects the current app before replacement, pulls and starts only the requested image, waits for Docker health, calls the exact public health URL, skips rollback on a first deployment, and restores the inspected previous image after a public-health failure while still returning nonzero.

- [ ] **Step 10: Run the rollout tests and verify they fail**

  Run: `bash deploy/tests/deploy_test.sh`

  Expected: FAIL on missing rollout and rollback commands.

- [ ] **Step 11: Implement rollout, public verification, and best-effort image rollback**

  Read the previous full image reference from `docker inspect` when an app container exists. Pull the requested app image, run `docker compose up -d --no-deps --wait app`, then call `curl --fail --silent --show-error` with bounded connect and total timeouts. On either health failure, start the previous image when present, verify its container health, and return failure regardless of rollback success. Finish successful deploys with dangling-image pruning only; do not delete tagged rollback candidates.

- [ ] **Step 12: Run syntax and behavior verification**

  Run: `bash -n deploy/deploy.sh deploy/tests/deploy_test.sh && bash deploy/tests/deploy_test.sh`

  Expected: PASS for every test and no secret fixture value in output.

- [ ] **Step 13: Commit the deployment script**

```bash
git add deploy/deploy.sh deploy/tests/deploy_test.sh
git commit -m "deploy: add guarded VPS rollout and rollback"
```

### Task 3: GitHub Actions verification, publication, and deployment

**Files:**
- Create: `.github/workflows/deploy.yml`
- Create: `deploy/tests/workflow_test.sh`

**Interfaces:**
- Consumes: GitHub `production` Environment secrets `VPS_SSH_KEY` and `VPS_KNOWN_HOSTS`; variables `VPS_HOST` and `VPS_USER`; Tasks 1 and 2 deployment files.
- Produces: PR verification; automatic `main` publication to `ghcr.io/shimmy8/next-game-crush:${GITHUB_SHA}`; serialized production SSH deployment.

- [ ] **Step 1: Write failing static workflow assertions**

  In `deploy/tests/workflow_test.sh`, assert PR and `main` push triggers, job ordering `verify -> publish -> deploy`, job-scoped minimal permissions, `production` environment URL, `concurrency.group: production`, `cancel-in-progress: false`, exact SHA image tagging, strict SSH host checking, absence of third-party SSH actions, and immutable action pins.

- [ ] **Step 2: Run the workflow test and verify it fails**

  Run: `bash deploy/tests/workflow_test.sh`

  Expected: FAIL because `.github/workflows/deploy.yml` is absent.

- [ ] **Step 3: Create the `verify` job**

  Use `ubuntu-latest`, `actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1` (`v7.0.1`), `actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` (`v7.0.0`) with Go `1.27.2`, and `actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1` (`v7.1.0`) with Node `26`. Install `pnpm@11.25.0`, run `pnpm --dir web install --frozen-lockfile`, use `/usr/bin/google-chrome` only when executable, then run `make verify` with `GOWORK=off`.

- [ ] **Step 4: Create the `publish` job**

  Restrict it to pushes on `main`, depend on `verify`, grant `contents: read` and `packages: write`, log in to `ghcr.io` with `GITHUB_TOKEN`, and publish the exact SHA tag. Pin `docker/login-action@dbcb813823bdd20940b903addbd779551569679f` (`v4.6.0`), `docker/setup-buildx-action@d7f5e7f509e45cec5c76c4d5afdd7de93d0b3df5` (`v4.1.0`), and `docker/build-push-action@c3c9e263c25d99ce0380d002d59b67737d91b0dc` (`v7.4.0`). Enable GitHub Actions layer caching and attach the source-repository OCI label.

- [ ] **Step 5: Create the `deploy` job**

  Depend on `publish`, use the `production` environment with URL `https://nextgamecrush.site`, and serialize with cancellation disabled. Write the SSH private key and reviewed `known_hosts` secret to mode-protected files, reject empty variables, create an incoming directory over SSH, copy `compose.prod.yml` and `deploy/deploy.sh`, install them atomically under `/opt/next-game-crush`, and invoke the script with `ghcr.io/shimmy8/next-game-crush:${GITHUB_SHA}`. Use only OpenSSH `ssh` and `scp` with `StrictHostKeyChecking=yes`.

- [ ] **Step 6: Run static workflow and shell checks**

  Run: `bash -n deploy/tests/workflow_test.sh && bash deploy/tests/workflow_test.sh`

  Expected: PASS and every external action reference is a 40-character commit SHA.

- [ ] **Step 7: Lint the workflow with actionlint**

  Run: `docker run --rm -v "$PWD:/repo" -w /repo rhysd/actionlint:1.7.7`

  Expected: exit 0 with no workflow diagnostics.

- [ ] **Step 8: Commit the workflow**

```bash
git add .github/workflows/deploy.yml deploy/tests/workflow_test.sh
git commit -m "ci: deploy verified main images to VPS"
```

### Task 4: Nginx example and operator documentation

**Files:**
- Create: `deploy/nginx.conf.example`
- Create: `docs/deployment.md`
- Create: `docs/vps-setup.md`
- Create: `deploy/tests/docs_test.sh`
- Modify: `README.md` after the local-run section

**Interfaces:**
- Consumes: Tasks 1-3 file names, environment names, image format, paths, and script interface.
- Produces: a standalone first-time Ubuntu runbook and a separate ongoing deployment/rollback runbook.

- [ ] **Step 1: Write failing documentation and Nginx contract checks**

  In `deploy/tests/docs_test.sh`, assert that the Nginx example names only `nextgamecrush.site`, proxies to `127.0.0.1:8080`, forwards `Host`, client address, and protocol, and contains no Mailpit or PostgreSQL route. Assert the VPS guide contains ordered sections for deploy user, official Docker repository, SSH key, fingerprint, UFW, directories, protected `.env`, Nginx validation, GitHub Environment, GHCR visibility, first deployment, logs, and reboot verification. Assert the operations guide contains normal deploy flow, single-dump semantics, image rollback, manual database restore warning and commands, and failure diagnosis.

- [ ] **Step 2: Run the documentation test and verify it fails**

  Run: `bash deploy/tests/docs_test.sh`

  Expected: FAIL naming the missing files.

- [ ] **Step 3: Create `deploy/nginx.conf.example`**

  Provide the HTTPS server block fragment for the existing certificate setup. Proxy `/` to `http://127.0.0.1:8080`, set `Host`, `X-Real-IP`, `X-Forwarded-For`, and `X-Forwarded-Proto`, and include practical proxy timeouts without defining a second TLS certificate lifecycle.

- [ ] **Step 4: Write the standalone `docs/vps-setup.md` bootstrap guide**

  Give copyable Ubuntu commands for a dedicated deploy user, Ed25519 key authorization, official Docker Engine plus Compose plugin installation, Docker-group warning, UFW ports 22/80/443, `/opt/next-game-crush/backups` ownership, and `.env` mode `0600`. Generate independent hex-safe PostgreSQL, session, and email-action secrets; use `APP_BASE_URL=https://nextgamecrush.site`, internal `db` and `mailpit` URLs, and `TEST_HARNESS_MODE=0`. Include Nginx backup/edit/`nginx -t`/reload steps, GitHub `production` Environment setup, host fingerprint verification, first GHCR publication and public-visibility step, first deploy rerun, health/login/log checks, and reboot survival.

- [ ] **Step 5: Write `docs/deployment.md`**

  Explain the three jobs, SHA image identity, concurrency, VPS file layout, normal logs and status commands, the single atomic pre-deploy dump, automatic image-only rollback, and why database restore is manual. Provide a guarded restore procedure that stops `app`, recreates only the application database from `latest.sql.gz`, restarts the selected app image, and verifies health; prefix it with an explicit destructive-operation warning and a second operator confirmation step.

- [ ] **Step 6: Link production documentation from `README.md`**

  Add a short production deployment paragraph that points to `docs/vps-setup.md` for first-time setup and `docs/deployment.md` for routine operation. Do not duplicate the runbooks or change local setup instructions.

- [ ] **Step 7: Run documentation checks**

  Run: `bash -n deploy/tests/docs_test.sh && bash deploy/tests/docs_test.sh && git diff --check`

  Expected: PASS with no broken repository-relative file references or whitespace errors.

- [ ] **Step 8: Commit the operator documentation**

```bash
git add deploy/nginx.conf.example docs/deployment.md docs/vps-setup.md deploy/tests/docs_test.sh README.md
git commit -m "docs: add VPS deployment runbooks"
```

### Task 5: Integrated verification and handoff

**Files:**
- Modify only if a verification failure exposes a defect: files created in Tasks 1-4

**Interfaces:**
- Consumes: all prior task deliverables.
- Produces: verified deployment artifacts ready for GitHub configuration and VPS bootstrap.

- [ ] **Step 1: Refresh CodeGraph and inspect affected areas**

  Run: `codegraph sync . && codegraph affected compose.prod.yml deploy/deploy.sh .github/workflows/deploy.yml README.md -p .`

  Expected: successful sync; use any reported source relationships to supplement, not replace, the required checks.

- [ ] **Step 2: Run all deployment-specific tests**

  Run: `bash deploy/tests/compose_test.sh && bash deploy/tests/deploy_test.sh && bash deploy/tests/workflow_test.sh && bash deploy/tests/docs_test.sh`

  Expected: PASS for every suite.

- [ ] **Step 3: Run shell, Compose, workflow, and diff validation**

  Run: `bash -n deploy/deploy.sh deploy/tests/*.sh`

  Expected: exit 0.

  Run: `docker run --rm -v "$PWD:/repo" -w /repo rhysd/actionlint:1.7.7`

  Expected: exit 0.

  Run: `git diff --check`

  Expected: exit 0.

- [ ] **Step 4: Run the repository release verification**

  Run: `GOWORK=off make verify`

  Expected: zero failures across migration checks, Go race tests, frontend tests/typecheck/lint/build, Docker build, and Playwright smoke test. If the pinned browser download is unavailable, rerun with the documented explicit system-Chrome fallback and record that path in the handoff.

- [ ] **Step 5: Inspect the final diff and security-sensitive values**

  Run: `git diff --stat && git diff -- .github/workflows/deploy.yml compose.prod.yml deploy docs/deployment.md docs/vps-setup.md README.md`

  Expected: only scoped deployment changes; no secret values, private keys, `.env`, test-only upstream overrides, publicly bound database/Mailpit ports, or unrelated user files.

- [ ] **Step 6: Commit any verification-only corrections**

```bash
git add .github/workflows/deploy.yml compose.prod.yml deploy docs/deployment.md docs/vps-setup.md README.md
git commit -m "deploy: finalize verified VPS delivery"
```

  Skip this commit when verification required no corrections.

- [ ] **Step 7: Hand off the two manual activation phases**

  Report fresh verification output and identify the exact commit to push. Then direct the operator to follow `docs/vps-setup.md`: bootstrap the VPS and GitHub Environment first, push the deployment commit second, make the first GHCR package public if the initial anonymous pull is denied, rerun the failed deploy job, and verify `https://nextgamecrush.site/api/health` plus Steam login. Do not push or modify the VPS without separate explicit authorization.
