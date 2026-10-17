# Synchronization Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce structured, correlated, privacy-safe lifecycle logs for sync requests, background jobs, Steam sources, and worker infrastructure.

**Architecture:** Wrap the application's single JSON `slog` sink in a typed observability boundary whose event methods enforce field and value allowlists. Extend queue leases with root-workflow correlation and typed failure disposition, instrument the worker with an independent heartbeat interval, and add aggregate source spans in the sync service without logging per-game data.

**Tech Stack:** Go 1.27.2, `log/slog`, pgx/sqlc, PostgreSQL job queue

**Spec:** `docs/superpowers/specs/2026-10-17-sync-observability-design.md`

## Global Constraints

- Use one JSON sink created by application composition; a nil typed logger becomes a discard logger in tests.
- Emit only strict allowlisted event names, field keys, job kinds, stages, sources, outcomes, and reasons.
- Never log user/Steam/game identifiers, game or profile names, avatar/callback URLs, payloads, idempotency keys, worker IDs, fencing tokens, secrets, prompts, model output, or raw errors.
- Use aggregate achievement counts only; never emit one source event per game.
- Keep heartbeat scheduling independent from lease renewal and rate-limit it per attempt.
- Preserve current queue semantics, source-state persistence, and last-known-good upstream data.
- Execute HTTP/sync/app wiring only after the privacy/profile backend tasks because those tracks share files.

## Review Focus

- A nested embedding job must carry the root workflow ID rather than its immediate feature parent as workflow correlation; pin this in Task 2.
- A malicious unknown job kind, stage, source, or reason must become a bounded fallback rather than pass through; pin this in Task 1.
- Canary data placed in payload, idempotency key, worker identity, fencing token, and errors must be absent from complete JSON output; pin this in Tasks 1, 3, and 5.
- A short job must not emit a heartbeat, while a long job emits bounded heartbeats without renewal-driven flooding; pin this in Task 3.
- Final failure severity must distinguish required (`ERROR`) from optional (`WARN`) work; pin this in Task 3.

---

### Task 1: Typed Safe Event Boundary

**Files:**
- Create: `internal/observability/events.go`
- Create: `internal/observability/events_test.go`

**Interfaces:**
- Produces: `type Logger struct` and `func New(*slog.Logger) *Logger` with nil-to-discard behavior.
- Produces: `type Correlation struct { WorkflowID uuid.UUID; JobID uuid.UUID; ParentJobID uuid.UUID }`, `WithCorrelation`, and `CorrelationFromContext`.
- Produces: `SyncRequestEvent { Outcome; RequestID; WorkflowID; HTTPStatus; Duration; Reason }`.
- Produces: `JobAttemptEvent { Stage; Correlation; Kind; LogicalStage; Attempt; MaxAttempts; Required; Duration; RetryDelay; Reason }`.
- Produces: `SyncSourceEvent { Stage; Correlation; Source; Availability; Reason; Duration; TotalCount; AvailableCount; UnavailableCount; FailedCount }`.
- Produces: `WorkerEvent { Stage; Correlation; Reason }` plus `Logger.SyncRequest`, `Logger.JobAttempt`, `Logger.SyncSource`, and `Logger.Worker`.
- Produces: request outcomes `accepted|failed`; job-attempt stages `started|heartbeat|deferred|succeeded|retry_scheduled|dead|cancelled|lease_lost`; source stages `started|finished`; worker stages `started|stopped|claim_failed|completion_failed|failure_record_failed|renewal_failed`.
- Produces: `Reason` allowlist: empty success, `timeout`, `rate_limited`, `upstream_unavailable`, `malformed_response`, `lease_lost`, `persistence_failed`, and `internal_error`.

- [ ] **Step 1: Write failing allowlist and severity tests**

Capture JSON and assert each event contains only its declared fields; unknown event values become `unknown` or `internal_error`; durations and retry delays are clamped to `0..24h`; levels match INFO/WARN/ERROR rules; nil logging does not panic.

- [ ] **Step 2: Write the correlation and full-canary tests**

Assert context round-trip and prove canaries supplied through every accepted string field cannot appear unless that exact field value belongs to its allowlist. The public event structs must have no `error`, payload, or arbitrary `slog.Attr` field.

- [ ] **Step 3: Verify tests fail**

Run: `GOWORK=off go test -race ./internal/observability`

Expected: FAIL because the package does not exist.

- [ ] **Step 4: Implement the typed logger**

Allow production job kinds `steam.sync`, `catalog.refresh`, `achievements.sync`, `ai.features`, `ai.embedding`, `ai.profile_summary`, `profile.rebuild`, `recommendations.rebuild`, `feedback.classify`, and `notifications.digest.send`. Allow logical stages `steam`, `catalog`, `achievements`, `ai_features`, `ai_embedding`, `profile_summary`, `profile`, `recommendations`, `feedback`, and `digest`; sources are `owned_games`, `recently_played`, `wishlist`, and `achievements`. Select level inside each method rather than accepting it from callers.

