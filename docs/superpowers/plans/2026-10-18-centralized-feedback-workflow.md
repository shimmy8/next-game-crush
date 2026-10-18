# Centralized Feedback Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a page-level workflow keyed by stable `gameId` the single owner of feedback submission, accepted rebuild polling, retry, optimistic dislike tombstones, and authoritative freshness reconciliation, while making backend exact-game eligibility follow the latest direct reaction.

**Architecture:** Replace the current component-local `useFeedbackWorkflow`, dashboard lock callbacks, and calendar lock state with a pure reducer plus one `FeedbackWorkflowProvider` mounted at the authenticated dashboard boundary. Consumers keep only draft, selection, dialog, and presentation state. The provider serializes one active command per game, owns all async work through explicit fresh/stale/failed reload results, and filters every overview collection by stable game tombstones. PostgreSQL reads remain defense in depth: one deterministic latest-direct-action relation drives both stale published reads and exact candidate eligibility, while historical dislike counts remain soft learning evidence.

**Tech Stack:** Go 1.27.2 (`GOWORK=off`), PostgreSQL/pgvector, pgx, React 19, TypeScript, Vitest/Testing Library, Playwright; existing dependencies only.

**Spec:** `docs/superpowers/specs/2026-10-18-centralized-feedback-workflow-design.md`

## Global Constraints

- Execute this plan in repository-required subagent mode. Give each implementation task to a fresh subagent, then inspect its diff and run fresh verification in the coordinating agent before accepting the task.
- Refactor the current partial solution; do not layer a provider over component-owned polling, `DashboardPage`'s `activeGames`/`busyGameIds`, calendar `locked`/`phase` workflow state, duplicate release callbacks, or any `BackgroundFeedbackWorkflow` variant present when execution starts.
- Coordinate by stable `gameId`. Recommendation IDs and ranking versions identify the submitted snapshot only and must never be used as the cross-surface lock or tombstone key.
- One game may have at most one active workflow. Acquisition must be synchronous against a ref-backed registry before the POST. Do not queue a competing same-game action; other game IDs remain independently usable.
- The provider is the exactly-one release owner. Consumer unmount, dialog close, accepted dislike tombstoning, dead rebuild, polling network error, stale reload, and failed reload must not release the same-game lock.
- Freeze one canonical payload and one client-generated idempotency key per acquired command. Poll/reconciliation retries reuse accepted event/job identity and never POST feedback again. A later action after release gets a new key.
- Preserve the existing multi-reason contract: zero or more canonical reasons in taxonomy order, no duplicates or unknown/empty values, optional free text within the existing 500-code-point limit, and exactly one feedback event/action. Do not reintroduce legacy `reason` in new frontend payloads.
- `remind_me` sends no reasons or free text, creates no tombstone, and does not overwrite or clear persisted latest `like`/`dislike` display.
- Accepted `dislike` immediately tombstones the exact `gameId` across featured discovery, discovery items, wishlist items, all six calendar buckets, selected calendar dialog, and resume affordances. Counts remain source-membership counts adjusted only for games hidden from the current overview; do not relabel wishlist/discovery membership.
- Only an explicit `fresh` reload result can reconcile and release an accepted workflow. A later `like` removes a tombstone only when fresh authoritative overview data confirms that game's `latestFeedbackAction === 'like'`; stale/failed data retains the tombstone and retry surface.
- Backend latest direct action is `like`, `dislike`, or legacy `exclude`, ordered by `created_at DESC, id DESC`; `remind_me` is ignored. Current `dislike`/`exclude` suppresses stale published reads and exact candidate eligibility. A later `like` restores eligibility. Historical negative counts/vectors/adjustments remain soft evidence for this or similar games.
- `Latest` filtering must occur before `BuildOverview`, so discoveries, wishlist, and every calendar partition inherit the same server-side protection.
- Add no migration, table, column, dependency, local-storage lock, new datastore, or global feedback mutex. Preserve migration `00013` and the existing Go/TypeScript/API multi-reason contracts.
- Public client errors are fixed safe copy. Never expose raw response bodies, IDs, job payloads, feedback text, SteamID, prompts, session values, or credentials. Never read `.env`; use only `.env.example` for variable names.
- Preserve unrelated dirty-worktree changes. Do not edit `docs/steam-release-recommender-idea.md`, existing design specs, or generated sqlc files for this work. Do not run broad formatters over unrelated files.
- Before each shared-symbol change run the relevant `codegraph impact <symbol> -p .`; after each task run `codegraph sync .` and `codegraph affected <changed-files...> -p .`. Never commit CodeGraph state.
- Every RED command must fail for the asserted missing behavior, not because infrastructure is absent. Start the isolated database with `make wait-test-db`; use only `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable'` for database tests.
- Do not create implementation commits in this shared dirty worktree. The user did not request commits, and the relevant files contain unrelated work. End each task with focused test output, `git diff --check`, `git status --short`, and a path-scoped diff review. Commit only if the user later authorizes it and exact owned hunks can be isolated without staging unrelated changes.

## Review Focus

