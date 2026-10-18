# Source Failure Diagnostics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make partial Steam request failures understandable in the UI and safely diagnosable from structured logs.

**Architecture:** The sync layer derives an allowlisted diagnostic from typed Steam errors and passes it to the observability boundary, which emits a bounded optional HTTP status. The frontend derives distinct warning copy directly from the existing `availability` contract, so no API or database migration is required.

**Tech Stack:** Go 1.27.2, slog, React, TypeScript, Vitest.

**Spec:** `docs/superpowers/specs/2026-10-17-source-failure-diagnostics-design.md`

## Global Constraints

- Do not log or expose SteamID, AppID, URLs, API keys, raw errors, payloads, or response bodies.
- Do not change the database schema or public sync-status JSON contract.
- Preserve best-effort achievements synchronization and last-known-good data.
- Use `GOWORK=off` for direct Go commands.

## Review Focus

- A non-retryable Steam 4xx must not collapse into `internal_error`.
- HTTP status logging must reject values outside `400..599`.
- One failed achievement must not reveal which game failed.
- Mixed unavailable and request-failed states must use deterministic copy.
- Historical source states must remain safely displayable without new fields.

---

### Task 1: Safe Steam Failure Classification

**Files:**
- Modify: `internal/observability/events.go`
- Modify: `internal/observability/events_test.go`
- Modify: `internal/sync/service.go`
- Modify: `internal/sync/service_test.go`

**Interfaces:**
- Produces: a bounded Steam diagnostic containing `observability.Reason` and optional upstream HTTP status.
- Produces: `SyncSourceEvent.UpstreamStatus int` serialized only for safe HTTP error statuses.

- [x] Add failing tests for timeout, 429, 4xx rejection, 5xx unavailability, malformed response, unknown errors, and bounded status serialization.
- [x] Run focused tests and confirm the new expectations fail.
- [x] Implement the minimal typed-error classification and aggregate achievement diagnostic propagation.
- [x] Run `GOWORK=off go test -race ./internal/observability ./internal/sync` against the isolated test database.

### Task 2: Accurate Partial-source Copy

**Files:**
- Modify: `web/src/components/syncProgressModel.ts`
- Modify: `web/src/components/syncProgressModel.test.ts`
- Modify: `web/src/components/SyncProgress.test.tsx` only if integration coverage requires it.

**Interfaces:**
- Consumes: existing `SyncStage.availability` values.
- Produces: deterministic warning copy from `deriveSyncPresentation`.

- [x] Add failing tests for unavailable-only, request-failed-only, mixed, and healthy active states.
- [x] Run the focused Vitest files and confirm the new expectations fail.
- [x] Implement unavailable-first warning selection with distinct failed-request copy.
- [x] Run the focused tests and the complete frontend test/typecheck/lint/build suite.

### Task 3: Integration Verification

**Files:**
- Modify: `docs/superpowers/plans/2026-10-17-source-failure-diagnostics.md` to mark completed steps.

- [x] Refresh CodeGraph and inspect affected coverage.
- [x] Run focused backend race tests with the isolated test database.
- [x] Run the complete frontend verification suite.
- [x] Run `make verify` with the documented system-Chrome fallback.
- [x] Run `git diff --check`.

