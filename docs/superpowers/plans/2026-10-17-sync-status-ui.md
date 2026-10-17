# Compact Synchronization Status UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep synchronization progress compact and accurately distinguish active, partially completed, failed, and unreachable status states.

**Architecture:** Derive a pure presentation model from the unchanged `SyncStatus` contract, grouping repeated per-game rows before rendering. Refactor `SyncProgress` so only its short summary is live, details scroll within a responsive maximum height, and the backend progress value remains authoritative.

**Tech Stack:** React 19, TypeScript 6, CSS, Vitest, Testing Library

**Spec:** `docs/superpowers/specs/2026-10-17-sync-status-ui-design.md`

## Global Constraints

- Do not change the backend sync status API or recalculate its progress value.
- Never render raw Steam AppIDs or per-game names in compact sync details.
- Keep unavailable optional sources secondary while the root workflow is active.
- Preserve the last successful snapshot when a polling request fails.
- Use exact Russian status copy from the design specification.
- Preserve current localization and the untracked `web/src/ui/labels.ts` helper.

## Review Focus

- One `catalog:<id>` or `achievements:<id>` row must still be sanitized even when it does not form a repeated group; pin this in Task 1.
- A request error with an existing snapshot must retain progress and details while changing only the summary; pin this in Task 2.
- Unknown root and child states must render `Статус уточняется` without throwing; pin this in Task 1.
- Duplicate kind/stage rows must not create duplicate React keys; pin grouped row identity in Task 1.
- The visible percentage and progress element must not be announced twice; pin this in Task 2.

---

### Task 1: Pure Status Precedence and Grouping Model

**Files:**
- Create: `web/src/components/syncProgressModel.ts`
- Create: `web/src/components/syncProgressModel.test.ts`

**Interfaces:**
- Produces: `type SyncViewState = 'request-failed' | 'active' | 'failed' | 'partial' | 'complete' | 'unknown'`.
- Produces: `type StageCounts = { total: number; completed: number; pending: number; failed: number; unknown: number }`.
- Produces: `type StageRow = { key: string; label: string; counts?: StageCounts; statusLabel?: string }`.
- Produces: `type SyncPresentation = { state: SyncViewState; headline: string; warning?: string; rows: StageRow[] }`.
- Produces: `deriveSyncPresentation(value: SyncStatus | null, requestError?: string): SyncPresentation` with exact headlines: request failure `Не удалось обновить статус`, active `Синхронизация идёт`, terminal `Синхронизация остановилась с ошибкой`, partial `Синхронизация завершена не полностью`, complete `Синхронизация завершена`, and unknown `Статус уточняется`.
- Produces: `groupSyncStages(stages: SyncStage[]): StageRow[]`.

- [ ] **Step 1: Write failing precedence tests**

Cover active plus unavailable source, retryable detail, terminal root, successful root with unavailable/request-failed/error child, clean success, request error, and unknown root. Assert the exact six primary messages and the active warning `Некоторые источники недоступны — продолжаем`.

- [ ] **Step 2: Write failing grouping/redaction tests**

Aggregate repeated `achievements`, `catalog`, `ai_features`, and `ai_embedding` stages into completed/pending/failed/unknown counts. Assert a single suffixed stage is sanitized, no input AppID reaches labels or keys, unknown kinds use `Другие этапы`, and repeated input cannot create duplicate row keys.

- [ ] **Step 3: Verify the model tests fail**

Run: `pnpm --dir web run test -- src/components/syncProgressModel.test.ts`

Expected: FAIL because the model is absent.

- [ ] **Step 4: Implement the pure model**

Use root status precedence in this order: request failure; active; terminal failure; successful with problems; clean success; unknown. Classify a stage as failed before completed/pending when it has `error`, `unavailable`, or `request_failed`. Strip suffixes before any label helper is called.

- [ ] **Step 5: Verify the model tests pass**

Run: `pnpm --dir web run test -- src/components/syncProgressModel.test.ts`

Expected: PASS.

- [ ] **Step 6: Commit the model**

```bash
git add web/src/components/syncProgressModel.ts web/src/components/syncProgressModel.test.ts
git commit -m "feat: derive compact sync status presentation"
```

### Task 2: Accessible Compact Progress Component

**Files:**
- Modify: `web/src/components/SyncProgress.tsx`
- Modify: `web/src/components/SyncProgress.test.tsx`
- Modify: `web/src/styles.css`

**Interfaces:**
- Consumes: `deriveSyncPresentation` from Task 1.
- Preserves: `SyncProgress({ value, requestError }: { value: SyncStatus | null; requestError?: string })`.

- [ ] **Step 1: Replace obsolete component expectations with failing spec tests**

Assert active root status wins over an unavailable source, partial and terminal copy are exact, retry remains detail, request failure retains the supplied snapshot, grouped counts render, and raw IDs are absent.

- [ ] **Step 2: Add failing accessibility assertions**

Assert exactly one short `role="status"` live region with `aria-atomic`, a named progressbar, a visible percentage hidden from accessibility, and a named focusable details region outside the live region.

- [ ] **Step 3: Verify focused component tests fail**

Run: `pnpm --dir web run test -- src/components/syncProgressModel.test.ts src/components/SyncProgress.test.tsx`

Expected: FAIL against the current precedence and markup.

- [ ] **Step 4: Refactor `SyncProgress` and add bounded styles**

Keep header/progress outside `.sync-progress__details`. Set desktop details to `max-block-size: min(18rem, 42vh)`, `overflow-y: auto`, and `overscroll-behavior: contain`; under the existing `700px` media query set `max-block-size: min(15rem, 38vh)`. Add a visible focus style without trapping focus.

- [ ] **Step 5: Verify all sync component and polling tests**

Run: `pnpm --dir web run test -- src/components/syncProgressModel.test.ts src/components/SyncProgress.test.tsx src/api/hooks.test.ts`

Expected: PASS.

- [ ] **Step 6: Commit the component**

```bash
git add web/src/components/SyncProgress.tsx web/src/components/SyncProgress.test.tsx web/src/styles.css
git commit -m "feat: compact synchronization progress details"
```

### Task 3: Frontend and Integration Verification

**Files:**
- No planned source changes; fix only regressions caused by Tasks 1-2.

**Interfaces:**
- Consumes: completed status model and component.

- [ ] **Step 1: Run the complete frontend suite**

Run: `pnpm --dir web run test`

Run: `pnpm --dir web run typecheck`

Run: `pnpm --dir web run lint`

Run: `pnpm --dir web run build`

Expected: all commands PASS.

- [ ] **Step 2: Refresh dependency evidence**

Run: `codegraph sync .`

Run: `codegraph affected web/src/components/SyncProgress.tsx web/src/components/SyncProgress.test.tsx web/src/components/syncProgressModel.ts web/src/components/syncProgressModel.test.ts web/src/styles.css -p .`

Expected: no untested affected consumer is reported.

- [ ] **Step 3: Run repository verification after backend tracks are integrated**

Run: `make verify`

Expected: all backend, frontend, build, Compose, and E2E gates PASS.

- [ ] **Step 4: Commit only if verification required a scoped fix**

Stage only files changed to repair a regression and use commit message `fix: integrate compact sync status UI`. If no source changed, do not create an empty commit.
