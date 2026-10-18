# Sync Resume and Stable Progress Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore active synchronization after a page reload and expose stable, meaningful workflow progress.

**Architecture:** The backend discovers the authenticated user's active root job with the existing database query and builds its status through the existing ownership-checked path. Progress is derived from fixed orchestration phase weights, while the frontend performs one mount-time discovery and resumes the existing abortable polling loop.

**Tech Stack:** Go 1.27.2, chi HTTP API, PostgreSQL/sqlc, React, TypeScript, Vitest.

**Spec:** `docs/superpowers/specs/2026-10-17-sync-resume-progress-design.md`

## Global Constraints

- Keep the product local-first and single-user.
- Do not add a database migration or another datastore.
- Do not expose SteamID values, job payloads, secrets, or raw upstream responses.
- Preserve authenticated ownership checks and existing CSRF behavior; the new read endpoint does not require CSRF.
- Preserve the current compact status UI, grouped details, and sanitized errors.
- Use Go 1.27.2 with `GOWORK=off` for direct Go commands.
- Do not edit `internal/store/sqlc/` manually.

## Review Focus

- A refresh while a job is active must resume that job without creating a second job or rerunning preflight.
- A refresh with no active job must not display stale progress or an error.
- A job completing between discovery and the first poll must still refresh dashboard data once.
- Atomic AI batch publication and later profile/recommendation child creation must not make the percentage decrease.
- Unmount or a manual restart during discovery must cancel the stale request and prevent competing pollers.

---

### Task 1: Stable Backend Progress

**Files:**
- Modify: `internal/sync/service.go`
- Test: `internal/sync/service_test.go`

**Interfaces:**
- Consumes: existing `dbgen.SyncJob` root and direct child rows returned by `GetJobChildren`.
- Produces: `workflowProgress(root dbgen.SyncJob, children []dbgen.SyncJob) int` used by `Service.GetStatus`.

- [x] **Step 1: Write failing table-driven tests for fixed progress milestones**

Cover initial active `0`, Steam import `20`, catalog `40`, achievements `60`, fractional AI progress between `60` and `80`, profile `90`, recommendations/root success `100`, atomic AI batch publication, and later child creation that never lowers progress.

- [x] **Step 2: Run the focused tests and verify they fail**

Run: `GOWORK=off go test ./internal/sync -run 'TestWorkflowProgress|TestSyncStatusProgress'`

Expected: FAIL because fixed phase progress is not implemented.

- [x] **Step 3: Implement `workflowProgress(root dbgen.SyncJob, children []dbgen.SyncJob) int`**

Use the exact weights and completion signals from the spec. Clamp the result to `0..100`; return `100` for a succeeded root. Replace the current dynamic `completed / total` calculation in `GetStatus`.

- [x] **Step 4: Run sync package race tests**

Run: `GOWORK=off go test -race ./internal/sync`

Expected: PASS with zero failures.

### Task 2: Active Sync Discovery API

**Files:**
- Modify: `internal/sync/service.go`
- Modify: `internal/httpapi/sync_handlers.go`
- Modify: `internal/httpapi/router.go`
- Test: `internal/sync/service_test.go`
- Test: `internal/httpapi/sync_handlers_test.go`

**Interfaces:**
- Consumes: generated `FindActiveUserSync(ctx context.Context, userID string) (dbgen.SyncJob, error)` and `Service.GetStatus(ctx, userID, jobID)`.
- Produces: `Service.GetCurrentStatus(ctx context.Context, userID uuid.UUID) (Status, bool, error)` and authenticated `GET /api/sync/current`.

- [x] **Step 1: Write failing service tests for active lookup and ownership-safe status construction**

Assert an active root returns its full status, no row returns `(Status{}, false, nil)`, and a mismatched payload cannot expose another user's status.

- [x] **Step 2: Write failing handler tests for the HTTP contract**

Assert authenticated active discovery returns `200` plus `SyncStatus`, no active job returns `204` with an empty body, and an unauthenticated request is rejected by existing middleware.

- [x] **Step 3: Run the focused tests and verify they fail**

Run: `GOWORK=off go test ./internal/sync ./internal/httpapi -run 'Test.*CurrentSync|Test.*CurrentStatus'`

Expected: FAIL because the service operation and route do not exist.

- [x] **Step 4: Implement service lookup, handler, and route**

