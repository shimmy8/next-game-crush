# Profile Summary Length Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce usable Russian AI profile summaries despite harmless length variance while retaining strict evidence grounding.

**Architecture:** Keep the OpenAI schema unchanged. Normalize profile-specific auxiliary text after decoding, validate summary lengths in Unicode code points, and reuse the same normalization/validation path in the OpenAI client and background service boundary.

**Tech Stack:** Go 1.27.2, OpenAI Responses structured output, PostgreSQL-backed jobs.

**Spec:** `docs/superpowers/specs/2026-10-18-profile-summary-length-design.md`

## Global Constraints

- Use `GOWORK=off` for Go commands.
- Do not log or inspect raw prompts, model responses, job payloads, secrets, or personal data.
- Do not edit generated sqlc files or introduce a schema change.
- Do not run browser or Playwright tests.

## Review Focus

- Cyrillic input must be counted as Unicode code points, not UTF-8 bytes.
- Boundary values 320 and 720 must pass; 319 and 721 must fail.
- Empty, duplicate, and 145-character auxiliary entries must be removed without losing a valid summary.
- Auxiliary order must remain stable after normalization.
- Unknown or duplicate evidence citations must still fail.

---

### Task 1: Profile output contract

**Files:**
- Modify: `internal/ai/prompts.go`
- Modify: `internal/ai/prompts_test.go`
- Modify: `internal/ai/openai.go`
- Modify: `internal/ai/openai_test.go`

**Interfaces:**
- Produces: `normalizeAndValidateProfileOutput(output ProfileSummary, allowed map[string]struct{}) (ProfileSummary, error)`.
- Consumes: the existing `validateCitations` evidence contract.

- [x] **Step 1: Write failing boundary and normalization tests**

Add tests for Cyrillic summaries at 320/720 and outside those limits, plus stable trimming, empty removal, deduplication, and dropping auxiliary values over 144 characters. Retain a direct unknown-citation rejection assertion.

- [x] **Step 2: Run the focused tests and confirm they fail**

Run: `GOWORK=off go test ./internal/ai -run 'Test(ProfileSummary|ProfilePrompt)'`

- [x] **Step 3: Implement prompt v2 and profile-specific normalization**

Set `ProfilePromptVersion` to `profile-summary-v2`; request Russian summary length 400–600 and auxiliary item length at most 120; validate the trimmed summary at 320–720 Unicode code points; normalize auxiliary arrays with the 144-character tolerance; keep citation checks unchanged.

- [x] **Step 4: Reuse normalized output at both validation boundaries**

Return the normalized value from `OpenAI.SummarizeProfile` and save the normalized value from `Service.processProfileSummary` so alternate `Client` implementations receive the same validation.

- [x] **Step 5: Run focused race tests**

Run: `GOWORK=off go test -race ./internal/ai`
Expected: PASS.

### Task 2: Local recovery and verification

**Files:**
- No source files.

**Interfaces:**
- Consumes: rebuilt local app and the existing PostgreSQL job queue.
- Produces: a ready saved AI summary or a newly classified bounded failure.

- [x] **Step 1: Run affected and broad backend checks**

Sync CodeGraph, inspect affected tests, then run the full Go race suite against the isolated test database.

- [x] **Step 2: Rebuild the local app**

Run the existing Compose build/start command and verify the health endpoint.

- [x] **Step 3: Retry only the current dead profile-summary job**

Use a conditional SQL update scoped to `kind = 'ai.profile_summary'` and `status = 'dead'`; do not print payloads or user identifiers.

- [x] **Step 4: Verify aggregate recovery state**

Check the job status counts and whether a non-empty profile summary is saved. Do not print the generated summary.