1. **Same-turn same-game race:** two surfaces dispatch before React rerenders; exactly one POST occurs, the second intent is rejected locally, and a different game remains usable. Task 3 pins this with reducer/registry tests; Task 6 repeats it through dashboard and calendar integration.
2. **Initiator disappears after acceptance:** accepted dislike removes the card/dialog but provider-owned polling continues, terminal success reloads, and exactly one release occurs. Task 4 proves provider lifetime; Task 5 proves collection removal does not abort it.
3. **Retry ownership after acceptance:** dead rebuild, polling network error, and stale/failed reconciliation keep the same job, idempotency key, tombstone, and per-game lock while exposing a reachable page-level retry. Task 4 tests each transition; Task 6 verifies the initiating calendar UI may be gone.
4. **Authoritative later-like ordering:** no competing like can race an active dislike, and a later like clears the tombstone only after terminal workflow plus fresh overview confirmation; inverse response ordering cannot mutate visibility. Tasks 3–4 test serialization/freshness, and Task 6 covers cross-surface behavior.
5. **Feedback contract preservation:** multiple reasons stay one ordered event, persisted reasons/action restore after reload, reminder sends no detail fields and preserves direct reaction, and current latest action—not lifetime dislike count—controls exact eligibility. Tasks 1 and 6 cover backend/frontend contract regressions.

## Delivery Boundaries and File Ownership

The worktree already contains a distributed partial implementation. At execution start, re-read the listed files because other approved work may have advanced them. Preserve useful presentation and multi-reason behavior, but migrate ownership instead of keeping compatibility callbacks indefinitely.

| Unit | Live files | Planned responsibility |
|---|---|---|
| Backend exact-game policy | `internal/recommendations/retrieval.go`, `types.go`, `scoring.go` | One latest-direct-action relation for candidate eligibility; historical counts remain scoring/profile evidence; `Latest` suppresses stale disliked rows before overview construction. |
| Backend regressions | `internal/recommendations/repository_integration_test.go`, `build_key_integration_test.go`, `scoring_test.go`, `overview_repository_integration_test.go` | Current dislike/exclude suppression, reminder neutrality, equal-time ID ordering, later-like restoration, catalog/wishlist parity, stale published v1 defense. |
| Reload result | `web/src/api/types.ts`, `web/src/api/hooks.ts`, `web/src/api/hooks.test.ts` | Return explicit fresh/stale/failed result without discarding preserved UI state. |
| Workflow domain | create `web/src/components/feedbackWorkflowMachine.ts`, `feedbackWorkflowMachine.test.ts` | Pure transitions, command identity, release/tombstone invariants. |
| Provider API | create `web/src/components/FeedbackWorkflowContext.ts`, `FeedbackWorkflowProvider.tsx`, `FeedbackWorkflowProvider.test.tsx`, `FeedbackWorkflowStatus.tsx` | Registry, submit/poll/retry/reconcile side effects, per-game public state, page-level retry/status. |
| Visibility projection | create `web/src/components/feedbackVisibility.ts`, `feedbackVisibility.test.ts` | Pure filtering/count adjustment for all overview collections by stable game tombstones. |
| Dashboard consumer | `web/src/pages/DashboardPage.tsx`, `DashboardPage.test.tsx` | Mount provider, supply explicit reload, consume tombstoned overview, remove local locks/background ownership. |
| Form/card consumers | `web/src/components/FeedbackForm.tsx`, `FeedbackForm.test.tsx`, `RecommendationCard.tsx`, `RecommendationCard.test.tsx` | Form owns draft only; cards read per-game provider state; persisted action/reasons copy remains. |
| Calendar consumers | `CalendarFeedbackContext.ts`, `CalendarFeedbackProvider.tsx`, `FeedbackDialog.tsx`, `FeedbackLauncher.tsx`, `web/src/pages/CalendarPage.tsx` and tests | Calendar owns selection/focus only; provider supplies busy/status; tombstone closes/reconciles selection. |
| Obsolete ownership | delete `web/src/components/useFeedbackWorkflow.ts`, `useFeedbackWorkflow.test.tsx`; simplify `rankingWorkflow.ts` only if provider can reuse it unchanged | Remove local controller/polling/release owner and every compatibility callback/duplicate lock. |

### Planned TypeScript interfaces

Keep domain types in the named modules so components do not import provider internals:

