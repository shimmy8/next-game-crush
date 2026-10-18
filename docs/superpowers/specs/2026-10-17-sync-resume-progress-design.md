# Sync Resume and Stable Progress Design

## Goal

Keep an in-flight Steam synchronization visible after a browser reload and report progress that advances monotonically through the existing workflow.

## Scope

- Restore only the authenticated user's active root `steam.sync` job.
- Resume status polling without starting another synchronization or repeating the Steam privacy preflight.
- Replace the dynamic direct-child percentage with fixed workflow phase weights.
- Show an indeterminate progress bar while the first measurable phase is still running.
- Keep the existing compact status panel, grouped stage details, degraded-source warnings, and sanitized errors.

No migration, new datastore, multi-user behavior, or persisted browser state is required.

## API Contract

Add authenticated `GET /api/sync/current`.

- `200 OK` with the existing `SyncStatus` JSON contract when the current user has an active root `steam.sync` job.
- `204 No Content` when no active synchronization exists.
- The handler obtains the user exclusively from the authenticated session. It never accepts or returns a SteamID, job payload, or another user's job.
- Existing `GET /api/sync/{id}` remains the polling endpoint after discovery.

The sync service exposes a current-status operation backed by the existing `FindActiveUserSync` query. Ownership is checked again through the same status-building path used by `GET /api/sync/{id}`.

## Progress Model

Progress uses stable weights whose sum is 100:

| Phase | Weight | Completion signal |
| --- | ---: | --- |
| Steam import | 20 | Initial `catalog` and `achievements` children have been created |
| Catalog | 20 | Direct `catalog` stage has succeeded |
| Achievements | 20 | Direct `achievements` stage has succeeded |
| AI features and embeddings | 20 | Fraction of direct `ai_features` jobs that have reached a terminal state; downstream `profile` creation implies the phase is complete |
| Profile | 10 | Direct `profile` stage has succeeded; downstream `recommendations` creation implies completion |
| Recommendations | 10 | Direct `recommendations` stage has succeeded |

Additional rules:

- A succeeded root job always reports `100`.
- Failed or dead stages do not masquerade as successful progress; the terminal error presentation remains authoritative.
- `AwaitChildren` publishes the complete planned `ai_features` batch atomically before any AI child can run, so the AI denominator is stable while its fraction advances. Later `profile` and `recommendations` creation never decreases active progress.
- Before Steam import has a measurable completion signal, the API may report `0`; the UI renders this as indeterminate and hides a misleading numeric `0%`.
- The algorithm operates on the root and its direct orchestration children. An `ai_features` job completes only after its required embedding child, so its terminal state represents both operations.

## Browser Lifecycle

On mounting the dashboard, `useSync` requests `/api/sync/current` once.

- If it receives an active status, it renders that status immediately and resumes the existing polling loop using the returned job ID.
- If it receives `204`, the dashboard remains unchanged.
- Discovery failure shows the existing safe connection error and does not start a job.
- Manual synchronization cancels discovery or previous polling before preflight and start.
- Component unmount aborts discovery and polling.
- A recovered job that reaches success triggers the existing dashboard data reload exactly as a manually started job does.

## Verification

- Service tests pin each weighted milestone, fractional AI progress, monotonic phase discovery, success at 100, and ownership isolation.
- Handler tests pin authenticated `200`, no-active-job `204`, and unauthenticated rejection.
- Hook tests pin reload recovery, no-job behavior, resumed polling, completion refresh, cancellation, and manual-run race prevention.
- Component tests pin indeterminate active rendering at zero and determinate rendering after measurable progress.
- Focused Go race tests and the full frontend test/typecheck/lint/build suite must pass.

