# Profile Summary Inline Evidence Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent internal `signal-*` evidence keys and citation ranges from appearing in reader-facing Activity Profile text while preserving strict structured citation validation.

**Architecture:** Keep the existing structured `evidence_keys` contract as the source of citation truth. Add a small profile-output sanitizer at the OpenAI boundary that removes citation-only bracketed or parenthesized groups when every referenced key is supplied, including bounded numeric ranges such as `signal-030–signal-033`; reject unknown, mixed, or ungrouped inline signal keys instead of silently accepting them. Tighten and version the prompt so new generations place opaque keys only in `evidence_keys`.

**Tech Stack:** Go 1.27.2, standard library regular expressions and UTF-8 handling, existing `internal/ai` tests.

**Spec:** `docs/superpowers/specs/2026-10-18-activity-profile-preference-overlay-design.md` section 19.

## Global Constraints

- Activity Profile evidence remains played activity only; do not reintroduce wishlist or direct-feedback evidence.
- Unknown inline evidence keys must fail validation, not be removed.
- Structured `evidence_keys` must remain non-empty, supplied-only, and duplicate-free.
- Preserve tolerant profile-summary length validation and auxiliary-array cleanup.
- Do not rewrite existing migrations, modify generated SQL, or commit without explicit authorization.

## Review Focus

- A parenthesized en-dash range is removed only when every expanded key is supplied.
- A bracketed list of supplied keys is removed without leaving doubled spaces or broken punctuation.
- A group containing an unknown key or a mixture of prose and keys is rejected.
- An ungrouped `signal-*` token is rejected rather than leaked or silently deleted.
- `preferences`, `avoidances`, and `uncertainty` receive the same protection as `summary`, while the prompt continues to forbid wishlist and direct-feedback wording.

---

### Task 1: Sanitize reader-facing profile citations

**Files:**
- Modify: `internal/ai/openai.go`
- Modify: `internal/ai/openai_test.go`
- Modify: `internal/ai/service_test.go`

**Interfaces:**
- Consumes: `normalizeAndValidateProfileOutput(ProfileSummary, map[string]struct{}) (ProfileSummary, error)` and generated profile keys `signal-%03d`.
- Produces: a private helper that sanitizes one reader-facing string against the supplied key set and returns `ErrInvalidEvidence` for unsafe inline citations.

- [x] **Step 1: Write failing table tests**

Add cases for supplied parenthesized ranges, supplied bracketed lists, unknown ranges, mixed prose/key groups, ungrouped keys, and all four reader-facing fields. Assert valid citation-only groups disappear cleanly and unsafe cases return `ErrInvalidEvidence`.

- [x] **Step 2: Run the focused tests and verify RED**

Run: `GOWORK=off go test -race ./internal/ai -run 'TestProfileSummary(InlineEvidence|UsesUnicodeLengthBoundaries|NormalizesOptionalArraysWithoutFailing|StillRejectsUnknownEvidence)'`

Expected: the new inline-evidence cases fail against the current validator.

- [x] **Step 3: Implement bounded cleanup**

Apply the sanitizer to `summary`, every retained `preferences` and `avoidances` value, and `uncertainty` before final length checks. Remove only citation-only `[]` or `()` groups whose single keys, lists, or same-prefix numeric ranges resolve entirely to allowed keys; reject all other inline `signal-*` occurrences.

- [x] **Step 4: Run focused tests and verify GREEN**

Repeat Step 2 and require zero failures.

### Task 2: Tighten and version the profile prompt

**Files:**
- Modify: `internal/ai/prompts.go`
- Modify: `internal/ai/prompts_test.go`
- Modify: `internal/ai/openai.go`
- Modify: `internal/ai/openai_test.go`

**Interfaces:**
- Produces: `ProfilePromptVersion = "profile-summary-v6-inline-evidence"` and structured response format name `profile_summary_v6`.

- [x] **Step 1: Write failing prompt/request tests**

Assert the prompt confines opaque evidence keys to structured `evidence_keys`, forbids them in all reader-facing fields, retains the one-caveat and activity-only rules, and sends the v6 response-format name.

- [x] **Step 2: Run focused tests and verify RED**

Run: `GOWORK=off go test -race ./internal/ai -run 'Test(ProfilePromptContract|ProfileExplanationAndFeedbackStructuredOperations)$'`

Expected: version/name/instruction assertions fail against v5.

- [x] **Step 3: Update the prompt contract**

Bump the prompt and response-format names and explicitly state that opaque evidence keys belong only in `evidence_keys`, never in `summary`, `preferences`, `avoidances`, or `uncertainty`. Keep the existing synthesis, recency, completion, and caveat instructions unchanged.

- [x] **Step 4: Run package and impact verification**

Run `GOWORK=off go test -race ./internal/ai`, `codegraph sync .`, `codegraph affected internal/ai/openai.go internal/ai/openai_test.go internal/ai/prompts.go internal/ai/prompts_test.go -p .`, and `git diff --check`. Require zero test failures and no whitespace errors.