```ts
// web/src/api/types.ts
export type SafeReloadReason = 'request_failed' | 'superseded' | 'aborted'
export type DashboardReloadResult =
  | { kind: 'fresh'; data: DashboardData }
  | { kind: 'stale'; data: DashboardData; reason: SafeReloadReason }
  | { kind: 'failed'; reason: SafeReloadReason }

// web/src/components/feedbackWorkflowMachine.ts
export type FeedbackWorkflowPhase =
  | 'submitting' | 'polling' | 'polling_unavailable' | 'rebuild_dead'
  | 'reconciling' | 'stale_unconfirmed'
export type CanonicalFeedbackCommand = Pick<FeedbackInput, 'action' | 'reasons' | 'freeText'>
export type FeedbackWorkflowRecord = {
  gameId: string
  sourceRecommendationId: string
  sourceRankingVersion: number
  gameName: string
  command: CanonicalFeedbackCommand
  idempotencyKey: string
  phase: FeedbackWorkflowPhase
  accepted: FeedbackAccepted | null
  diff: RankingDiff | null
  error: FeedbackWorkflowError | null
  tombstoned: boolean
}
export type PublicFeedbackWorkflowState = {
  phase: FeedbackWorkflowPhase | 'idle'
  accepted: boolean
  diff: RankingDiff | null
  error: string
  canRetry: boolean
  busy: boolean
  tombstoned: boolean
}
export function feedbackWorkflowReducer(state: FeedbackWorkflowState, event: FeedbackWorkflowEvent): FeedbackWorkflowState

// web/src/components/FeedbackWorkflowContext.ts
export type FeedbackSubmitIntent = {
  gameId: string
  recommendationId: string
  rankingVersion: number
  gameName: string
  action: FeedbackAction
  reasons?: string[]
  freeText?: string
}
export type FeedbackWorkflowContextValue = {
  stateFor(gameId: string): PublicFeedbackWorkflowState
  submit(input: FeedbackSubmitIntent): Promise<void>
  retry(gameId: string): void
  isBusy(gameId: string): boolean
  isTombstoned(gameId: string): boolean
}
export function useFeedbackWorkflows(): FeedbackWorkflowContextValue
export function useGameFeedback(gameId: string): PublicFeedbackWorkflowState & {
  submit(input: Omit<FeedbackSubmitIntent, 'gameId'>): Promise<void>
  retry(): void
}

// web/src/components/FeedbackWorkflowProvider.tsx
export type FeedbackWorkflowProviderProps = {
  csrfToken: string
  reload(): Promise<DashboardReloadResult>
  onRankingUpdated?(diff: RankingDiff): void
  children: ReactNode
  submitFeedback?: typeof apiSubmitFeedback
  readRankingDiff?: typeof getRankingDiff
  wait?: typeof waitForRanking
}
export function FeedbackWorkflowProvider(props: FeedbackWorkflowProviderProps): JSX.Element

// web/src/components/feedbackVisibility.ts
export function applyFeedbackTombstones(overview: RecommendationOverview, gameIds: ReadonlySet<string>): RecommendationOverview
export function findRecommendationByGameId(overview: RecommendationOverview, gameId: string): Recommendation | undefined
```

`FeedbackWorkflowError` should be a closed presentation-safe code union such as `submit_failed | polling_unavailable | rebuild_dead | stale_unconfirmed`; translate it to Russian copy in one provider/status helper. Reducer events must distinguish pre-acceptance failure, accepted identity, retry polling, terminal diff, reconciliation result, and page teardown. Do not expose `AbortController`, raw exceptions, CSRF, or identifiers through context.

## Test Commands

Before Task 1:

```bash
codegraph status .
codegraph sync . # only if status reports stale/pending files
codegraph context "centralized feedback workflow latest direct action candidate eligibility reload freshness tombstone" -p .
make wait-test-db
export TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable'
```

Use the same isolated URL for every Go integration command below. Do not recreate, truncate, or clean the application database. The final `make verify` remains mandatory for this cross-cutting workflow/persistence/read-model change; if pinned Playwright Chromium is unavailable, use the repository-documented explicit system-Chrome fallback and report it.

---

### Task 1: Make latest direct action control backend visibility and exact candidate eligibility

**Files:** Modify `internal/recommendations/retrieval.go`, `internal/recommendations/types.go`, `internal/recommendations/scoring.go`, `internal/recommendations/scoring_test.go`, `internal/recommendations/repository_integration_test.go`, `internal/recommendations/build_key_integration_test.go`; modify `internal/recommendations/overview_repository_integration_test.go` only if its fixture is the smallest way to assert every overview partition.

**Interfaces and query behavior:** Keep public repository interfaces unchanged. Replace lifetime-count hard exclusion with a current-action boolean in `Candidate`:

```go
type Candidate struct {
    // existing fields unchanged
    ExcludedByLatestDirectAction bool
    Dislikes int // historical soft evidence only
}

func Eligible(Candidate) bool
```

In both candidate sources, derive `latest_direct.action` from `feedback_events` joined through `recommendations.game_id`, scoped to the same user/game, restricted to `like/dislike/exclude`, ordered `created_at DESC, id DESC`. Scan `action IN ('dislike','exclude')` into `ExcludedByLatestDirectAction`. Keep aggregate `likes`/`dislikes` for `Factors.PositiveFeedback`, `Factors.NegativeFeedback`, profile vectors, and similar-game learning, but remove `COALESCE(f.dislikes,0)<2`, lifetime `bool_or(exclude)` eligibility, and obsolete `ShouldExcludeForFeedback`.

Preserve the already-implemented `Latest` read-time suppression of current `dislike` before `BuildOverview`; treat its stale-published-ranking regression as baseline evidence, not the RED target. Keep deterministic ordering and reminder neutrality intact. Extend that relation only where coverage proves a gap, such as legacy `exclude` membership or equal-`created_at` ID ordering, and continue returning current like/reasons for visible rows.

