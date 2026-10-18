# Game Platform and Steam Deck Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import Steam desktop-platform and Deck compatibility data, preserve its freshness independently, expose it on recommendations, and add visible badges and calendar filters for every state.

**Architecture:** Extend the existing Steam boundary with strict platform parsing and an isolated best-effort Deck request. Persist compatibility in a dedicated table so failed Deck enrichment cannot erase prior success or block catalog refresh, then join that table into recommendation presentation and filter the normalized frontend contract locally.

**Tech Stack:** Go 1.27.2, PostgreSQL/pgvector, goose migrations, sqlc 1.31.1, React 19, TypeScript 6, Vitest/Testing Library, Playwright.

**Spec:** `docs/superpowers/specs/2026-10-18-game-platform-deck-compatibility-design.md`

## Global Constraints

- Run direct Go commands with `GOWORK=off`; use `-race` for focused backend verification.
- PostgreSQL with pgvector remains the only persistent service.
- Add migration `00009_game_compatibility.sql`; do not rewrite existing migrations.
- Do not edit `internal/store/sqlc/` by hand; change queries/schema and run `make sqlc`.
- Preserve the last successful upstream-derived compatibility state on failure.
- Deck enrichment is optional and must not block catalog refresh or deterministic recommendations.
- Do not scrape HTML, introduce a third-party compatibility provider, or change ranking scores.
- Keep Go handlers/types, `web/src/api/types.ts`, normalization, and contract tests aligned.
- Preserve unrelated working-tree changes and never stage them in task commits.

## Review Focus

- A missing or partially typed Store `platforms` object stays unknown instead of becoming three false values; Task 1 pins this with strict-response tests.
- A missing or unrecognized Deck category is malformed and preserves the prior successful state; Tasks 1 and 3 cover parsing and persistence.
- Deck timeouts, rate limits, and unavailable responses do not fail catalog refresh; Task 3 exercises the degraded path.
- Recommendations without a compatibility row expose omitted platforms and Deck `unknown`, not invented support; Task 4 covers both `Latest` and `Get`.
- Platform and Deck filters compose with every existing filter across all six buckets and both views; Task 6 exercises the full intersection.

---

### Task 1: Strict Steam compatibility contracts

**Files:**
- Modify: `internal/steam/types.go`
- Modify: `internal/steam/client.go`
- Modify: `internal/steam/http_client.go`
- Modify: `internal/steam/http_client_test.go`
- Modify: `internal/steam/testdata/store_app.json`
- Create: `internal/steam/testdata/deck_compatibility_verified.json`
- Create: `internal/steam/testdata/deck_compatibility_playable.json`
- Create: `internal/steam/testdata/deck_compatibility_unsupported.json`
- Create: `internal/steam/testdata/deck_compatibility_unknown.json`
- Modify: `internal/sync/service_test.go`

**Interfaces:**
- Produces: `type Platforms struct { Windows, Mac, Linux bool }`.
- Produces: `type DeckCompatibility string` with constants `verified`, `playable`, `unsupported`, and `unknown`.
- Produces: `Client.DeckCompatibility(context.Context, int64) (DeckCompatibility, Availability, error)`.
- Produces: `GameDetails.Platforms Platforms` populated only from a complete, boolean Store object.

- [ ] **Step 1: Write failing Store-platform contract tests**

Extend `TestGameDetailsContractAndMalformedReleaseDate` to assert the exact
Windows/macOS/Linux values from `store_app.json`. Add table cases for a missing
`platforms` object, a missing member, and a non-boolean member; each must return
an error satisfying `ErrInvalidResponse`.

- [ ] **Step 2: Run the focused Store test and verify it fails**

Run: `GOWORK=off go test ./internal/steam -run TestGameDetailsContractAndMalformedReleaseDate`

Expected: FAIL because `GameDetails` does not expose or validate platforms.

- [ ] **Step 3: Implement the platform type and strict parser**

Add `Platforms` to `types.go`, add it to `GameDetails`, and decode the Store
object through pointer booleans so missing members cannot silently become
unsupported platforms.

- [ ] **Step 4: Write failing Deck request and category-mapping tests**

Add a `TestDeckCompatibilityContracts` table that checks the request path
`/saleaction/ajaxgetdeckappcompatibilityreport`, the positive AppID query, all
four numeric mappings, `success != 1`, missing results/category, unknown numeric
categories, malformed JSON, and non-positive AppIDs. Invalid structured bodies
must satisfy `ErrInvalidResponse`; unavailable responses must remain distinct.

- [ ] **Step 5: Run the Deck contract test and verify it fails**

Run: `GOWORK=off go test ./internal/steam -run TestDeckCompatibilityContracts`

Expected: FAIL because the operation and client method do not exist.

- [ ] **Step 6: Implement the isolated Deck adapter**

