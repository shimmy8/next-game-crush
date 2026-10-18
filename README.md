# Next Game Crush

Next Game Crush is a local, single-user Steam release recommender. It imports public Steam activity, keeps release-date precision intact, builds deterministic recommendations with optional OpenAI enrichment, collects feedback, and sends a local test digest to Mailpit.

## Prerequisites

- Go **1.27.2** (commands run with `GOWORK=off` because a parent workspace may exist)
- Node.js 26 and pnpm 11
- Docker with Docker Compose

Copy `.env.example` to `.env` and fill only the empty values. Required secrets are `STEAM_WEB_API_KEY`, `EMAIL_ACTION_SECRET`, and `SESSION_SECRET`; `OPENAI_API_KEY` is optional. Generate signing/session secrets locally and never commit `.env`. `ALLOWED_STEAM_ID` must be the one 17-digit SteamID allowed to log in.

`APP_BASE_URL` must exactly match the browser-visible origin because it anchors Steam callbacks, signed links, and CSRF origin checks. Use the Go origin (`http://localhost:8080`) for `make dev`; for split development opened through Vite, set it to the Vite URL (`http://localhost:5173`) and let Vite proxy `/api`, `/auth`, and `/actions` to Go.

Steam OpenID proves account ownership but does not unlock private data. Set Steam **Game details** and wishlist visibility to Public if you want them imported. Private or unavailable resources appear as unavailable/stale while the last successful snapshot remains usable.

Completed games are inferred automatically from Steam achievements; there is no manual “completed” control. A first unlocked ending or full achievement completion is a durable historical taste signal, and distinct additional endings strengthen it up to a bounded maximum. This lets a short completed story game count at least as strongly as the maximum two-week activity signal, without pretending that historical completion is recent play. Direct Dislike/Exclude feedback can suppress its positive use without deleting the history.

The import first uses localized English metadata returned with `GetPlayerAchievements`; when a successful response lacks names or descriptions it asks `GetSchemaForGame` only for that game. Literal completion/ending wording is classified locally before optional OpenAI semantic classification. Steam or OpenAI failure preserves the last successful achievement snapshot, semantic generation, completion projection, profile, and ranking; unresolved new metadata remains unknown instead of being treated as “not completed”.

## Run locally

```bash
cp .env.example .env
# edit .env without committing it
make dev
```

Open <http://localhost:8080>. Mailpit is at <http://localhost:8025>. The production Docker image builds the frontend, copies it into `/app/web/dist`, and embeds SQL migrations in the Go binary, so the same container serves assets and migrates the database at startup.

To expose a public read-only view of the saved allowlisted account, set `DEMO_MODE_ENABLED=1` and restart the app. The login screen then shows a separate dark demo button. A demo session reads the owner's current data but cannot synchronize, change preferences, submit reactions or reminders, send a digest, or delete data; the server rejects those mutations even if a client bypasses the UI. Demo login is unavailable until the owner has signed in at least once.

For split development, start PostgreSQL and Mailpit with `docker compose up -d db mailpit`, set `APP_BASE_URL=http://localhost:5173` when running `GOWORK=off go run ./cmd/next-game-crush`, then run `pnpm --dir web dev` and open <http://localhost:5173>.

The reviewed bounded fallback catalog lives in `catalog_seed.json`. Edit AppIDs, source URLs, and observed dates together; do not replace review with HTML scraping. Optional Steam catalog discovery is disabled by default (`CATALOG_DISCOVERY_ENABLED=0`). When explicitly enabled it incrementally adds qualified upcoming games to the same shared candidate union while the seed remains available as a fallback. It is a bounded best-effort search, not a completeness promise: defaults are a 90-day horizon, hourly cadence, 200 unique qualifications per hour, 180-day freshness for unknown/Coming Soon releases, and 100 items per page. Supported bounds are documented in `.env.example`.

Discovery starts from a recent watermark and slowly rolls backward, so coverage grows over time. Transport retries reuse durable page/qualification identities rather than spending a new unique qualification for every HTTP attempt. Safe aggregate progress and retry state appear in Diagnostics; raw upstream bodies and keys do not. Disabling discovery stops new scans without deleting the last successful shared candidates. Re-enabling resumes durable state. During rollout the application writes the seed/discovery source union before cleaning legacy wishlist projection, so a source failure cannot turn a valid catalog into an empty one.

For production, follow [docs/vps-setup.md](docs/vps-setup.md) once to prepare the Ubuntu VPS and GitHub `production` Environment. Use [docs/deployment.md](docs/deployment.md) for routine deployments, status checks, rollback behavior, and guarded database recovery.

## Verify and diagnose

```bash
make verify
```

This starts a disposable PostgreSQL test service, creates a separate E2E database, applies migrations, checks pinned sqlc output, runs Go race tests, frontend tests/typecheck/lint/build, installs the pinned Playwright Chromium when needed, and runs the product-loop, presentation-state, catalog-discovery, and completion-signal browser suites against fake Steam/OpenAI services. The completion suite also runs once with OpenAI disabled. Application fixture authentication and upstream URL overrides fail startup unless `TEST_HARNESS_MODE=1`; the wishlist/catalog controls exist only in the separate E2E fake executable and are never linked into the application.

If the Playwright browser CDN is unavailable, an already-installed Chromium-family executable can be used explicitly for local verification with `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/absolute/path/to/chromium make verify`; the default path still installs Playwright's pinned Chromium.

The diagnostics card and `GET /api/health` show component state and freshness. Authenticated `GET /api/jobs` shows status and sanitized errors—never payloads, prompts, tokens, achievement text, IDs, or secrets. Achievement diagnostics remain aggregate: a schema fallback or classifier outage may leave the saved completion state unchanged, which is intentional last-known-good behavior. Use the diagnostics deletion confirmation to remove the local user’s imported library, wishlist, achievement unlocks/completion projections, profiles/vectors, recommendations, feedback, jobs, deliveries, and sessions; shared catalog and shared achievement definitions/classifications remain.

Explicit negative feedback can create a visible preference correction only when the saved classifier result is actionable and sufficiently confident. The UI first confirms that feedback was saved, then reports whether a rule was applied after the ranking rebuild. Active corrections are listed in the taste profile; “Не учитывать” schedules another deterministic rebuild and keeps the immutable correction history visible in Diagnostics.

## Cleanup

`make down` stops services. To remove the persistent local application database too, run `docker compose down -v` only when you intend to erase it. Browser-test data is disposable and recreated by `make verify`.

See [docs/local-demo.md](docs/local-demo.md) for the demo/runbook and real-account limitations.