- [ ] **Step 1 — Impact check:** Run `codegraph impact Candidates -p .`, `codegraph impact Eligible -p .`, and `codegraph impact Latest -p .`; inspect callers and preserve current catalog/wishlist union, horizon, mode adjustment, explanation, and published-run behavior.
- [ ] **Step 2 — Preserve baseline read-defense evidence:** Rename the current stale-ranking fixture test to `TestLatestSuppressesDislikedGameWhileReplacementBuildFails` if that clearer name is not already present, and keep it green: published v1 contains exact and similar games; after first dislike and failed v2, `Latest` and every overview/calendar partition omit only the exact game. Add later-like, reminder, legacy-`exclude`, and equal-timestamp/higher-ID assertions only when absent from current coverage. This test protects existing behavior and is not expected to provide the initial RED.
- [ ] **Step 3 — RED candidate tests:** Replace `TestTwoDislikesExcludeExactCandidate` with `TestLatestDirectActionControlsExactCandidateEligibility`, asserting historical `Dislikes` alone do not hard-exclude while `ExcludedByLatestDirectAction` does. Add `TestCandidatesUseLatestDirectActionAcrossSources` in `repository_integration_test.go`: exercise one catalog candidate and one wishlist candidate through `dislike -> remind_me -> like`; first dislike excludes both exact games, reminder does not change that, later like restores both, and historical `Dislikes`/negative factor remains nonzero. In the same test or a separately named `TestCandidatesLatestDirectActionTieBreakAndLegacyExclude`, assert equal `created_at` resolves by `id DESC` and legacy `exclude` is current hard exclusion but a later like reverses it.
- [ ] **Step 4 — Verify honest RED:** First run the existing-defense baseline: `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test -race ./internal/recommendations -run 'TestLatestSuppressesDislikedGameWhileReplacementBuildFails'` and require it to pass. Then run `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test -race ./internal/recommendations -run 'Test(LatestDirectActionControlsExactCandidateEligibility|CandidatesUseLatestDirectActionAcrossSources|CandidatesLatestDirectActionTieBreakAndLegacyExclude)'`. Expected RED: candidate retrieval still uses lifetime `dislikes`/`bool_or(exclude)`, so the first dislike is not the current-action hard exclusion and a later like cannot reliably reverse accumulated dislike/legacy-exclude state. The failure must not claim that stale `Latest` suppression is missing.
- [ ] **Step 5 — Implement minimally:** Add the latest-direct CTE/lateral joins and one candidate boolean; change only `Eligible`'s hard exclusion predicate and remove the obsolete threshold helper. Match the established `Latest` action membership/order; change `Latest` only for a demonstrated missing legacy-`exclude` or tie-break assertion. Do not delete historical feedback or change scoring/vector counts.
- [ ] **Step 6 — Verify GREEN:** Repeat both commands from Step 4, then run `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test -race ./internal/recommendations`. Confirm no DB tests skip, the stale-read baseline stays green, and the similar game remains eligible.
- [ ] **Step 7 — Graph and non-committing checkpoint:** Run `codegraph sync .`, `codegraph affected internal/recommendations/retrieval.go internal/recommendations/types.go internal/recommendations/scoring.go -p .`, run any additional affected Go packages, then `git diff --check`, `git status --short`, and `git diff -- internal/recommendations/retrieval.go internal/recommendations/types.go internal/recommendations/scoring.go internal/recommendations/scoring_test.go internal/recommendations/repository_integration_test.go internal/recommendations/build_key_integration_test.go internal/recommendations/overview_repository_integration_test.go`. Do not stage or commit; identify the exact owned hunks in the task report.

### Task 2: Return explicit dashboard reload freshness

**Files:** Modify `web/src/api/types.ts`, `web/src/api/hooks.ts`, `web/src/api/hooks.test.ts`.

**Interfaces:** Add `SafeReloadReason` and `DashboardReloadResult` exactly as declared above. Change `useDashboardData().reload` from `Promise<void>` to `Promise<DashboardReloadResult>`. The hook may still publish `DashboardLoadState` for presentation, but every execution path—including superseded request, abort, preserved prior data, and initial failure—must return a discriminated result.

- [ ] **Step 1 — Impact check:** Run `codegraph impact useDashboardData -p .` and inspect every `reload()` caller. Record which callers need `void reload()` (ordinary user refresh/sync) versus which must await and branch on freshness (feedback provider).
- [ ] **Step 2 — RED tests:** In `hooks.test.ts`, assert a successful authoritative response returns `{kind:'fresh', data}`; a 503 with prior usable data returns `{kind:'stale', data: previous, reason:'request_failed'}`; a 503 without usable data returns `{kind:'failed', reason:'request_failed'}`; and an older superseded request returns a non-fresh result rather than `undefined`. Preserve assertions that UI state retains complete previous overview and only the newest request publishes state.
- [ ] **Step 3 — Verify RED:** Run `pnpm --dir web run test -- src/api/hooks.test.ts`. Expected failure: `reload()` currently resolves `undefined`, so none of the freshness discriminants exist.
- [ ] **Step 4 — Implement minimally:** Return the exact data used to publish state on success. In failure handling, compute the return value from the same previous-state decision that updates React state; do not infer freshness from `deriveFreshness` age labels—the network response is authoritative even if its timestamps make the presentation state `stale`/`unknown`. Superseded/aborted requests must never claim `fresh`.
- [ ] **Step 5 — Verify GREEN:** Repeat Step 3, then run `pnpm --dir web run typecheck`.
- [ ] **Step 6 — Graph and non-committing checkpoint:** Run `codegraph sync .`, `codegraph affected web/src/api/types.ts web/src/api/hooks.ts -p .`, run listed affected frontend tests, then `git diff --check`, `git status --short`, and `git diff -- web/src/api/types.ts web/src/api/hooks.ts web/src/api/hooks.test.ts`. Do not stage or commit; identify exact owned hunks.

### Task 3: Define the pure per-game workflow state machine and synchronous registry

**Files:** Create `web/src/components/feedbackWorkflowMachine.ts`, `web/src/components/feedbackWorkflowMachine.test.ts`; create `web/src/components/FeedbackWorkflowContext.ts` with context/types only.

