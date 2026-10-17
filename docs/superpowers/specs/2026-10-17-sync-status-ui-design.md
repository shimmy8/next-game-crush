# Compact Synchronization Status UI Design

**Date:** 2026-10-17
**Status:** Ready for user review
**Related design:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`

## 1. Objective

Make synchronization progress understandable without allowing the status card
to dominate the page. The interface must distinguish the state of the overall
workflow from warnings about optional sources and must remain useful when the
workflow contains many per-game catalog or achievement jobs.

This change uses the existing sync status API. It does not add a backend
contract or change queue semantics.

## 2. Layout

Keep the workflow summary, primary status, and progress bar fixed at the top of
the card. Put detailed stages in a separately named scroll region with:

```css
max-block-size: min(18rem, 42vh);
overflow-y: auto;
overscroll-behavior: contain;
```

On narrow screens, use the smaller bound `min(15rem, 38vh)`. The card itself
remains responsive and must not set a fixed pixel height.

Group repeated per-game work rather than listing every raw job:

- achievements: total, completed, waiting/retrying, and failed counts;
- catalog refresh: total, completed, waiting/retrying, and failed counts;
- AI feature and embedding work: grouped stage counts when repeated.

Do not show raw Steam AppIDs or game names in the compact status details.

## 3. Status Precedence and Copy

The primary message is derived from the root workflow state, not from the first
unavailable source.

Apply this precedence:

1. A status request failure shows `Не удалось обновить статус` while retaining
   the last successful snapshot.
2. Root `queued`, `running`, or `waiting` shows `Синхронизация идёт`.
3. A retryable stage shows `Ожидает повтора` as stage detail, without replacing
   the active workflow message.
4. Root `dead` or terminal failure shows `Синхронизация остановилась с ошибкой`.
5. A successful root workflow with unavailable or failed optional stages shows
   `Синхронизация завершена не полностью`.
6. A clean successful workflow shows `Синхронизация завершена`.

While work is active, unavailable optional sources appear as a secondary
warning: `Некоторые источники недоступны — продолжаем`. A source warning must
not imply that the worker is stuck.

## 4. Progress Semantics

The current API remains the source of truth. The client may aggregate repeated
rows for display but must not invent completed work. Progress is monotonic for
a single displayed snapshot sequence unless a newly discovered child job
increases the known total; in that case the status text remains authoritative.

Waiting and retrying are active non-terminal states. A terminal failure is
shown only when the workflow or required stage is terminal according to the
API. Optional-stage failures lead to partial completion rather than total
failure.

## 5. Accessibility

Use one short polite live region for the primary summary only. Do not place the
entire changing detail list inside a live region.

The progress element has an accessible name and exposes its value once. The
scrolling details region has a visible or programmatic label. Keyboard focus
must not be trapped in the region, and focus indicators remain visible.

Color is supplemental: active, warning, retry, failure, and success states also
use text labels or icons with accessible names.

## 6. Resilience

Polling errors preserve the last successful status snapshot and add a bounded
request-error message. They must not clear the progress display or falsely mark
the workflow failed.

Unknown future job kinds render under a generic `Другие этапы` group. Unknown
states render safely as `Статус уточняется` rather than causing the component to
fail.

## 7. Verification and Acceptance Criteria

Component tests must cover:

- active workflow plus unavailable wishlist keeps the active primary status;
- optional failures after root success produce partial completion;
- required terminal failure produces the stopped message;
- polling failure retains the previous snapshot;
- repeated achievement, catalog, and AI rows are grouped;
- raw AppIDs are absent from compact output;
- the details region has the maximum height and scroll behavior;
- the live region, progress label, and details label are accessible;
- narrow-screen layout keeps the summary visible and the details bounded.

The feature is accepted when a long real synchronization remains compact, the
user can tell whether work is active or terminal, and an unavailable optional
source cannot replace the overall progress message.