Add `DeckCompatibilityOperation`, its resource-scoped cache key, the four
constants, and:

```go
func (c *HTTPClient) DeckCompatibility(ctx context.Context, appID int64) (DeckCompatibility, Availability, error)
```

Use `storeBase`, the public JSON path, and query key `nAppID`. Publish cache
metadata only after validation, use existing bounded response/retry behavior,
and never include a request URL or body in returned errors.

- [ ] **Step 7: Update Steam test doubles for the expanded interface**

Give `fakeSteam` in `internal/sync/service_test.go` deterministic per-AppID Deck
statuses, availabilities, and errors, defaulting to a successful `unknown`
response so unrelated tests retain their behavior.

- [ ] **Step 8: Run focused Steam tests with the race detector**

Run: `GOWORK=off go test -race ./internal/steam`

Expected: PASS with zero failures.

- [ ] **Step 9: Commit the Steam boundary**

```bash
git add internal/steam internal/sync/service_test.go
git commit -m "feat: import Steam compatibility metadata"
```

### Task 2: Compatibility schema and generated store bindings

**Files:**
- Create: `db/migrations/00009_game_compatibility.sql`
- Modify: `db/queries/catalog.sql`
- Modify: `internal/store/migration_upgrade_test.go`
- Modify: `internal/store/store_test.go`
- Regenerate: `internal/store/sqlc/`

**Interfaces:**
- Consumes: `Platforms` and `DeckCompatibility` from Task 1.
- Produces: `game_compatibility` with independent platform and Deck freshness.
- Produces sqlc queries: `UpsertGamePlatforms`, `RecordDeckCompatibilitySuccess`, and `RecordDeckCompatibilityFailure`.

- [ ] **Step 1: Write a failing migration contract test**

Add `TestGameCompatibilityMigration` that upgrades through migration 8, creates
a game, applies migration 9, and asserts: the table exists; valid Deck states
are accepted; an invalid state is rejected; deleting the game cascades; and a
down migration removes the table without changing prior schema.

- [ ] **Step 2: Run the migration test and verify it fails**

Run: `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test ./internal/store -run TestGameCompatibilityMigration`

Expected: FAIL because migration 9 does not exist.

- [ ] **Step 3: Add migration 9 and table inventory coverage**

Create the table and constraints exactly as specified. Add
`game_compatibility` to the expected table list in `store_test.go`; the down
migration drops only this table.

- [ ] **Step 4: Add atomic compatibility queries**

`UpsertGamePlatforms` creates or updates the platform booleans and platform
success timestamp without changing existing Deck fields.
`RecordDeckCompatibilitySuccess` changes status, availability, attempt, and
success timestamps. `RecordDeckCompatibilityFailure` changes only availability
and attempt time, leaving status and success time untouched.

- [ ] **Step 5: Generate and verify sqlc output**

Run: `make sqlc`

Expected: generated models and query methods compile; no manual generated-file edits.

- [ ] **Step 6: Run focused store tests with the race detector**

Run: `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test -race ./internal/store`

Expected: PASS with zero failures.

- [ ] **Step 7: Commit the schema slice**

```bash
git add db/migrations/00009_game_compatibility.sql db/queries/catalog.sql internal/store/migration_upgrade_test.go internal/store/store_test.go internal/store/sqlc
git commit -m "feat: persist game compatibility state"
```

### Task 3: Best-effort catalog compatibility persistence

**Files:**
- Modify: `internal/catalog/service.go`
- Modify: `internal/sync/service_test.go`
- Modify: `cmd/e2e-fakes/main.go`

**Interfaces:**
- Consumes: `Client.DeckCompatibility` and Task 2 sqlc queries.
- Produces: catalog refresh that stores platforms only after validated details and records Deck success/failure independently.

- [ ] **Step 1: Write failing catalog persistence tests**

In the existing isolated-database catalog workflow tests, assert that a
successful refresh stores exact platform booleans and each Deck status. Add a
two-refresh case where the first Deck request succeeds and the second is rate
limited or malformed; the second refresh must return success, update the safe
Deck availability/attempt state, and retain the first status and success time.

- [ ] **Step 2: Run the focused catalog workflow tests and verify they fail**

Run: `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test ./internal/sync -run 'Test.*Catalog'`

Expected: FAIL because catalog refresh does not write compatibility state.

- [ ] **Step 3: Persist platform compatibility after valid Store details**

Call `UpsertGamePlatforms` only after normal details persistence succeeds. A
Store details failure must continue through the existing catalog failure path
and leave the compatibility row unchanged.

- [ ] **Step 4: Add best-effort Deck enrichment**