**Interfaces:** Implement the TypeScript types and `feedbackWorkflowReducer`, `useFeedbackWorkflows`, and `useGameFeedback` signatures declared in Delivery Boundaries. Add internal exported testable helpers:

```ts
export function createFeedbackWorkflowRegistry(): FeedbackWorkflowRegistry
export type FeedbackWorkflowRegistry = {
  acquire(intent: FeedbackSubmitIntent, command: CanonicalFeedbackCommand, idempotencyKey: string): FeedbackWorkflowRecord | null
  get(gameId: string): FeedbackWorkflowRecord | undefined
  release(gameId: string): boolean
}
export function canonicalFeedbackCommand(intent: FeedbackSubmitIntent): CanonicalFeedbackCommand
```

Canonicalization preserves the existing fixed reason taxonomy order, removes absent optional fields, and strips reasons/free text for `remind_me`. UI should prevent duplicates, while the API remains the authority that rejects invalid/unknown inputs; do not silently turn invalid server-bound values into different semantic reasons.

- [ ] **Step 1 — RED tests:** Add table-driven reducer/registry tests for: two same-turn acquisitions of one `gameId` yield one record; different games acquire independently; command/idempotency remain frozen; pre-acceptance `submit_failed` releases exactly once; accepted/polling/dead/network/stale states retain ownership; terminal diff alone does not release; fresh dislike keeps tombstone then releases; fresh like clears tombstone then releases; reminder never changes tombstone; stale/failed reconciliation keeps retry; duplicate release is a no-op; teardown marks local work aborted without pretending the server job was cancelled. Add multi-reason ordering and reminder stripping cases.
- [ ] **Step 2 — Verify RED:** Run `pnpm --dir web run test -- src/components/feedbackWorkflowMachine.test.ts`. Expected failure: the module, registry, and transitions do not exist.
- [ ] **Step 3 — Implement minimally:** Keep the reducer pure and exhaustive. The registry performs the synchronous guard before any state dispatch. Reducer state is keyed by game ID and stores only frozen safe records. Release/tombstone decisions require an explicit reconciliation event carrying `DashboardReloadResult`; a `succeeded` poll event cannot release.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2 and run `pnpm --dir web run typecheck`.
- [ ] **Step 5 — Graph and non-committing checkpoint:** Run `codegraph sync .`, `codegraph affected web/src/components/feedbackWorkflowMachine.ts web/src/components/FeedbackWorkflowContext.ts -p .`, then `git diff --check`, `git status --short`, and path-scoped diffs for the three task files. Do not stage or commit.

### Task 4: Implement provider-owned submit, polling, retry, tombstone, and reconciliation

**Files:** Create `web/src/components/FeedbackWorkflowProvider.tsx`, `web/src/components/FeedbackWorkflowProvider.test.tsx`, `web/src/components/FeedbackWorkflowStatus.tsx`; reuse `web/src/components/rankingWorkflow.ts` and its test unless a small signature extension is necessary.

**Interfaces:** Implement `FeedbackWorkflowProviderProps` and context methods declared above. `submit()` resolves after acquisition/submission handling, not necessarily after the asynchronous accepted workflow finishes. `retry(gameId)` branches by record phase: retry polling with accepted identity for `polling_unavailable/rebuild_dead`, or retry only reload reconciliation for `stale_unconfirmed`; it never reposts feedback. Render `FeedbackWorkflowStatus` inside the provider so retry remains reachable after cards/dialogs unmount.

- [ ] **Step 1 — RED provider tests:** With injected submit/read/wait/reload fakes, assert: synchronous same-game dashboard/calendar submits cause one POST while another game posts; accepted dislike immediately makes `isTombstoned(gameId)` true; unmounting the initiating consumer does not abort provider polling; terminal success invokes reload and `onRankingUpdated`; dead and network outcomes retain lock/tombstone and show Russian retry copy; retry reuses the accepted job and causes zero extra POSTs; stale/failed reload retains lock/tombstone and switches retry to reconciliation; fresh dislike absence/current dislike keeps tombstone then releases; later like cannot submit until prior release and clears tombstone only after fresh item with `latestFeedbackAction:'like'`; fresh data that still reports dislike cannot clear it; reminder posts `{action:'remind_me', idempotencyKey}` only and preserves prior direct state; provider teardown aborts outstanding local work.
- [ ] **Step 2 — Verify RED:** Run `pnpm --dir web run test -- src/components/FeedbackWorkflowProvider.test.tsx`. Expected failure: provider/status modules do not exist and current component hook aborts with its consumer.
- [ ] **Step 3 — Implement acquisition/submission:** Create one provider-lifetime registry ref and one provider-lifetime controller per active game. Acquire before calling the injected API. Dispatch submitting/accepted events around the frozen `FeedbackInput`; classify failures before acceptance as releasable `submit_failed` with safe copy.
- [ ] **Step 4 — Implement polling/retry:** Move accepted job identity and calls to `pollRankingWorkflow` into the provider. Dead/network states remain records with `canRetry=true`. Poll retry must preserve `accepted.eventId`, `rebuildJobId`, `previousRankingVersion`, command, and idempotency key.
- [ ] **Step 5 — Implement reconciliation:** After a succeeded diff, await `reload()`. For `fresh`, use `findRecommendationByGameId`: dislike/exclude or authoritative absence after the server's fresh read retains the tombstone; like requires the matching item to report latest like before clearing; reminder leaves visibility unchanged. Treat a contradictory/missing later-like confirmation as `stale_unconfirmed`, not success. For `stale/failed`, retain record and expose reload retry. Invoke `onRankingUpdated` once per newly observed terminal diff, not on every reconciliation retry.
- [ ] **Step 6 — Verify GREEN:** Repeat Step 2; then run `pnpm --dir web run test -- src/components/feedbackWorkflowMachine.test.ts src/components/FeedbackWorkflowProvider.test.tsx src/components/rankingWorkflow.test.ts` and `pnpm --dir web run typecheck`.
- [ ] **Step 7 — Graph and non-committing checkpoint:** Run `codegraph sync .`, `codegraph affected web/src/components/FeedbackWorkflowProvider.tsx web/src/components/FeedbackWorkflowStatus.tsx -p .`, run affected tests, then `git diff --check`, `git status --short`, and path-scoped diffs for the provider/status/ranking-workflow files. Do not stage or commit.

