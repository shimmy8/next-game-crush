# Steam Privacy Preflight and Profile Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Block synchronization until Steam profile and Game Details access are verified, while safely projecting the user's Steam name and avatar into the UI.

**Architecture:** Extend the typed Steam client with Player Summaries, persist privacy knowledge separately from import availability, and expose a CSRF-protected preflight endpoint plus nullable identity fields on `/api/me`. The sync start service repeats preflight server-side so the gate cannot be bypassed; the React hook performs the user-facing preflight first and renders actionable blocked/error states.

**Tech Stack:** Go 1.27.2, chi, pgx/sqlc/goose, PostgreSQL, React 19, TypeScript, Vitest, Playwright

**Spec:** `docs/superpowers/specs/2026-10-17-steam-privacy-preflight-profile-design.md`

## Global Constraints

- Keep the application single-user and use only the authenticated server-side Steam account; never accept a SteamID in preflight input.
- Preserve last-known-good `display_name` and `avatar_url` on every inconclusive or failed refresh.
- Keep visibility (`public|private|unknown`) separate from availability (`available|unavailable|request_failed`).
- Treat an empty Owned Games response as `unknown`/`game_details_unavailable`, not confirmed private.
- Allow avatar URLs only over HTTPS from exact reviewed Steam CDN hosts.
- Do not log identifiers, identity values, URLs, upstream payloads, credentials, or raw errors.
- Add migration `00008`; never edit an applied migration or hand-edit `internal/store/sqlc/`.
- Preserve all current uncommitted authentication, localization, and user-owned changes.

## Review Focus

- A public profile with zero visible games must be blocked as ambiguous without being labelled private; pin this in Task 3.
- A valid Player Summaries envelope containing no matching player must fail closed and preserve identity; pin this in Tasks 1 and 3.
- A malicious avatar using userinfo, HTTP, a lookalike suffix, or an unexpected port must be rejected; pin this in Task 1.
- A direct call to `/api/sync` without the UI preflight must still be gated; pin this in Task 3.
- An avatar image load failure must show a local fallback without hiding the display name; pin this in Task 6.

---

### Task 1: Typed Player Summaries Boundary

**Files:**
- Modify: `internal/steam/types.go`
- Modify: `internal/steam/client.go`
- Modify: `internal/steam/http_client.go`
- Modify: `internal/steam/http_client_test.go`
- Create: `internal/steam/testdata/player_summary_public.json`
- Create: `internal/steam/testdata/player_summary_private.json`

**Interfaces:**
- Produces: `type Visibility string` with `VisibilityPublic`, `VisibilityPrivate`, and `VisibilityUnknown`.
- Produces: `type PlayerSummary struct { Visibility Visibility; DisplayName string; AvatarURL string }`.
- Produces: `Client.PlayerSummary(context.Context, string) (PlayerSummary, error)` and `(*HTTPClient).PlayerSummary`.
- Produces: `ValidateAvatarURL(string) (string, error)` using the exact host allowlist `avatars.cloudflare.steamstatic.com`, `avatars.fastly.steamstatic.com`, and `avatars.akamai.steamstatic.com`.

- [ ] **Step 1: Write failing Steam client contract tests**

Add tests for public/private visibility mapping, exact matching player selection, missing/mismatched player rejection, malformed JSON, and avatar rejection for HTTP, userinfo, unexpected port, and hostname suffix attacks.

- [ ] **Step 2: Verify the focused tests fail**

Run: `GOWORK=off go test -race ./internal/steam -run 'TestPlayerSummary|TestValidateAvatarURL'`

Expected: FAIL because the types and method do not exist.

- [ ] **Step 3: Implement the minimal typed boundary**

Call `/ISteamUser/GetPlayerSummaries/v2/` with the configured API key and server-side SteamID. Require exactly one matching player, map community visibility `3` to public and `1` to private, reject unknown/missing values, trim the display name, and validate a non-empty avatar before returning it.

- [ ] **Step 4: Verify the Steam package**

Run: `GOWORK=off go test -race ./internal/steam`

Expected: PASS.

- [ ] **Step 5: Commit the boundary**

```bash
git add internal/steam/client.go internal/steam/types.go internal/steam/http_client.go internal/steam/http_client_test.go internal/steam/testdata/player_summary_public.json internal/steam/testdata/player_summary_private.json
git commit -m "feat: add Steam player summary client"
```