- [ ] **Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./internal/observability`

Expected: PASS.

```bash
git add internal/observability/events.go internal/observability/events_test.go
git commit -m "feat: add safe synchronization event logger"
```

### Task 2: Queue Failure Outcome and Root Correlation

**Files:**
- Modify: `db/queries/jobs.sql`
- Regenerate: `internal/store/sqlc/jobs.sql.go`
- Modify: `internal/jobs/queue.go`
- Modify: `internal/jobs/queue_test.go`

**Interfaces:**
- Produces: `type FailureOutcome string` with `FailureRetryScheduled` and `FailureDead`.
- Changes: `Queue.Fail(context.Context, Lease, error, time.Time) (FailureOutcome, error)`.
- Extends: `Lease` with `ParentJobID uuid.UUID`, `WorkflowID uuid.UUID`, `Required bool`, and `Stage string`.
- Produces: sqlc query `GetJobWorkflowID(context.Context, pgtype.UUID) (pgtype.UUID, error)` using a recursive ancestor walk to the root job.

- [ ] **Step 1: Write failing failure-disposition tests**

Assert first failure returns retry scheduled, final attempt returns dead, and stale ownership returns an empty outcome plus `ErrLeaseLost`.

- [ ] **Step 2: Write failing lease-correlation tests**

Assert root `WorkflowID == ID`, direct child and nested grandchild use the same root workflow ID, and claimed parent/required/stage metadata matches the stored job.

- [ ] **Step 3: Verify focused tests fail**

Run: `GOWORK=off go test -race ./internal/jobs -run 'Test.*(Failure|Workflow|Lease)'`

Expected: FAIL because the outcome and lease fields are absent.

- [ ] **Step 4: Add the recursive query and regenerate sqlc**

Run: `make sqlc`

Expected: generated `jobs.sql.go` contains `GetJobWorkflowID`; no generated file is edited manually.

- [ ] **Step 5: Implement queue propagation and update existing callers**

Populate correlation during claim and return the disposition already computed by `Fail`. Preserve retry timing, dead-letter propagation, and fencing behavior exactly.

- [ ] **Step 6: Verify the jobs package**

Run: `GOWORK=off go test -race ./internal/jobs`

Expected: PASS.

- [ ] **Step 7: Commit queue metadata**

```bash
git add db/queries/jobs.sql internal/store/sqlc/jobs.sql.go internal/jobs/queue.go internal/jobs/queue_test.go
git commit -m "feat: correlate job attempts with sync workflows"
```

### Task 3: Worker Lifecycle and Heartbeats

**Files:**
- Modify: `internal/jobs/worker.go`
- Modify: `internal/jobs/worker_test.go`

**Interfaces:**
- Consumes: typed observability logger and expanded `Lease` from Tasks 1-2.
- Produces: `WithLogger(*observability.Logger) WorkerOption`.
- Produces: `WithHeartbeatInterval(time.Duration) WorkerOption`, defaulting to 15 seconds with the first heartbeat only after a full interval.
- Produces: private `workerQueue` interface and `newWorker(workerQueue, string, ...WorkerOption) *Worker`; public `NewWorker(*Queue, string, ...WorkerOption)` remains compatible.

- [ ] **Step 1: Add a fake queue and failing lifecycle tests**

Cover `started -> succeeded`, deferred without failure, retry scheduled with delay, final required dead, final optional dead, claim/renew/complete/failure-record infrastructure errors, lease loss, cancellation, and worker start/stop.

- [ ] **Step 2: Add failing heartbeat and log-canary tests**

Use a short configurable heartbeat interval to prove long work emits heartbeat and short work does not. Put canaries in handler errors, payload, idempotency key, worker ID, and fencing token and assert none appear in captured JSON.

- [ ] **Step 3: Verify worker tests fail**

Run: `GOWORK=off go test -race ./internal/jobs -run 'TestWorker.*(Log|Heartbeat|Lifecycle|Infrastructure|Canary)'`

Expected: FAIL because worker instrumentation is absent.

- [ ] **Step 4: Implement lifecycle instrumentation**

Attach lease correlation to the handler context, use separate renewal and heartbeat timers, log `ErrDeferred` as deferred, classify lease loss explicitly, and log persistence failures without passing error text.

- [ ] **Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./internal/jobs`

Expected: PASS.

```bash
git add internal/jobs/worker.go internal/jobs/worker_test.go
git commit -m "feat: log job lifecycle and heartbeats"
```

### Task 4: HTTP Sync Request Events

**Files:**
- Modify: `internal/httpapi/sync_handlers.go`
- Modify: `internal/httpapi/sync_handlers_test.go`
- Modify: `internal/httpapi/router.go`

