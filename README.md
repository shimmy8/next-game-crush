# Next Game Crush

Next Game Crush is a local, single-user Steam release recommender. It imports public Steam activity, keeps release-date precision intact, builds deterministic recommendations with optional OpenAI enrichment, collects feedback, and sends a local test digest to Mailpit.

## Prerequisites

- Go **1.27.2** (commands run with `GOWORK=off` because a parent workspace may exist)
- Node.js 26 and pnpm 11
- Docker with Docker Compose

Copy `.env.example` to `.env` and fill only the empty values. Required secrets are `STEAM_WEB_API_KEY`, `EMAIL_ACTION_SECRET`, and `SESSION_SECRET`; `OPENAI_API_KEY` is optional. Generate signing/session secrets locally and never commit `.env`. `ALLOWED_STEAM_ID` must be the one 17-digit SteamID allowed to log in.

`APP_BASE_URL` must exactly match the browser-visible origin because it anchors Steam callbacks, signed links, and CSRF origin checks. Use the Go origin (`http://localhost:8080`) for `make dev`; for split development opened through Vite, set it to the Vite URL (`http://localhost:5173`) and let Vite proxy `/api`, `/auth`, and `/actions` to Go.

Steam OpenID proves account ownership but does not unlock private data. Set Steam **Game details** and wishlist visibility to Public if you want them imported. Private or unavailable resources appear as unavailable/stale while the last successful snapshot remains usable.

## Run locally

```bash
cp .env.example .env
# edit .env without committing it
make dev
```

Open <http://localhost:8080>. Mailpit is at <http://localhost:8025>. The production Docker image builds the frontend, copies it into `/app/web/dist`, and embeds SQL migrations in the Go binary, so the same container serves assets and migrates the database at startup.

For split development, start PostgreSQL and Mailpit with `docker compose up -d db mailpit`, set `APP_BASE_URL=http://localhost:5173` when running `GOWORK=off go run ./cmd/next-game-crush`, then run `pnpm --dir web dev` and open <http://localhost:5173>.

The reviewed bounded catalog lives in `catalog_seed.json`. Edit AppIDs, source URLs, and observed dates together; do not replace review with HTML scraping.

## Verify and diagnose

```bash
make verify
```

This starts a disposable PostgreSQL test service, creates a separate E2E database, applies migrations, checks pinned sqlc output, runs Go race tests, frontend tests/typecheck/lint/build, installs the pinned Playwright Chromium when needed, and runs the browser smoke test against fake Steam/OpenAI services. Fixture authentication and upstream URL overrides fail startup unless `TEST_HARNESS_MODE=1`; they are only set by the test command.

If the Playwright browser CDN is unavailable, an already-installed Chromium-family executable can be used explicitly for local verification with `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/absolute/path/to/chromium make verify`; the default path still installs Playwright's pinned Chromium.

The diagnostics card and `GET /api/health` show component state and freshness. Authenticated `GET /api/jobs` shows status and sanitized errors—never payloads, prompts, tokens, or secrets. Use the diagnostics deletion confirmation to remove the local user’s imported library, wishlist, achievements, profiles/vectors, recommendations, feedback, jobs, deliveries, and sessions; shared catalog rows remain.

## Cleanup

`make down` stops services. To remove the persistent local application database too, run `docker compose down -v` only when you intend to erase it. Browser-test data is disposable and recreated by `make verify`.

See [docs/local-demo.md](docs/local-demo.md) for the demo/runbook and real-account limitations.
