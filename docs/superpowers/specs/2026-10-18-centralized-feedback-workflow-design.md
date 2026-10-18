# Centralized Feedback Workflow Design

**Date:** 2026-10-18
**Status:** Draft for review
**Related feedback design:** `docs/superpowers/specs/2026-10-18-feedback-preference-corrections-calendar-design.md`
**Related presentation design:** `docs/superpowers/specs/2026-10-18-discovery-wishlist-presentation-design.md`

## 1. Objective

Make one page-level workflow the single owner of recommendation feedback from
submission through ranking reconciliation.

The triggering failure was concrete: the user selected `Не моё` for a game,
the feedback event was accepted, but the game remained visible. The first
server-side dislike already excludes the exact game from future candidate
builds, yet the UI can continue rendering an older published ranking while the
asynchronous rebuild is pending, failed, or unavailable. Earlier client fixes
also split ownership between card-local hooks, calendar dialogs, optimistic
tombstones, and background polling. That split permits unmounts to abort an
accepted workflow and permits competing reactions for the same game.

Success means:

- an accepted dislike immediately removes the game from every dashboard and
  calendar presentation;
- the accepted workflow continues even if the initiating component unmounts;
- one stable `gameId` has at most one active feedback workflow on the page;
- other games remain interactive;
- terminal success is reconciled against a fresh authoritative overview;
- dead jobs and unavailable polling expose a reachable retry surface;
- a later like can restore a previously tombstoned game only after the earlier
  workflow is terminal and fresh server data confirms the like;
- the backend read filter remains a defense-in-depth guarantee when the client
  has no local workflow state.

## 2. Current Baseline and Failure Modes

Feedback is persisted before profile and recommendation rebuild jobs complete.
The latest published ranking therefore remains a valid fallback during a
rebuild. Without an additional read filter or local tombstone, that fallback
can still contain the newly disliked game.

The current partial frontend implementation distributes responsibility among:

- `FeedbackForm` and `useFeedbackWorkflow`, which submit and poll locally;
- recommendation cards, which can unmount immediately after an optimistic
  dislike;
- `CalendarFeedbackProvider`, which keeps selected-dialog state;
- `DashboardPage`, which owns tombstones and a separate background poller;
- per-surface callbacks and locks that must agree on release ordering.

This creates several invalid states:

- accepting a dislike unmounts the card and aborts the hook that owns polling;
- dashboard and calendar representations of the same game can submit competing
  actions before either response arrives;
- a hidden calendar dialog can keep every other launcher locked after a dead or
  network-error result;
- multiple code paths can release the same game lock;
- a late response can clear or retain a tombstone without proving which server
  action is authoritative;
- a reload failure can silently preserve stale data while client code treats
  the reload as confirmation.

The implementation must refactor this partial solution into the centralized
model below. It must not add another coordination layer alongside the existing
card, calendar, and dashboard workflow owners.

## 3. Product Semantics

### Direct actions

The direct actions remain:

- `like`;
- `dislike`;
- `remind_me`.

`like` and `dislike` may contain zero or more canonical predefined reasons and
optional free text up to the existing limit. `remind_me` contains neither
reasons nor free text and does not replace the latest direct like/dislike
reaction.

Reasons are one event payload, not separate feedback events. Their canonical
order, validation, idempotency, classification, and soft preference effects
remain defined by the existing feedback contract. A dislike both:

- immediately hides the exact game from the current page; and
- remains a soft negative signal for similar games and supported preference
  adjustments.

The exact disliked game is excluded after the first dislike and remains
excluded while `dislike` is its latest direct action. A later authoritative
`like` reverses that exact-game exclusion after fresh reconciliation. The
historical dislike remains available as learning evidence. Similar games are
not hard-excluded solely because they resemble it.

### Later reactions

The application does not queue competing actions for the same game. While one
workflow is active, another dashboard or calendar representation of that game
is disabled. The user may submit a later action only after the previous
workflow reaches terminal success or is explicitly retried to terminal
success.

A later authoritative like may restore visibility. It does not remove the
tombstone merely because its POST response arrived. Restoration occurs only
after its ranking workflow succeeds and a fresh overview confirms the latest
direct action. This ordering prevents an older dislike response and newer like
response from racing in local state.

