# AI Feature Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Recover AI feature extraction by constraining citations at generation time, clearing stale retry errors, and logging bounded AI failure reasons.

**Architecture:** The OpenAI adapter derives a request-specific JSON Schema from normalized evidence while retaining runtime validation. The PostgreSQL queue clears presentation error state at claim time, and the generic worker consumes an allowlisted safe-reason interface implemented by AI sentinel errors.

**Tech Stack:** Go 1.27.2, OpenAI Responses structured outputs, PostgreSQL/sqlc, slog.

**Spec:** `docs/superpowers/specs/2026-10-17-ai-feature-recovery-design.md`

## Global Constraints

- Never expose evidence values or keys, prompts, job payloads, AppID, SteamID, API keys, URLs, or raw OpenAI responses in logs or UI.
- Keep runtime citation validation as defense in depth.
- Preserve valid cached features and embeddings; do not bump prompt or schema versions.
- Do not add a database migration.
- Change `db/queries/jobs.sql` and regenerate sqlc; never edit generated bindings manually.

## Review Focus

- Empty, duplicate, or unknown citations must still be rejected even with the schema constraint.
- Dynamic enum generation must be deterministic and contain keys, not evidence values.
- Clearing `last_error` must not erase `job_attempts` history.
- A second failure after reclaim must store the new error.
- Arbitrary errors must not inject unapproved reason strings into logs.

---

### Task 1: Request-specific Evidence Schema and Safe AI Reasons

**Files:**
- Modify: `internal/ai/openai.go`
- Modify: `internal/ai/openai_test.go`
- Modify: `internal/ai/types.go`
- Modify: `internal/jobs/worker.go`
- Modify: `internal/jobs/worker_test.go`
- Modify: `internal/observability/events.go`
- Modify: `internal/observability/events_test.go`

**Interfaces:**
- Produces: `gameFeaturesSchema(allowedKeys []string) map[string]any`.
- Produces: optional `SafeReason() string` on classified AI sentinel errors.
- Consumes: the safe-reason interface in `jobs.failureReason(error)`.

- [x] Add failing tests for exact enum emission, supported array bounds, value non-disclosure, safe reason propagation, and hostile fallback.
- [x] Run focused non-database tests and confirm the new expectations fail.
- [x] Implement the dynamic schema and allowlisted AI reason mapping without removing runtime duplicate/unknown-citation validation.
- [x] Run `GOWORK=off go test -race ./internal/ai ./internal/jobs ./internal/observability` with the isolated test database where required.

### Task 2: Clear Stale Job Errors on Claim

**Files:**
- Modify: `db/queries/jobs.sql`
- Regenerate: `internal/store/sqlc/jobs.sql.go`
- Modify: `internal/jobs/queue_test.go`

**Interfaces:**
- Produces: `ClaimJob` that atomically sets `last_error = NULL` for a fresh attempt.

- [x] Add a failing integration test that fails, reclaims, verifies current error clearing, then fails again and verifies replacement.
- [x] Run the focused queue test and confirm it fails before the query change.
- [x] Update `ClaimJob`, run `make sqlc`, and inspect the generated diff.
- [x] Run `GOWORK=off go test -race ./internal/jobs` with the isolated test database.

### Task 3: Integration and Live Recovery

**Files:**
- Modify: `docs/superpowers/plans/2026-10-17-ai-feature-recovery.md` to mark completed steps.

- [x] Refresh CodeGraph and inspect affected tests.
- [x] Run focused Go race tests for AI, jobs, observability, sync, and HTTP status presentation.
- [x] Run `make verify` with system Chrome.
- [x] Rebuild and restart only the local `app` service.
- [x] Confirm health `200` and inspect aggregate AI job categories without reading payloads or raw errors.
- [x] Run `git diff --check`.

Execution note: the first Docker build was interrupted by a temporary Docker Hub DNS failure. After connectivity recovered, the normal image build and full `make verify` both passed. The current workflow's pre-fix `invalid_evidence` presentation state was cleared only for already-waiting feature jobs; `job_attempts` history was retained.