### Task 2: Visibility and Identity Persistence

**Files:**
- Create: `db/migrations/00008_steam_visibility.sql`
- Modify: `db/queries/library.sql`
- Modify: `internal/store/migration_upgrade_test.go`
- Regenerate: `internal/store/sqlc/models.go`
- Regenerate: `internal/store/sqlc/library.sql.go`

**Interfaces:**
- Produces: nullable `user_source_states.visibility` constrained to `public|private|unknown`.
- Produces: sqlc query `UpdateSteamAccountIdentity(ctx, UpdateSteamAccountIdentityParams) error` that preserves stored values when nullable replacements are absent.
- Produces: sqlc query `UpsertUserSourceVisibility(ctx, UpsertUserSourceVisibilityParams) error` that updates visibility without changing source availability timestamps.

- [ ] **Step 1: Write a failing migration upgrade test**

Add `TestSteamVisibilityMigration` proving an existing database upgrades, null remains valid, all three allowed values persist, and an invalid value violates the check constraint.

- [ ] **Step 2: Verify the migration test fails**

Run: `make wait-test-db` then `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/store -run TestSteamVisibilityMigration`

Expected: FAIL because migration `00008` is absent.

- [ ] **Step 3: Add the migration and named queries**

The down migration removes only `visibility`. `UpdateSteamAccountIdentity` uses `COALESCE` so a failed/incomplete refresh cannot erase a good value. Keep the existing `UpsertUserSourceState` availability behavior unchanged.

- [ ] **Step 4: Regenerate and verify sqlc output**

Run: `make sqlc`

Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/store -run 'TestMigrateCreatesRequiredTables|TestSteamVisibilityMigration'`

Expected: generation succeeds and tests PASS.

- [ ] **Step 5: Commit persistence**

```bash
git add db/migrations/00008_steam_visibility.sql db/queries/library.sql internal/store/migration_upgrade_test.go internal/store/sqlc/models.go internal/store/sqlc/library.sql.go
git commit -m "feat: persist Steam profile visibility"
```

### Task 3: Preflight Domain Service and Server-Side Gate

**Files:**
- Modify: `internal/sync/service.go`
- Modify: `internal/sync/service_test.go`

**Interfaces:**
- Consumes: `steam.Client.PlayerSummary`, `UpdateSteamAccountIdentity`, and `UpsertUserSourceVisibility` from Tasks 1-2.
- Produces: `type PreflightResult struct { CanSync bool; ProfileVisibility steam.Visibility; GameDetailsVisibility steam.Visibility; Reason *string; DisplayName *string; AvatarURL *string }` with the JSON names from the spec.
- Produces: `func (s *Service) Preflight(context.Context, uuid.UUID) (PreflightResult, error)`.
- Produces: sentinel errors `ErrPreflightBlocked` and `ErrPreflightFailed`; the latter represents an inconclusive upstream check without exposing its cause.
- Changes: `func (s *Service) Start(context.Context, uuid.UUID) (uuid.UUID, error)` calls `Preflight` before opening the enqueue transaction and returns `ErrPreflightBlocked` when `CanSync` is false.

- [ ] **Step 1: Extend `fakeSteam` and write failing preflight tests**

Cover public plus non-empty owned games, private profile, unavailable owned games, available-but-empty games, Player Summaries failure, Owned Games failure, invalid user ID, and preservation of stored identity after a later failure.

- [ ] **Step 2: Write the direct-start gate test**

Assert `Start` creates no `steam.sync` row when preflight is private, ambiguous, or failed, and creates/reuses the workflow only after a successful server-side preflight. Assert optional source behavior is not consulted.

- [ ] **Step 3: Verify focused tests fail**

Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/sync -run 'TestPreflight|TestStartRequiresPreflight'`

Expected: FAIL because `Preflight` and the gate do not exist.

- [ ] **Step 4: Implement `Preflight` and the `Start` gate**

Load the account by authenticated user ID, refresh Player Summaries, persist only validated identity, persist visibility separately, and probe Owned Games. Return stable reasons `profile_private`, `game_details_unavailable`, or `check_failed`; never return an upstream message.