## 4. Architecture

Add one `FeedbackWorkflowProvider` at the authenticated dashboard page boundary.
It is the only owner of mutable feedback workflows and is keyed by stable
`gameId`, never by recommendation ID. Recommendation IDs are ranking-version
snapshots and can change after every rebuild.

The provider owns:

- synchronous per-game acquisition before a POST;
- the frozen command payload and idempotency key;
- submission state and accepted event/job identity;
- polling and retry controllers;
- terminal diff and safe public error state;
- the per-game interaction lock;
- optimistic tombstones;
- authoritative freshness reconciliation;
- release of the workflow and lock.

`RecommendationCard`, `FeedbackForm`, `FeedbackDialog`, dashboard collections,
and calendar presentations become consumers. They render provider state and
send intents to it. They do not own `AbortController`, accepted job identity,
poll loops, tombstone lifetime, or lock release.

Unmounting a card or dialog removes only that view. It never cancels an
accepted workflow. The provider itself aborts outstanding requests only when
the authenticated page unmounts or the session is replaced.

## 5. Workflow Identity and Serialization

The provider stores at most one active workflow per `gameId`:

```ts
type FeedbackWorkflowRecord = {
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
```

Acquisition is synchronous against a ref-backed registry before submission.
React state alone is insufficient because two surfaces can dispatch in the
same render turn. If the game is already active, the provider rejects the
second intent locally and performs no POST.

Other game IDs have independent records and remain usable. There is no global
feedback mutex.

The provider is the exactly-one release owner. Consumers never call an unlock
callback. A record is released only after:

- submission failed before acceptance, leaving no accepted server workflow;
- a non-retryable validation response was returned before acceptance; or
- terminal success was reconciled with explicit freshness.

Dead and temporarily unavailable polling states remain owned and retryable.
They do not release the game merely to make the UI look unlocked.

## 6. State Machine

```text
idle
  -> submitting
       -> submit_failed          -> idle
       -> accepted
            -> polling
                 -> polling_unavailable -> retrying -> polling
                 -> rebuild_dead        -> retrying -> polling
                 -> succeeded
                      -> reconciling
                           -> fresh_confirmed -> released
                           -> stale_unconfirmed -> retrying_reconciliation
```

Rules:

- `idle -> submitting` atomically acquires the game.
- The idempotency key and canonical payload are frozen at acquisition.
- `accepted` stores `eventId`, `rebuildJobId`, and
  `previousRankingVersion` before polling begins.
- Poll retries reuse the accepted job identity and never repost feedback.
- A network error after acceptance is `polling_unavailable`, not submit
  failure.
- `rebuild_dead` and `polling_unavailable` expose a visible retry action.
- `succeeded` is not sufficient to release the game or clear a tombstone.
- `reconciling` requests authoritative overview/profile data.
- Only `fresh_confirmed` can apply the final like/dislike visibility state and
  release the game.
- A stale fallback remains usable for unrelated browsing but cannot confirm a
  later like or release ordering-sensitive local state.

`remind_me` follows the same submit/accepted/poll/retry lifecycle but does not
create a tombstone and does not require a ranking action to change visibility.
Its terminal reconciliation may release after fresh workflow status confirms
completion; it must not overwrite persisted direct reaction display.

## 7. Optimistic Visibility and Freshness

### Dislike

On successful feedback acceptance, the provider marks the game tombstoned.
The dashboard derives all presentation collections from authoritative data plus
the tombstone set:

- discovery featured item;
- discovery grid;
- wishlist grid;
- every exact and imprecise calendar bucket;
- selected calendar feedback item and resume affordances.

The tombstone uses `gameId`, so it survives recommendation-version changes and
stale reloads. Counts displayed from the current overview are adjusted
consistently without relabeling wishlist and discovery membership.

### Explicit reload result

Dashboard data loading must return an explicit result rather than encode
freshness only as component state:

```ts
type DashboardReloadResult =
  | { kind: 'fresh'; data: DashboardData }
  | { kind: 'stale'; data: DashboardData; reason: SafeReloadReason }
  | { kind: 'failed'; reason: SafeReloadReason }
```