### Task 5: Mount the provider and derive every dashboard/calendar collection through tombstones

**Files:** Create `web/src/components/feedbackVisibility.ts`, `web/src/components/feedbackVisibility.test.ts`; modify `web/src/pages/DashboardPage.tsx`, `web/src/pages/DashboardPage.test.tsx`; modify `web/src/pages/CalendarPage.tsx` only enough to consume the already-filtered calendar and remove workflow-coordination props after Task 6 lands.

**Interfaces:** Implement `applyFeedbackTombstones` and `findRecommendationByGameId` from Delivery Boundaries. Split `DashboardPage` into a thin data-loading/provider boundary and an internal consumer if needed so the same `useDashboardData().reload` instance can be passed to the provider and its tombstone state consumed below it.

- [ ] **Step 1 — RED visibility tests:** Build one overview where the same `gameId` is featured, present in discovery/wishlist fixtures, and repeated in exact/month/quarter/year/comingSoon/tba buckets. Assert one tombstone removes every representation by stable game ID, promotes the next non-tombstoned discovery item to featured without duplication, adjusts discovery/wishlist totals once per source membership, preserves source status/timestamps/catalog status, returns fresh arrays/objects without mutating input, and leaves unrelated games/counts unchanged.
- [ ] **Step 2 — RED dashboard test:** Replace the current expectation that a pending disliked game stays visible. Assert accepted dislike removes it immediately from card grids and calendar while its read-diff promise is pending; the POST-ing form unmounts but polling continues; a different game's form remains usable; dead/network/stale reload displays the provider-owned retry even though the original card is absent; terminal success plus fresh reload invokes the ranking update path.
- [ ] **Step 3 — Verify RED:** Run `pnpm --dir web run test -- src/components/feedbackVisibility.test.ts src/pages/DashboardPage.test.tsx`. Expected failure: there is no central tombstone projection, current dashboard renders source overview directly, and local workflow ownership is lost with the card.
- [ ] **Step 4 — Implement projection and boundary:** Mount `FeedbackWorkflowProvider` once beneath authenticated dashboard data loading. Pass `user.csrfToken`, explicit `reload`, and a stable ranking-diff callback. Derive the displayed overview with `applyFeedbackTombstones(sourceData.overview, tombstonedGameIds)`; if the context intentionally exposes only `isTombstoned`, derive the set from unique game IDs in the source overview rather than broadening the public API.
- [ ] **Step 5 — Remove dashboard ownership:** Delete `activeGames`, `busyGameIds`, `acquireGame`, `releaseGame`, `settleGameFeedback`, card/calendar lock callbacks, and any `BackgroundFeedbackWorkflow`. Keep ordinary sync/profile ranking refresh behavior, but do not let those callbacks release feedback records. Use filtered overview consistently for `UpcomingWindows`, all cards, totals, and `CalendarPage`.
- [ ] **Step 6 — Verify GREEN:** Repeat Step 3, then `pnpm --dir web run test -- src/api/hooks.test.ts src/components/FeedbackWorkflowProvider.test.tsx src/pages/DashboardPage.test.tsx` and `pnpm --dir web run typecheck`.
- [ ] **Step 7 — Graph and non-committing checkpoint:** Run `codegraph sync .`, `codegraph affected web/src/pages/DashboardPage.tsx web/src/components/feedbackVisibility.ts -p .`, run affected tests, then `git diff --check`, `git status --short`, and path-scoped diffs for the visibility/dashboard/calendar files. Do not stage or commit.

### Task 6: Convert form, card, dialog, and calendar to provider consumers and remove obsolete owners

**Files:** Modify `web/src/components/FeedbackForm.tsx`, `FeedbackForm.test.tsx`, `RecommendationCard.tsx`, `RecommendationCard.test.tsx`, `FeedbackDialog.tsx`, `FeedbackDialog.test.tsx`, `FeedbackLauncher.tsx`, `FeedbackLauncher.test.tsx`, `CalendarFeedbackContext.ts`, `CalendarFeedbackProvider.tsx`, `web/src/pages/CalendarPage.tsx`, `CalendarPage.test.tsx`; delete `web/src/components/useFeedbackWorkflow.ts`, `useFeedbackWorkflow.test.tsx`; update imports/types in any directly affected components.

**Interfaces and ownership:**