After platform persistence, call `DeckCompatibility`. On valid success call
`RecordDeckCompatibilitySuccess`. On unavailable or failed requests call
`RecordDeckCompatibilityFailure` with the existing safe availability value and
return nil from the Deck substep so catalog refresh proceeds. Return database
write failures because local persistence is not optional.

- [ ] **Step 5: Extend E2E Steam fakes**

Make fake Store details include complete platform data and add a deterministic
Deck JSON route so the end-to-end sync covers the production contract without
network access.

- [ ] **Step 6: Run focused catalog and sync tests with the race detector**

Run: `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test -race ./internal/catalog ./internal/sync`

Expected: PASS with zero failures.

- [ ] **Step 7: Refresh CodeGraph and inspect affected tests**

Run: `codegraph sync . && codegraph affected internal/steam/types.go internal/steam/client.go internal/steam/http_client.go internal/catalog/service.go db/migrations/00009_game_compatibility.sql db/queries/catalog.sql -p .`

Expected: graph is current; inspect the reported packages before proceeding.

- [ ] **Step 8: Commit catalog persistence**

```bash
git add internal/catalog/service.go internal/sync/service_test.go cmd/e2e-fakes/main.go
git commit -m "feat: refresh game compatibility safely"
```

### Task 4: Recommendation API compatibility metadata

**Files:**
- Modify: `internal/recommendations/types.go`
- Modify: `internal/recommendations/retrieval.go`
- Modify: `internal/recommendations/repository_integration_test.go`
- Modify: `internal/httpapi/ranking_handlers_test.go`

**Interfaces:**
- Consumes: `game_compatibility` from Task 2.
- Produces: `Recommendation.Platforms *Platforms` with JSON `platforms,omitempty`.
- Produces: `Recommendation.DeckCompatibility DeckCompatibility` with JSON `deckCompatibility` and an `unknown` default.

- [ ] **Step 1: Write failing repository/API contract assertions**

Extend `TestLatestAndGetExposeSteamPresentation` with one compatibility row and
assert identical platform values and Deck status from `Latest`, `Get`, and the
safe service wrappers. Add a second recommendation without a row and assert nil
platforms plus Deck `unknown`. Extend the HTTP handler contract fixture so JSON
field names and values are decoded and asserted.

- [ ] **Step 2: Run focused recommendation and handler tests and verify failure**

Run: `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test ./internal/recommendations ./internal/httpapi -run 'TestLatestAndGetExposeSteamPresentation|TestRecommendationReadEndpointsRequireSession'`

Expected: FAIL because the recommendation contract lacks compatibility.

- [ ] **Step 3: Add presentation types and repository joins**

Define the API platform object in `types.go`. Update both recommendation SELECT
statements with the same left join and scan order. When no row exists, leave
platforms nil and set Deck to `unknown`; otherwise expose stored booleans and
the stored Deck status. Do not add compatibility to scoring, build keys, or
explanation evidence.

- [ ] **Step 4: Run focused tests with the race detector**

Run: `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test -race ./internal/recommendations ./internal/httpapi`

Expected: PASS with zero failures.

- [ ] **Step 5: Commit the backend contract**

```bash
git add internal/recommendations internal/httpapi/ranking_handlers_test.go
git commit -m "feat: expose recommendation compatibility"
```

### Task 5: Frontend normalization and compatibility badges

**Files:**
- Modify: `web/src/api/types.ts`
- Modify: `web/src/api/client.ts`
- Modify: `web/src/api/hooks.test.ts`
- Modify: `web/src/components/RecommendationCard.tsx`
- Modify: `web/src/components/RecommendationCard.test.tsx`
- Modify: `web/src/styles.css`

**Interfaces:**
- Consumes: backend `platforms` and `deckCompatibility` fields from Task 4.
- Produces: `PlatformSupport`, `DeckCompatibility`, normalized recommendations, and accessible card badges.

- [ ] **Step 1: Write failing normalization tests**

Add cases proving valid platform booleans survive normalization, absent or
partially invalid platforms become `undefined`, all four Deck states survive,
and missing or unrecognized Deck values become `unknown`.

- [ ] **Step 2: Run the focused normalization test and verify failure**

Run: `pnpm --dir web run test -- src/api/hooks.test.ts`

Expected: FAIL because compatibility normalization is absent.

- [ ] **Step 3: Implement the TypeScript contract and defensive normalization**

Add exact union types and centralize runtime guards in `client.ts`. Keep
`platforms` absent unless all three members are booleans; normalize Deck to one
of the four allowed strings.

- [ ] **Step 4: Write failing card presentation tests**

Cover all supported desktop badges, the absent-platform label, each localized
Deck status, and presence of the compatibility row in compact cards. Assert
text labels so the meaning does not depend on color.

- [ ] **Step 5: Run the card test and verify failure**

Run: `pnpm --dir web run test -- src/components/RecommendationCard.test.tsx`