`fresh` means the request completed successfully and returned authoritative
current data. `stale` means preserved prior data is intentionally displayed.
`failed` means no usable result was obtained. Callers must not infer freshness
from absence of an exception.

After terminal workflow success, the provider awaits this result:

- fresh data with latest direct `dislike` keeps the tombstone and releases the
  workflow;
- fresh data with latest direct `like` removes the tombstone and releases the
  workflow;
- a reminder leaves the direct tombstone/reaction unchanged;
- stale or failed data keeps the existing tombstone and the workflow's
  reconciliation retry surface.

This makes a later like restoration authoritative and ordered. It also avoids
briefly resurfacing a disliked game when an old overview is retained after a
reload failure.

## 8. Consumer Interfaces

The provider exposes a narrow context:

```ts
type FeedbackWorkflowContextValue = {
  stateFor(gameId: string): PublicFeedbackWorkflowState
  submit(input: {
    gameId: string
    recommendationId: string
    rankingVersion: number
    gameName: string
    action: FeedbackAction
    reasons?: FeedbackReason[]
    freeText?: string
  }): Promise<void>
  retry(gameId: string): void
  isBusy(gameId: string): boolean
  isTombstoned(gameId: string): boolean
}
```

The public state contains presentation-safe phase, accepted status, diff,
error copy, and retry availability. It does not expose an abort controller,
raw server error, CSRF token, user ID, SteamID, or job payload.

Consumer behavior:

- `FeedbackForm` owns only editable draft state and validation before submit;
- `FeedbackDialog` owns only visibility and focus restoration;
- `RecommendationCard` renders the provider state for its `gameId`;
- `CalendarFeedbackProvider` coordinates one dialog selection but not workflow
  lifetime;
- `DashboardPage` derives filtered collections from provider tombstones and
  supplies the provider with the explicit reload function;
- all representations of a busy game are disabled consistently;
- a hidden initiating component does not hide dead/network status: the page
  renders a provider-owned workflow status area with retry controls.

The provider may render one compact status per active or retryable game. Error
copy is safe and actionable, for example:

- `Отзыв сохранён, но статус пересборки пока недоступен.`;
- `Пересборка рекомендаций завершилась с ошибкой.`;
- `Данные пока не подтверждены — повторить обновление.`

## 9. Backend Defense in Depth

The frontend tombstone is an immediate UX guarantee, not the sole correctness
boundary.

The recommendation `Latest`/overview read path continues to suppress a game
when the latest direct action for the same user and game is `dislike` or legacy
`exclude`, ordered deterministically by `created_at DESC, id DESC`.
`remind_me` does not overwrite that direct action. A later direct `like` is the
authoritative opposite action.

This read filter applies before `BuildOverview`, so discovery, wishlist, and
all calendar partitions share the same protection even while the latest
published ranking predates the feedback event. Candidate rebuilding uses the
same latest-direct-action rule for exact-game eligibility: current dislike or
legacy exclude removes the game, while a later like makes it eligible again.
Historical negative evidence remains available to the profile and preference
signals for similar games; eligibility must not be based on a lifetime count
that makes the direct reaction impossible to reverse.

The backend behavior protects page reloads, new sessions, clients without
optimistic state, queued rebuilds, and failed replacement rankings. The client
must still tombstone immediately because an already-rendered page does not
re-read automatically at acceptance time.

## 10. Idempotency and Error Handling

- Each new command receives one client-generated idempotency key.
- Duplicate clicks and retries before acceptance reuse the frozen command.
- Poll and reconciliation retries never submit the feedback event again.
- A second action for the same game is rejected while the first workflow owns
  the game.
- A later action after release receives a new idempotency key and is serialized
  after the previous terminal workflow.
- Invalid reasons, duplicate reasons, unknown reasons, conflicting legacy
  `reason`/`reasons`, and excessive free text are rejected by existing API
  validation and leave no accepted workflow.
- Abort caused by page/session teardown stops local work but does not imply the
  accepted server job was cancelled.
- Safe errors never include raw response bodies, identifiers, job payloads,
  prompts, secrets, or personal data.