- `FeedbackForm` props become recommendation/game identity, persisted `initialAction`/`initialReasons`, and `readOnly`; it obtains `useGameFeedback(gameId)`, owns only `action`, selected reasons, free text, and validation, then sends a `FeedbackSubmitIntent`.
- `RecommendationCard` no longer accepts `onFeedbackAccepted`, `onSubmissionStart`, `onWorkflowReleased`, or workflow-specific ranking callbacks. It passes `item.gameId`, snapshot recommendation ID/version, name, and persisted reaction fields to the form/launcher.
- `FeedbackDialog` owns `open`, focus trap/restore, and presentation only. It never creates a hook, controller, poller, or lock callback.
- `CalendarFeedbackContext` exposes only selection/focus operations plus read-only availability; busy state comes from `useGameFeedback(item.gameId)` in launchers/forms.
- `CalendarFeedbackProvider` owns `selected`, `open`, `opener`, and fallback focus. If the selected game becomes tombstoned or disappears from filtered calendar, close and clear it; workflow status/retry remains in the page-level provider status area.
- `CalendarPage` drops `busyGameIds`, `onGameSubmissionStart`, `onGameWorkflowReleased`, `onGameFeedbackAccepted`, and `onGameRankingUpdated` props.

- [ ] **Step 1 — RED form/card tests:** Update tests to render consumers under a provider fixture. Assert checkbox reasons submit once in canonical order, persisted `latestFeedbackAction/latestFeedbackReasons` render `Ранее выбрано: Нравится` plus labels after reload, reminder sends no reasons/free text, read-only never submits, and all representations of one active game are disabled while another game remains enabled.
- [ ] **Step 2 — RED calendar/dialog tests:** Assert accepted calendar dislike closes and clears the selected dialog immediately; dead/network status is still reachable in page-level status; retry works with no selected dialog; an in-flight dashboard dislike blocks a calendar like POST for the same `gameId`; an in-flight calendar reaction blocks the dashboard form; after terminal fresh completion a later like can submit with a new idempotency key and only fresh like confirmation restores visibility/future launch; focus returns to the surviving opener or view-switch fallback.
- [ ] **Step 3 — Verify RED:** Run `pnpm --dir web run test -- src/components/FeedbackForm.test.tsx src/components/RecommendationCard.test.tsx src/components/FeedbackDialog.test.tsx src/components/FeedbackLauncher.test.tsx src/pages/CalendarPage.test.tsx src/pages/DashboardPage.test.tsx`. Expected failure: consumers still instantiate `useFeedbackWorkflow` or depend on dashboard/calendar lock callbacks, and accepted workflow/status is coupled to dialog/card lifetime.
- [ ] **Step 4 — Migrate form/card:** Replace local workflow imports with provider context. Preserve current Russian persisted-reaction and multi-reason UI. Disable edits from public per-game busy state; do not hide an accepted reminder/like status merely because local draft state resets.
- [ ] **Step 5 — Migrate calendar/dialog:** Reduce calendar context/provider to selection and focus. Derive launcher busy state from stable game ID. Reconcile selected item against filtered calendar/tombstone on each render; close without aborting. Keep accessible dialog focus trapping and fallback behavior.
- [ ] **Step 6 — Remove obsolete code:** Delete `useFeedbackWorkflow.ts` and its tests only after all imports are gone. Remove optional injected `workflow`, `FeedbackWorkflowOptions`, `FeedbackWorkflowState`, local `FeedbackPhase`, parent-owned polling handoff, release callbacks, `lockedId`, `isGameBusy` duplication, and any background workflow component. Keep `rankingWorkflow.ts` as the provider's low-level poll helper if still useful; do not create a second owner there.
- [ ] **Step 7 — Verify GREEN:** Repeat Step 3, then run `rg -n "useFeedbackWorkflow|BackgroundFeedbackWorkflow|onWorkflowReleased|onSubmissionStart|activeGames|busyGameIds|lockedId" web/src` and require no obsolete ownership references (an intentional provider API name may be documented). Run `pnpm --dir web run test`, `pnpm --dir web run typecheck`, `pnpm --dir web run lint`, and `pnpm --dir web run build`.
- [ ] **Step 8 — Graph and non-committing checkpoint:** Run `codegraph sync .`, `codegraph affected web/src/components/FeedbackForm.tsx web/src/components/RecommendationCard.tsx web/src/components/FeedbackDialog.tsx web/src/components/CalendarFeedbackProvider.tsx web/src/pages/CalendarPage.tsx -p .`, run affected tests, then `git diff --check`, `git status --short`, and path-scoped diffs for every file named in this task, including deletion diffs. Do not stage or commit; report which hunks are owned and which pre-existed.

### Task 7: Close cross-layer regressions and run the release gate

**Files:** Modify only focused tests or implementation files shown by failures: `internal/recommendations/build_key_integration_test.go`, `repository_integration_test.go`, `overview_repository_integration_test.go`; `web/src/pages/DashboardPage.test.tsx`, `CalendarPage.test.tsx`, `web/src/components/FeedbackWorkflowProvider.test.tsx`; `web/tests/e2e/smoke.spec.ts` only if its existing authenticated fake workflow can cover the behavior without adding a new harness.

**Acceptance matrix:** One scenario must cover `published v1 -> accepted first dislike -> pending/dead/failed v2`: backend `Latest`/overview omit exact game, frontend removes it immediately, similar game remains, retry remains reachable. A second scenario must cover `dislike fully reconciled -> later like -> fresh overview`: no concurrent POST, current eligibility/visibility returns, historical dislike evidence remains. A third preserves multiple reasons and reminder semantics end to end.