Add `GetCurrentStatus` to the HTTP service interface, use `FindActiveUserSync`, delegate response construction to `GetStatus`, and register `/api/sync/current` before `/api/sync/{id}`.

- [x] **Step 5: Run backend race tests**

Run: `GOWORK=off go test -race ./internal/sync ./internal/httpapi`

Expected: PASS with zero failures.

### Task 3: Reload Recovery and Polling

**Files:**
- Modify: `web/src/api/client.ts`
- Modify: `web/src/api/hooks.ts`
- Test: `web/src/api/client.test.ts`
- Test: `web/src/api/hooks.test.ts`

**Interfaces:**
- Consumes: `GET /api/sync/current` from Task 2 and existing `pollSyncJob(jobId, options)`.
- Produces: `getCurrentSync(signal?: AbortSignal): Promise<SyncStatus | null>` and mount-time recovery inside `useSync`.

- [x] **Step 1: Write failing client contract tests**

Assert `200` decodes `SyncStatus`, `204` returns `null`, credentials remain same-origin, and other failures reject without leaking response contents.

- [x] **Step 2: Write failing hook tests for reload recovery and lifecycle races**

Assert active discovery renders and polls the recovered ID, no active job stays quiet, recovered success calls `onComplete` once, unmount aborts recovery, and a manual run cancels recovery before preflight/start.

- [x] **Step 3: Run frontend focused tests and verify they fail**

Run: `pnpm --dir web run test -- src/api/client.test.ts src/api/hooks.test.ts`

Expected: FAIL because recovery is not implemented.

- [x] **Step 4: Implement `getCurrentSync` and one-owner polling lifecycle**

Use one `AbortController` at a time. On mount, discover and set the current status before calling `pollSyncJob`; manual `run` aborts that controller before beginning preflight. Reuse one completion helper so recovered and manually started jobs reload dashboard data identically.

- [x] **Step 5: Run focused frontend tests**

Run: `pnpm --dir web run test -- src/api/client.test.ts src/api/hooks.test.ts`

Expected: PASS with zero failures.

### Task 4: Indeterminate Initial Progress

**Files:**
- Modify: `web/src/components/SyncProgress.tsx`
- Test: `web/src/components/SyncProgress.test.tsx`

**Interfaces:**
- Consumes: existing `SyncStatus.progress` and active-state derivation.
- Produces: an accessible indeterminate `<progress>` while active progress is zero, and the existing determinate form after progress becomes measurable.

- [x] **Step 1: Write failing component tests**

Assert an active status at `0` omits the progress element's `value` attribute and hides `0%`; active progress above zero and all terminal statuses retain a clamped determinate value.

- [x] **Step 2: Run the component test and verify it fails**

Run: `pnpm --dir web run test -- src/components/SyncProgress.test.tsx`

Expected: FAIL because zero is currently rendered as determinate.

- [x] **Step 3: Implement indeterminate rendering**

Derive `indeterminate` only when the synchronization presentation is active and clamped progress is zero. Keep the Russian accessible label and existing visual states.

- [x] **Step 4: Run the component test**

Run: `pnpm --dir web run test -- src/components/SyncProgress.test.tsx`

Expected: PASS with zero failures.

### Task 5: Cross-Layer Verification

**Files:**
- Modify: `docs/superpowers/plans/2026-10-17-sync-resume-progress.md` (check completed steps)

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: fresh verification evidence for the integrated behavior.

- [x] **Step 1: Refresh CodeGraph and inspect affected tests**

Run: `codegraph sync .` then `codegraph affected internal/sync/service.go internal/httpapi/sync_handlers.go internal/httpapi/router.go web/src/api/client.ts web/src/api/hooks.ts web/src/components/SyncProgress.tsx -p .`

Expected: index is current and the reported affected areas are covered by the focused suites.

- [x] **Step 2: Run backend verification**

Run: `GOWORK=off go test -race ./internal/sync ./internal/httpapi`

Expected: PASS with zero failures.

- [x] **Step 3: Run frontend verification**

Run: `pnpm --dir web run test && pnpm --dir web run typecheck && pnpm --dir web run lint && pnpm --dir web run build`

Expected: all commands exit zero.

- [x] **Step 4: Validate the artifact and patch formatting**

Run: `git diff --check`

Expected: no output and exit zero.