If reconciliation remains unavailable, the game stays locked only against a
competing reaction for that same game. Other games and non-feedback dashboard
features remain available.

## 11. Data and Migration Impact

This design adds no new database table, column, or migration beyond the
already-approved feedback reasons and reaction persistence work.

Existing durable identities remain sufficient:

- `feedback_events.id` and `idempotency_key` identify the accepted command;
- `rebuildJobId` identifies workflow polling;
- `games.id` is the stable coordination key;
- recommendation IDs remain immutable ranking-version snapshots;
- latest direct reaction ordering remains `created_at DESC, id DESC`.

Provider state is in-memory page state. A full page reload reconstructs truth
from the backend latest-reaction read model and does not attempt to persist
client locks in browser storage.

## 12. Testing Strategy

### State-machine unit tests

- synchronous same-game acquisition permits exactly one submit;
- different game IDs proceed independently;
- duplicate clicks reuse one idempotency key;
- pre-acceptance failure releases ownership;
- accepted workflow survives consumer unmount;
- polling retry reuses event/job identity and performs no POST;
- dead and network states retain ownership and expose retry;
- terminal success does not release before reconciliation;
- stale/failed reconciliation retains tombstone and retry state;
- fresh dislike confirmation retains tombstone and releases;
- fresh later-like confirmation removes tombstone and releases;
- reminder completion preserves the latest direct reaction;
- teardown aborts local requests without pretending to cancel accepted server
  work.

### Dashboard and calendar integration tests

- dashboard dislike acceptance immediately removes the game from featured,
  discovery, wishlist, and every calendar bucket where it appears;
- removing the initiating card does not stop polling or the eventual reload;
- an in-flight dashboard action disables the same game's calendar launcher and
  causes no second POST;
- an in-flight calendar action disables the same game's dashboard form;
- other games remain interactive throughout;
- dead and network failures show a page-level retry even when the original
  card/dialog is gone;
- retry reaches terminal success and refreshes the dashboard;
- reload returning stale data does not clear the tombstone or game lock;
- a later serialized like clears the tombstone only after fresh authoritative
  data reports the like;
- inverse response ordering cannot occur because same-game actions are not
  queued or submitted concurrently;
- multiple selected reasons are submitted as one event and restored from the
  latest direct reaction;
- reminders create no tombstone and do not erase displayed like/dislike state.

### Backend integration tests

- publish ranking v1 containing a game, persist its first dislike, and verify
  `Latest` and all overview/calendar partitions suppress it before v2 exists;
- fail v2 and verify the old published run remains readable without the
  disliked game;
- verify a similar game remains eligible so the dislike still acts as a soft
  preference signal rather than a category exclusion;
- verify deterministic latest-action ordering for equal timestamps by ID;
- verify a reminder after dislike does not resurface the game;
- verify a later like becomes the latest direct action and is visible after an
  authoritative fresh read;
- verify a later like restores exact-game candidate eligibility without
  deleting historical negative learning evidence;
- verify catalog and wishlist candidate sources share exact-game exclusion.

### Required verification

- focused provider/state-machine and dashboard/calendar tests;
- complete frontend test, typecheck, lint, and production build;
- relevant Go tests with `GOWORK=off` and `-race` against isolated
  `TEST_DATABASE_URL`;
- `codegraph sync` and affected-test review after implementation;
- `git diff --check` and, for the completed cross-cutting change,
  `make verify` when the shared worktree is suitable.

## 13. Implementation Boundary

The implementation is a refactor of workflow ownership, not a new product
feature and not another patch on the partial callback/lock structure.

It should:

- introduce the provider and state machine first;
- move submission, accepted job state, polling, retry, tombstones, and release
  into it;
- convert forms, cards, dialogs, dashboard, and calendar to consumers;
- remove obsolete component-owned polling, dashboard background workflow,
  duplicate release callbacks, and surface-specific lock state once migrated;
- preserve existing backend contracts and feedback semantics;
- avoid unrelated presentation changes, dependency upgrades, or data-model
  changes.

Completion requires one auditable owner for every transition from draft submit
through fresh authoritative reconciliation.