**Interfaces:**
- Consumes: `requestIDFromContext` and `*observability.Logger`.
- Changes: `startSync(service SyncService, logger *observability.Logger) http.HandlerFunc`.
- Adds: `Observability *observability.Logger` to `httpapi.Dependencies` while preserving the existing raw `Logger *slog.Logger` for authentication events.

- [ ] **Step 1: Write failing accepted/failed request log tests**

Assert accepted includes generated request ID, returned workflow ID, HTTP 202, and bounded duration. Assert failure includes safe HTTP status/reason, excludes workflow ID and raw service error, and ignores a hostile incoming `X-Request-ID`.

- [ ] **Step 2: Add strict field and middleware tests**

Assert the event field allowlist and prove authentication/CSRF rejection cannot create a false accepted event.

- [ ] **Step 3: Verify focused tests fail**

Run: `GOWORK=off go test -race ./internal/httpapi -run 'TestSync.*Log|TestSyncRequest'`

Expected: FAIL because request events are not emitted.

- [ ] **Step 4: Implement handler instrumentation on the post-preflight code**

Re-read the current handler signature after the privacy/profile track. Measure only the handler's start operation, map errors to bounded reasons, and keep response bodies unchanged and sanitized.

- [ ] **Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./internal/httpapi`

Expected: PASS.

```bash
git add internal/httpapi/sync_handlers.go internal/httpapi/sync_handlers_test.go internal/httpapi/router.go
git commit -m "feat: log synchronization requests"
```

### Task 5: Steam Source Spans and Achievement Aggregates

**Files:**
- Modify: `internal/sync/service.go`
- Modify: `internal/sync/service_test.go`

**Interfaces:**
- Consumes: correlation context and typed logger from Task 1.
- Produces: `type ServiceOption func(*Service)` and `WithLogger(*observability.Logger) ServiceOption`.
- Changes: `NewService(..., options ...ServiceOption)` remains source-compatible for existing callers.
- Produces: package-local `steamReason(steam.Availability, error) observability.Reason`.

- [ ] **Step 1: Write failing source-span tests**

Assert owned games, recently played, and wishlist each emit exactly one started and one finished event with bounded availability/reason/duration for success, unavailable, timeout, rate limit, malformed response, and unknown failure.

- [ ] **Step 2: Write failing persistence and achievement tests**

Assert persistence failure maps to `persistence_failed`; achievements emit only one started and one finished event with target/available/unavailable/failed counts regardless of game count.

- [ ] **Step 3: Add a complete source log-canary test**

Place canaries in SteamID, user UUID, AppID, game/display names, avatar, and typed/raw errors; assert none appear while last-known-good state remains preserved after upstream failure.

- [ ] **Step 4: Verify focused tests fail**

Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/sync -run 'Test.*(SourceLog|AchievementLog|LogCanary)'`

Expected: FAIL because source instrumentation is absent.

- [ ] **Step 5: Implement aggregate source instrumentation**

Log around existing import boundaries without changing save/delete behavior. Use typed Steam errors and call-site knowledge for safe reasons; never pass an error object or per-game metadata to the logger.

- [ ] **Step 6: Verify and commit**

Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/sync`

Expected: PASS.

```bash
git add internal/sync/service.go internal/sync/service_test.go
git commit -m "feat: log Steam synchronization sources"
```

### Task 6: Composition, Runbook, and Full Verification

**Files:**
- Modify: `internal/app/app.go`
- Modify: `docs/local-demo.md`

**Interfaces:**
- Consumes: logger options and HTTP dependency from Tasks 3-5.
- Produces: one raw JSON sink and one typed wrapper shared by HTTP, worker, and sync service.

- [ ] **Step 1: Wire the shared logger in application composition**

Create the sink once, retain it for existing auth logs, wrap it once for typed events, and inject the same wrapper into the HTTP dependencies, worker, and sync service.

- [ ] **Step 2: Add the safe local runbook section**

Document `docker compose logs -f app`, the `sync_request`, `job_attempt`, and `sync_source` events, how to follow correlation IDs, and the rule not to paste secrets or raw upstream data into diagnostics.

- [ ] **Step 3: Verify documentation and dependency impact**

Run: `git diff --check`

Run: `codegraph sync .`

Run: `codegraph affected internal/observability/events.go internal/jobs/queue.go internal/jobs/worker.go internal/httpapi/sync_handlers.go internal/sync/service.go internal/app/app.go -p .`

Expected: no formatting errors and all affected packages are represented in the tests above.

- [ ] **Step 4: Run full verification**

Run: `make verify`

Expected: sqlc diff is empty and all Go race tests, frontend checks, Compose/image build, and E2E tests PASS.

- [ ] **Step 5: Commit composition and runbook**

```bash
git add internal/app/app.go docs/local-demo.md
git commit -m "docs: document synchronization diagnostics"
```