- [ ] **Step 1 — Cross-layer RED review:** Run the focused backend and frontend suites from Tasks 1 and 6 together. Add only missing regression assertions identified by the five Review Focus items: exact six calendar buckets, same-turn acquisition, initiator unmount, dead/network/reload retry, later-like ordering, multi-reason persistence, and reminder neutrality. If every assertion already exists, record that evidence instead of duplicating tests.
- [ ] **Step 2 — Verify any new RED:** Run the narrowest command for each new assertion and record its expected behavioral failure. Do not accept fixture/type/infrastructure failures as RED.
- [ ] **Step 3 — Minimal fixes:** Change only the owner/query/projection responsible for each observed failure. Do not add fallback component hooks, unlock-on-timeout, local storage, queued same-game actions, or a second latest-action query with different ordering.
- [ ] **Step 4 — Focused GREEN:** Run `TEST_DATABASE_URL="$TEST_DATABASE_URL" GOWORK=off go test -race ./internal/recommendations` and `pnpm --dir web run test -- src/api/hooks.test.ts src/components/feedbackWorkflowMachine.test.ts src/components/FeedbackWorkflowProvider.test.tsx src/components/feedbackVisibility.test.ts src/components/FeedbackForm.test.tsx src/components/RecommendationCard.test.tsx src/components/FeedbackDialog.test.tsx src/components/FeedbackLauncher.test.tsx src/pages/DashboardPage.test.tsx src/pages/CalendarPage.test.tsx`.
- [ ] **Step 5 — Full frontend:** Run `pnpm --dir web run test`, `pnpm --dir web run typecheck`, `pnpm --dir web run lint`, and `pnpm --dir web run build`; require zero failures. If an unrelated dirty-worktree test fails, prove it is unrelated with the focused suite and report it rather than altering that feature.
- [ ] **Step 6 — CodeGraph and repository checks:** Run `codegraph sync .`, then `codegraph affected` over every changed product file. Run the additional affected tests, `git diff --check`, and `git status --short`. Inspect the final diff to ensure no `.env`, generated CodeGraph data, unrelated files, migration, or hand-edited sqlc output is included.
- [ ] **Step 7 — Cross-cutting verification:** Run `make verify` with the isolated test database available. If the pinned browser download is unavailable, use the documented system-Chrome fallback, rerun the browser gate, and record both commands/results. Do not claim completion from subagent reports; the coordinating agent must inspect fresh zero-failure output.
- [ ] **Step 8 — Final non-committing checkpoint:** Run `git status --short`, `git diff --check`, and path-scoped diffs for the exact files changed by this plan. Compare against the pre-task dirty-worktree inventory and report owned hunks separately from unrelated work. Do not stage or commit. If the user later requests a commit, first prove exact owned hunks can be isolated and obtain that authorization.

## Spec Coverage Self-Review

| Spec section | Plan coverage |
|---|---|
| 1. Objective | Goal, Tasks 4–7: immediate hide, persistent owner, per-game serialization, retry, fresh reconciliation. |
| 2. Current Baseline and Failure Modes | Architecture, Delivery Boundaries, explicit removal of local hook/dashboard/calendar/background owners. |
| 3. Product Semantics | Global Constraints, Tasks 1 and 6: like/dislike/reminder, multiple reasons, soft evidence, later action. |
| 4. Architecture | Tasks 3–6: page provider, pure reducer, consumer-only components, provider teardown boundary. |
| 5. Workflow Identity and Serialization | Task 3 registry and Task 4 submit tests: stable game key, synchronous acquisition, frozen key/payload, exactly-one release. |
| 6. State Machine | Declared phases and Tasks 3–4 transition/retry/reconciliation tests. |
| 7. Optimistic Visibility and Freshness | Tasks 2, 4, and 5: discriminated reload result, every overview partition, counts, later-like confirmation. |
| 8. Consumer Interfaces | Planned interfaces plus Task 6 prop/context migration and page-level status. |
| 9. Backend Defense in Depth | Task 1: deterministic latest action, stale published read suppression, reversible candidate eligibility, historical soft evidence. |
| 10. Idempotency and Error Handling | Global Constraints and Tasks 3–4: frozen key, no repost on retry, safe errors, no same-game queue. |
| 11. Data and Migration Impact | Global Constraints: no new migration/storage; existing durable IDs and migration `00013` preserved. |
| 12. Testing Strategy | Five Review Focus items, per-task RED/GREEN commands, isolated DB, full frontend, CodeGraph, `make verify`. |
| 13. Implementation Boundary | Delivery table and Tasks 5–6 explicitly remove obsolete distributed ownership rather than layering. |

Self-review gaps corrected while writing this plan: the live backend still used lifetime `dislikes`/legacy hidden aggregation for candidate eligibility, so Task 1 now makes that the honest RED while treating the already-implemented stale `Latest` suppression as protected baseline evidence; the live reload returned `undefined`, so superseded/aborted paths now have a non-fresh contract; the live calendar/dashboard callbacks contained multiple release owners, so deletion criteria and an `rg` zero-reference gate are included rather than leaving a compatibility layer; and every task now ends with non-committing path-scoped evidence because whole-file staging would capture unrelated work from the shared dirty tree.