Expected: FAIL because compatibility badges are not rendered.

- [ ] **Step 6: Implement accessible compatibility badges**

Render the compatibility row after the title and before the score. Use Russian
visible labels and status-specific CSS classes; keep the row compact and
wrapping on narrow cards.

- [ ] **Step 7: Run focused frontend tests and typecheck**

Run: `pnpm --dir web run test -- src/api/hooks.test.ts src/components/RecommendationCard.test.tsx`

Run: `pnpm --dir web run typecheck`

Expected: both commands PASS with zero failures.

- [ ] **Step 8: Commit frontend contracts and badges**

```bash
git add web/src/api web/src/components/RecommendationCard.tsx web/src/components/RecommendationCard.test.tsx web/src/styles.css
git commit -m "feat: show game compatibility badges"
```

### Task 6: Platform and Steam Deck calendar filters

**Files:**
- Modify: `web/src/pages/CalendarPage.tsx`
- Modify: `web/src/pages/CalendarPage.test.tsx`
- Modify: `web/tests/e2e/smoke.spec.ts`

**Interfaces:**
- Consumes: normalized recommendation compatibility from Task 5.
- Produces: platform filter `all | windows | mac | linux | unknown`.
- Produces: Deck filter `all | verified | playable | unsupported | unknown`.

- [ ] **Step 1: Enrich calendar fixtures and write failing filter tests**

Give fixture items complementary platform and Deck states. In both month and
list views, assert the exact option labels and values, concrete platform
matching, absent-platform `unknown`, all four exact Deck states, and an AND
intersection with source, score, and date precision across all six buckets.

- [ ] **Step 2: Run the calendar test and verify failure**

Run: `pnpm --dir web run test -- src/pages/CalendarPage.test.tsx`

Expected: FAIL because the controls and predicates do not exist.

- [ ] **Step 3: Implement the two filter controls and predicates**

Extend `filterCalendar` with typed platform and Deck selections. A concrete
platform requires `platforms?.[selection] === true`; platform `unknown`
requires an absent platform object; Deck compares exact status. Include both
states in `useMemo` dependencies and render labeled selects in the existing
filter bar.

- [ ] **Step 4: Extend the browser smoke test**

After sync, assert a compatibility badge is visible, select a platform and a
Deck state, and verify matching and non-matching fixture recommendations in the
calendar without weakening existing release-date and feedback checks.

- [ ] **Step 5: Run all frontend checks**

Run: `pnpm --dir web run test`

Run: `pnpm --dir web run typecheck`

Run: `pnpm --dir web run lint`

Run: `pnpm --dir web run build`

Expected: all commands PASS with zero failures and zero lint warnings.

- [ ] **Step 6: Commit calendar filtering**

```bash
git add web/src/pages/CalendarPage.tsx web/src/pages/CalendarPage.test.tsx web/tests/e2e/smoke.spec.ts
git commit -m "feat: filter releases by compatibility"
```

### Task 7: Cross-cutting verification and documentation check

**Files:**
- Verify: all files changed in Tasks 1-6
- Modify only if needed: `README.md`
- Modify only if needed: `docs/local-demo.md`

**Interfaces:**
- Consumes: the complete feature.
- Produces: release-ready evidence and accurate operator guidance.

- [ ] **Step 1: Review user-facing operational docs**

Check whether catalog refresh and the new unknown/degraded states make any
existing README or local-demo statement inaccurate. Update only affected text;
do not add speculative API guarantees.

- [ ] **Step 2: Run CodeGraph impact selection and repository hygiene checks**

Run: `codegraph sync .`

Run: `codegraph affected $(git diff --name-only HEAD~6..HEAD) -p .`

Run: `git diff --check`

Expected: the graph is current, affected packages are covered by prior tests,
and the diff check prints no errors. If the working tree contains unrelated
user changes, derive the changed-file list from the feature commits explicitly
instead of staging or modifying those changes.

- [ ] **Step 3: Run the full release-ready verification**

Run: `make verify`

Expected: sqlc output is stable; all Go race tests, frontend tests, typecheck,
lint, build, Docker checks, and Playwright smoke tests pass. If the pinned
Chromium download is unavailable, use the documented explicit system-Chrome
fallback and record that fact.

- [ ] **Step 4: Inspect the final feature diff**

Run: `git diff --stat e9ef207..HEAD && git diff --check e9ef207..HEAD`

Expected: only compatibility feature files and any strictly necessary docs are
included; no `.env`, `.codegraph` data, build output, or unrelated files appear.

- [ ] **Step 5: Commit any verification-only documentation correction**

If Step 1 changed documentation:

```bash
git add README.md docs/local-demo.md
git commit -m "docs: explain compatibility freshness"
```

Otherwise, do not create an empty commit.
