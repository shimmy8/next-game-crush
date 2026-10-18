# Recommendation Presentation Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace leaked technical recommendation data with grounded Russian explanations, recover unavailable AI-profile summaries, and add Steam cover images and store links.

**Architecture:** Keep deterministic ranking and internal factor snapshots unchanged. Derive a separate public evidence list from positive weighted factors, expose Steam presentation metadata through the recommendation read model, and represent AI-summary availability separately from summary content. Existing database columns are sufficient; no migration is required.

**Tech Stack:** Go 1.27.2, PostgreSQL 16 with pgvector, React, TypeScript, Vitest.

**Spec:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`

## Global Constraints

- Preserve deterministic ranking; OpenAI must not become the ranking authority.
- Never expose raw factor JSON, prompts, upstream bodies, secrets, Steam IDs, or unsafe external URLs.
- Preserve the last successful upstream-derived state when Steam or OpenAI fails.
- Keep Go and TypeScript API contracts aligned.
- Do not add a database migration for data already stored in `games.source_snapshot` and `games.steam_app_id`.
- Verification must not download or run Chrome/Playwright; use Go tests plus frontend unit tests, typecheck, lint, and build.

## Review Focus

- Legacy recommendations containing raw JSON must render only safe derived reasons.
- Missing, malformed, non-HTTPS, or lookalike-host cover URLs must fall back without losing the Steam link.
- A dead or unavailable AI-summary job must not masquerade as an English model-generated summary.
- Generated explanations may cite only the human-readable evidence supplied to OpenAI.
- Compact and full recommendation cards must both retain an accessible Steam store link.

---

### Task 1: Human-readable recommendation evidence

**Files:**
- Modify: `internal/recommendations/service.go`
- Modify: `internal/recommendations/retrieval.go`
- Modify: `internal/ai/client.go`
- Test: `internal/recommendations/service_test.go`
- Test: `internal/recommendations/repository_integration_test.go`

**Interfaces:**
- Consumes: `Recommendation.Factors`, `Recommendation.Weights`.
- Produces: `publicEvidence(Recommendation) []Evidence` with at most three Russian positive reasons and `safePresentation(Recommendation) Recommendation` for legacy rows.

- [ ] Add failing tests proving fallback explanation/evidence contain no JSON, factor keys, internal score, or release precision and contain at most three Russian reasons.
- [ ] Run `GOWORK=off go test ./internal/recommendations` and confirm the new tests fail.
- [ ] Implement positive-contribution evidence selection, Russian deterministic fallback copy, and read-time legacy sanitization.
- [ ] Run `GOWORK=off go test -race ./internal/recommendations ./internal/ai` and confirm zero failures.

### Task 2: Steam cover and store presentation contract

**Files:**
- Modify: `internal/steam/types.go`
- Modify: `internal/recommendations/types.go`
- Modify: `internal/recommendations/retrieval.go`
- Modify: `web/src/api/types.ts`
- Test: `internal/recommendations/repository_integration_test.go`
- Test: `internal/steam/http_client_test.go`

**Interfaces:**
- Produces: `steamAppId int64`, `coverImageUrl,omitempty string`, and canonical `storeUrl string` on `Recommendation`.
- Produces: `steam.ValidateCoverURL(raw string) (string, error)` accepting only trusted HTTPS Steam CDN hosts.

- [ ] Add failing URL-validation and repository round-trip tests for valid, missing, malformed, HTTP, userinfo, port, and lookalike-host cover URLs.
- [ ] Run focused Go tests and confirm the new tests fail.
- [ ] Select `g.steam_app_id` and `g.source_snapshot`, derive a validated cover URL, and build `https://store.steampowered.com/app/{appid}/` server-side.
- [ ] Update Go and TypeScript contracts and run focused Go tests with `-race`.

### Task 3: AI profile-summary recovery and state

**Files:**
- Modify: `internal/profile/service.go`
- Modify: `internal/profile/repository.go`
- Modify: `internal/profile/service_test.go`
- Modify: `internal/httpapi/profile_handlers.go`
- Modify: `internal/httpapi/ranking_handlers_test.go`
- Modify: `web/src/api/types.ts`
- Modify: `web/src/pages/DashboardPage.tsx`
- Test: corresponding Go and frontend tests.

**Interfaces:**
- Produces: profile JSON with `summaryStatus: "ready" | "unavailable"` and an empty summary when unavailable.
- Produces: a safe, idempotent re-enqueue path for a failed optional `ai.profile_summary` job on a later profile rebuild/sync.

- [ ] Add failing tests proving a missing summary is not replaced with English prose and is reported as unavailable.
- [ ] Add failing queue/service tests proving a later rebuild can enqueue a fresh summary job after a dead attempt without blocking deterministic recommendations.
- [ ] Implement summary state, localized UI copy, and idempotent retry behavior.
- [ ] Run focused Go tests with `-race` and frontend unit tests.

### Task 4: Recommendation card media and safe presentation

**Files:**
- Modify: `web/src/components/RecommendationCard.tsx`
- Modify: `web/src/components/RecommendationCard.test.tsx`
- Modify: `web/src/styles.css`

**Interfaces:**
- Consumes: `Recommendation.coverImageUrl?` and `Recommendation.storeUrl`.
- Produces: cover image with accessible fallback and an external `Открыть в Steam` link in full and compact cards.

- [ ] Add failing component tests for image/link attributes, load-error fallback, missing image, compact cards, and suppression of legacy technical strings.
- [ ] Run the focused Vitest test and confirm failure.
- [ ] Implement the cover/link UI and bounded legacy display defense.
- [ ] Run the focused Vitest test and confirm success.

### Task 5: Integrated verification and local refresh

**Files:**
- Modify only if verification exposes a defect.

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: a locally running service whose next sync rebuilds presentation data and retries the AI summary.

- [ ] Run `codegraph sync .` and inspect affected symbols.
- [ ] Run `GOWORK=off go test -race ./...`.
- [ ] Run `pnpm --dir web run test`, `pnpm --dir web run typecheck`, `pnpm --dir web run lint`, and `pnpm --dir web run build`.
- [ ] Run `git diff --check`.
- [ ] Rebuild/restart only the local app service, trigger or document the next sync, and verify safe aggregate database status without reading secrets or raw payloads.
- [ ] Request independent code review and resolve all critical or important findings.
