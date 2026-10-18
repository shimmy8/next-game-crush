# Next Game Crush repository instructions

## Project intent

Next Game Crush is a local-first, single-user Steam release recommender. It imports the allowlisted user's Steam activity, preserves release-date precision, builds deterministic recommendations with optional OpenAI enrichment, learns from feedback, and sends local digests through Mailpit.

Keep the local MVP usable when optional upstreams are unavailable. Do not expand it into a multi-user service or a full Steam catalog unless the user explicitly changes the scope.

## Sources of truth

- Start with `README.md` for setup, commands, and operational behavior.
- Use `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md` for product and architecture decisions.
- Use `docs/superpowers/plans/2026-10-17-next-game-crush-mvp.md` for the implemented phase boundaries and acceptance criteria.
- Use `docs/local-demo.md` for real-account verification and troubleshooting.
- Treat live code, migrations, generated SQL bindings, and observed test output as evidence of current behavior. Plans describe intent, not proof.

## Subagent workflow

- Perform every task in this repository in subagent mode: delegate the work to at least one subagent, and use the parent agent only for coordination and final reporting.

## Architecture and implementation rules

- Use Go 1.27.2 with `GOWORK=off`. The backend is a modular monolith that serves the API, worker, and built React application.
- PostgreSQL with `pgvector` is the only persistent service. The queue is PostgreSQL-backed; do not introduce Redis or another datastore without approval.
- Keep recommendation ranking deterministic. OpenAI may extract structured features, create embeddings and summaries, classify feedback, and write grounded explanations, but it must not become the final ranking authority.
- Preserve the last successful upstream-derived state when a Steam or OpenAI call fails.
- Represent private, unavailable, malformed, and failed Steam responses explicitly. Never convert them into a successful empty library, wishlist, or achievement set.
- Preserve release precision. Exact dates belong on calendar days; month, quarter, year, Coming Soon, and TBA values stay in separate buckets. Never invent an exact date.
- SMTP is not exactly-once. Preserve the `ambiguous` delivery state and never automatically resend an attempt whose acceptance is uncertain.
- Keep API and frontend contracts aligned. Update Go handlers/types, `web/src/api/types.ts`, normalization, and contract tests together.
- Add a new numbered migration for schema changes. Do not rewrite an already applied migration.
- Do not edit `internal/store/sqlc/` by hand. Change `db/queries/` or `sqlc.yaml`, then run `make sqlc` and commit the generated result.
- Keep test-only authentication and upstream overrides fail-closed behind `TEST_HARNESS_MODE=1`.

## CodeGraph workflow

CodeGraph is available as a navigation and impact-analysis aid. Its index is local evidence, not a substitute for reading the relevant source or running tests.

- Before broad discovery, run `codegraph status .`. If the index is stale, run `codegraph sync .`.
- For a non-trivial task, start with `codegraph context "<task>" -p .` to identify relevant symbols and relationships.
- Use `codegraph explore <area> -p .` for an unfamiliar flow, `codegraph node <symbol> -p .` for source plus callers/callees, and `codegraph impact <symbol> -p .` before changing shared behavior.
- Use `codegraph affected <changed-files...> -p .` to help select focused regression tests. It supplements rather than replaces the repository's required verification.
- After source changes, run `codegraph sync .` before relying on further graph results.
- Never commit `.codegraph/codegraph.db`, daemon state, sockets, logs, or other generated CodeGraph data. Keep `.codegraph/.gitignore` tracked so those files remain local.
- If CodeGraph output conflicts with source code, generated bindings, or test results, trust the latter and refresh the index.

## Testing and verification

- Write or update the smallest useful failing test before changing behavior, then verify the focused test passes.
- Use the isolated `TEST_DATABASE_URL` and the Compose `test-db` service for database tests. Never recreate or clean the application database from a test.
- Prefer `codegraph affected` plus package knowledge for focused tests, then run broader tests for shared queues, auth, sync, ranking, or lifecycle changes.
- For backend changes, run the relevant Go tests with `-race`; use `GOWORK=off` for direct Go commands.
- For frontend changes, run `pnpm --dir web run test`, `typecheck`, `lint`, and `build`.
- During implementation, prefer the smallest relevant unit and integration tests. Do not rerun the full browser E2E suite after every small edit; run focused E2E only after the related end-to-end user workflow is complete or when a browser-only behavior needs proof.
- For cross-cutting, security-sensitive, persistence, workflow, Docker, or release-ready changes, run `make verify`. If the pinned Playwright Chromium download is unavailable, use the documented explicit system-Chrome fallback and report it.
- Run one fresh full `make verify` as the final release gate after the implementation and focused reviews have stabilized. Repeat it only when a subsequent change can affect that gate or when the previous run was interrupted or invalidated.
- For documentation-only changes, run `git diff --check` and validate every referenced command or path that changed.
- Do not claim a fix or completion from an agent report alone. Inspect the diff and report fresh command output with zero failures.

## Security and data handling

- Never read, print, log, commit, or paste `.env`, Steam/OpenAI keys, session cookies, CSRF tokens, signing secrets, raw prompts, or upstream response bodies. Use `.env.example` for variable names.
- Keep Steam/OpenAI keys server-side and use HTTPS for real upstream requests.
- Maintain the single-user SteamID allowlist on login and authenticated requests.
- Require authentication and CSRF protection for browser mutations. Preserve signed-action purpose, expiry, nonce, and replay protections.
- Sanitize diagnostics and errors. Do not expose SteamID values, job payloads, secrets, or personal data.
- Preserve deletion fencing/tombstones so in-flight workers cannot recreate deleted user data.

## Scope and repository hygiene

- Preserve unrelated user changes and untracked files. Do not modify `docs/steam-release-recommender-idea.md` unless the user explicitly asks.
- Avoid unrelated refactors, dependency upgrades, or architecture changes while completing a bounded task.
- Do not push, deploy, expose credentials, or make external writes without explicit user authorization.
- Keep commits scoped when the user asks for commits; never include `.env`, CodeGraph data, build output, or unrelated files.