- [ ] **Step 5: Verify sync tests**

Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/sync`

Expected: PASS.

- [ ] **Step 6: Commit domain behavior**

```bash
git add internal/sync/service.go internal/sync/service_test.go
git commit -m "feat: gate Steam sync with privacy preflight"
```

### Task 4: HTTP Preflight and Current-User Identity Contracts

**Files:**
- Modify: `internal/httpapi/sync_handlers.go`
- Modify: `internal/httpapi/sync_handlers_test.go`
- Modify: `internal/httpapi/auth_handlers.go`
- Modify: `internal/httpapi/auth_handlers_test.go`
- Modify: `internal/httpapi/router.go`
- Modify: `internal/app/app.go`

**Interfaces:**
- Consumes: `Preflight(context.Context, uuid.UUID) (syncservice.PreflightResult, error)`.
- Produces: `type SteamIdentity struct { DisplayName *string; AvatarURL *string }` and narrow `IdentityReadService.SteamIdentity(context.Context, uuid.UUID) (syncservice.SteamIdentity, error)` implemented by `sync.Service`.
- Produces: authenticated and CSRF-protected `POST /api/sync/preflight`.
- Changes: `GET /api/me` adds nullable `displayName` and `avatarUrl` while preserving existing fields.
- Maps: `ErrPreflightFailed` to HTTP 502 with `reason: "check_failed"`, `ErrPreflightBlocked` from a direct start to HTTP 409, and unexpected persistence failures to HTTP 500.

- [ ] **Step 1: Write failing handler contract tests**

Test auth and CSRF rejection, exact public/private/unknown JSON, safe `502` `check_failed` for upstream failures, `500` for internal persistence failure, and absence of raw/canary data in body and captured logs.

- [ ] **Step 2: Write failing `/api/me` identity tests**

Cover populated identity, nullable identity, identity reader failure, and unchanged authentication/session fields.

- [ ] **Step 3: Verify handler tests fail**

Run: `GOWORK=off go test -race ./internal/httpapi -run 'TestSyncPreflight|TestMe'`

Expected: FAIL because routes and dependencies are missing.

- [ ] **Step 4: Implement handlers and dependency wiring**

Pass the existing application logger and sync service through `Dependencies`. Return only bounded JSON/error messages and preserve the current auth logging/request-ID changes in these already-modified files.

- [ ] **Step 5: Verify HTTP and app packages**

Run: `GOWORK=off go test -race ./internal/httpapi ./internal/app`

Expected: PASS.

- [ ] **Step 6: Commit the HTTP contract**

```bash
git add internal/httpapi/sync_handlers.go internal/httpapi/sync_handlers_test.go internal/httpapi/auth_handlers.go internal/httpapi/auth_handlers_test.go internal/httpapi/router.go internal/app/app.go
git commit -m "feat: expose Steam privacy preflight"
```

### Task 5: Frontend Preflight State Machine

**Files:**
- Modify: `web/src/api/types.ts`
- Modify: `web/src/api/client.ts`
- Modify: `web/src/api/client.test.ts`
- Modify: `web/src/api/hooks.ts`
- Modify: `web/src/api/hooks.test.ts`

**Interfaces:**
- Produces: `type PreflightResponse` matching the backend JSON exactly.
- Produces: `preflightSync(csrfToken: string, signal?: AbortSignal): Promise<PreflightResponse>`.
- Changes: `CurrentUser` gains `displayName?: string | null` and `avatarUrl?: string | null`.
- Changes: `useSync` returns `preflight: PreflightResponse | null`, `preflightError: string`, and `checking: boolean` in addition to existing sync state and `run`; calling `run` again is the retry action.

- [ ] **Step 1: Write failing client and hook tests**

Assert preflight is POSTed with CSRF before start, blocked results never call `startSync`, request failure produces a retryable check state, successful preflight starts polling, aborts do not surface errors, and a retry can proceed.

- [ ] **Step 2: Verify focused tests fail**

Run: `pnpm --dir web run test -- src/api/client.test.ts src/api/hooks.test.ts`

Expected: FAIL because the API and state do not exist.

- [ ] **Step 3: Implement the API types and state machine**

Keep transport errors separate from `canSync: false`. Retain the last sync snapshot and use the preflight result's identity as the newest local projection.

- [ ] **Step 4: Verify focused frontend tests**

Run: `pnpm --dir web run test -- src/api/client.test.ts src/api/hooks.test.ts`

Expected: PASS.

- [ ] **Step 5: Commit frontend data flow**

```bash
git add web/src/api/types.ts web/src/api/client.ts web/src/api/client.test.ts web/src/api/hooks.ts web/src/api/hooks.test.ts
git commit -m "feat: add Steam preflight client flow"
```

### Task 6: Privacy Instructions and Profile Identity UI

**Files:**
- Create: `web/src/components/SteamPreflightStatus.tsx`
- Create: `web/src/components/SteamPreflightStatus.test.tsx`
- Create: `web/src/components/SteamIdentity.tsx`
- Create: `web/src/components/SteamIdentity.test.tsx`
- Modify: `web/src/pages/DashboardPage.tsx`
- Modify: `web/src/components/DashboardStates.test.tsx`
- Modify: `web/src/styles.css`

**Interfaces:**
- Consumes: the `useSync` result from Task 5 and `CurrentUser` identity fields.
- Produces: `SteamPreflightStatus` with checking, private/ambiguous, failed, and retry states.
- Produces: `SteamIdentity` rendering a validated remote avatar or local neutral fallback plus display name.

- [ ] **Step 1: Write failing component tests**

Assert exact Russian instructions for opening Profile and Game Details, the note about individually hidden games and optional sources, `Проверить снова`, separate check-failure copy, and no sync progress before a job exists.

- [ ] **Step 2: Write identity and dashboard tests**

Assert display name/avatar rendering, fallback on absent URL and image error, display name retention with fallback, and both dashboard sync entry points using the same preflight flow.

- [ ] **Step 3: Verify component tests fail**

Run: `pnpm --dir web run test -- src/components/SteamPreflightStatus.test.tsx src/components/SteamIdentity.test.tsx src/components/DashboardStates.test.tsx`

Expected: FAIL because components and wiring are absent.

- [ ] **Step 4: Implement components, wiring, and responsive styles**

Preserve the current Russian localization and do not expose even a shortened SteamID when display name or fallback text is available. Use a bundled/CSS fallback; do not fetch a fallback asset.

- [ ] **Step 5: Verify frontend quality gates**

Run: `pnpm --dir web run test`

Run: `pnpm --dir web run typecheck`

Run: `pnpm --dir web run lint`

Run: `pnpm --dir web run build`

Expected: all commands PASS.

- [ ] **Step 6: Commit the UI**

```bash
git add web/src/components/SteamPreflightStatus.tsx web/src/components/SteamPreflightStatus.test.tsx web/src/components/SteamIdentity.tsx web/src/components/SteamIdentity.test.tsx web/src/pages/DashboardPage.tsx web/src/components/DashboardStates.test.tsx web/src/styles.css
git commit -m "feat: show Steam privacy guidance and identity"
```

### Task 7: End-to-End Contract and Full Verification

**Files:**
- Modify: `cmd/e2e-fakes/main.go`
- Modify: `web/tests/e2e/smoke.spec.ts`
- Modify: `docs/local-demo.md`

**Interfaces:**
- Consumes: complete backend and frontend preflight flow.
- Produces: local verification instructions that do not ask the user to expose or paste secrets.

- [ ] **Step 1: Add safe Player Summaries behavior to the E2E fake**

Return a public matching profile and non-empty owned library for the allowed fixture flow; add a controllable private/ambiguous case without embedding a real user identifier.

- [ ] **Step 2: Extend the E2E scenario and local instructions**

Assert preflight precedes sync, identity renders, private access blocks job creation, and retry works after the fake becomes public. Document the Steam privacy navigation and what data remains optional.

- [ ] **Step 3: Refresh graph evidence**

Run: `codegraph sync .`

Run: `codegraph affected internal/steam/client.go internal/steam/http_client.go internal/sync/service.go internal/httpapi/sync_handlers.go internal/httpapi/auth_handlers.go web/src/api/hooks.ts web/src/pages/DashboardPage.tsx -p .`

Expected: output includes the focused packages and frontend consumers already tested.

- [ ] **Step 4: Run full verification**

Run: `make verify`

Expected: sqlc diff is empty and all Go race tests, frontend tests/typecheck/lint/build, Compose checks, image build, and Playwright tests PASS.

- [ ] **Step 5: Commit E2E and documentation**

```bash
git add cmd/e2e-fakes/main.go web/tests/e2e/smoke.spec.ts docs/local-demo.md
git commit -m "test: verify Steam privacy preflight flow"
```
