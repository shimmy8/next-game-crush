# Local demo and operations runbook

## Prepare

Install Go 1.27.2, Node 26, pnpm 11, Docker, and Docker Compose. Copy `.env.example` to `.env`; supply the allowlisted 17-digit SteamID, a server-side Steam Web API key, and randomly generated email-action/session secrets. An OpenAI API key is optional: without it, deterministic ranking and template explanations remain available. Never paste credentials into issues, logs, screenshots, this file, or `catalog_seed.json`.

Steam OpenID does not override privacy. Before the first synchronization, open Steam, go to **Profile → Edit Profile → Privacy Settings**, set **My profile** and **Game details** to **Public**, and make sure individual games needed for the demo are not marked private. Save the settings, return to Next Game Crush, and choose **Проверить снова**. The profile and Game Details checks are required; wishlist and achievement access are optional. When an optional source is private or unavailable, the application shows it as unavailable/stale and retains the last successful snapshot instead of blocking the whole synchronization.

Set `APP_BASE_URL` to the origin actually open in the browser: the Go origin (`http://localhost:8080`) for Compose, or Vite (`http://localhost:5173`) for split development where `/api`, `/auth`, and `/actions` are proxied to Go. CSRF validation is exact-origin. If Steam cannot return to localhost, use a temporary HTTPS tunnel as `APP_BASE_URL` and keep it consistent for the whole login.

## Start and demo

1. Run `make dev` and wait for the Compose health checks.
2. Open `http://localhost:8080`, sign in through Steam, and confirm the account matches the allowlist.
3. Start synchronization. Watch stages for library, recent activity, wishlist, achievements, catalog, AI enrichment, profile, and recommendations.
4. Confirm an exact release appears on a calendar day while month, quarter, year, Coming Soon, and unknown releases stay in their named buckets.
5. Open recommendation evidence and submit Like/Dislike with a reason and optional text. Wait for the visible ranking diff.
6. Send a test digest and inspect the one message at `http://localhost:8025`. Follow one signed feedback link, then replay it and confirm the idempotent “already saved” response.
7. Run synchronization again and confirm the workflow ID/records remain idempotent.
8. Open Diagnostics. Check Steam/OpenAI/SMTP/job state and freshness, then use deletion only if you intend to sign out and remove all imported user data.

Diagnostics expose fixed, sanitized error categories. They do not return job payloads, raw prompts, API keys, cookies, signing material, SteamID values, or upstream response bodies.

## Verification and troubleshooting

Run `make verify` before a demo. It uses fake upstreams and a dedicated `next_game_crush_e2e` database; it does not recreate the application database. `TEST_HARNESS_MODE` and `FIXTURE_AUTH_ENABLED` are test-only and must remain `0`/unset for real use. A configured fixture route is absent outside the explicit harness mode, and test upstream URL overrides are rejected at startup.

If synchronization is stale, inspect the source states and sanitized job error in Diagnostics. Check profile visibility and Steam rate limiting first. If OpenAI is disabled or unavailable, existing features/vectors remain and ranking falls back to deterministic evidence. For email ambiguity, inspect Mailpit before explicitly retrying; do not assume SMTP exactly-once delivery.

For server-side synchronization diagnostics, follow the structured JSON output:

```bash
docker compose logs -f app
```

Start with a `sync_request` event and copy its `workflow_id`. Follow matching `job_attempt` events through `started`, optional `heartbeat`, and the terminal stage. Use `job_id` and `parent_job_id` to inspect nested work. Matching `sync_source` events show the start and finish of owned games, recent play, wishlist, and the aggregate achievement import. A `retry_scheduled` stage means the queue will retry; `dead` is terminal. Safe `reason` values distinguish timeouts, rate limiting, upstream availability, malformed responses, lease loss, persistence failure, and an otherwise bounded internal error.

These logs intentionally omit SteamID, user and AppID values, game/profile names, job payloads, worker and fencing identifiers, credentials, prompts, upstream bodies, and raw errors. Do not paste secrets or raw Steam/OpenAI responses into diagnostics when sharing logs.

`catalog_seed.json` is a manually reviewed 90-day input. Update its AppIDs and adjacent source/observed metadata together, then run `make verify`.

## Cleanup

- Stop services: `make down`.
- Remove application data and volumes intentionally: `docker compose down -v`.
- Remove only imported user data while retaining the shared catalog: use the authenticated Diagnostics deletion action.

## Real-Steam limitations observed

A real-account smoke run was not performed because no Steam/OpenAI credentials were supplied to this workspace. Consequently there are no direct observations to claim for Steam login, private-profile behavior, live rate limiting, current Store date text, or live OpenAI responses. Automated verification uses deterministic local fakes and proves adapter contracts plus the fixture product loop; it does not prove third-party availability on a future demo day. Run the manual steps above with the allowlisted account before a real demo and record the observed privacy/API results without recording credentials.
