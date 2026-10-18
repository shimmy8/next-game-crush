# Grounded Recommendation Explanations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace repeated generic recommendation copy with specific, stored, deterministic evidence and optional grounded AI summaries without changing recommendation ordering.

**Architecture:** Build a versioned explanation-evidence snapshot from the same saved profile inputs used by deterministic ranking, persist it with each unpublished recommendation, and publish only after every item has safe presentation data. Keep evidence derivation, AI validation, persistence compatibility, diagnostics, and React rendering as separate units; OpenAI receives only selected public evidence and remains optional.

**Tech Stack:** Go 1.27.2 with `GOWORK=off`, PostgreSQL 16 with pgvector and Goose migrations, sqlc 1.31.1, React, TypeScript, Vitest, Playwright.

**Spec:** `docs/superpowers/specs/2026-10-18-grounded-recommendation-explanations-design.md`

## Global Constraints

- Preserve deterministic ranking and the current factor weights; this delivery must not redefine or reweight novelty.
- Never use wishlist membership, release precision, confidence, or catalog source as substantive evidence that the user may enjoy a game.
- Never send novelty or wishlist-only context to OpenAI as recommendation-affinity evidence.
- Preserve release-date precision and the last successful upstream-derived state.
- Keep OpenAI optional: a disabled, rejected, invalid, timed-out, rate-limited, or unavailable call must still publish a specific deterministic explanation when concrete evidence exists.
- Persist evidence from the ranking build snapshot; reads must not reinterpret an old recommendation with newer profile or Steam data.
- Keep `generatedExplanation` for one compatibility cycle and derive it from `explanationSource == "ai"`.
- Do not expose source UUIDs, factor values, weights, embeddings, exact playtime, prompts, raw errors, upstream bodies, SteamIDs, or secrets through the API, UI, diagnostics, or logs.
- Use a new numbered migration and run `make sqlc`; never edit `internal/store/sqlc/` by hand.
- Preserve unrelated working-tree changes, especially the in-progress profile-recency, discovery/wishlist, and frontend layout work.
- Use the isolated `TEST_DATABASE_URL` and Compose `test-db` for every database test; never clean or recreate the application database.

## Review Focus

- A candidate with a valid semantic score but no safe anchor or concrete feature overlap must show neutral copy, not a generic similarity claim; Task 2 pins this.
- Duplicate, oversized, malformed, or technical feature strings must be omitted without leaking JSON or internal identifiers; Task 2 pins this.
- Old, malformed, or v1 explanation evidence must degrade safely without making the recommendation endpoint fail; Task 5 pins this.
- Mixed AI outcomes in one rebuild must publish all recommendations and report only aggregate allowlisted reasons; Tasks 4–6 pin this.
- Large lifetime playtime with missing or old recency must not become a current-interest claim; Tasks 1–3 pin this.

---

### Task 1: Add explanation source persistence and reusable profile signal weights

**Files:**
- Create: `db/migrations/00009_grounded_explanations.sql`
- Modify: `internal/store/store_test.go`
- Modify: `internal/store/migration_upgrade_test.go`
- Modify: `internal/profile/vectors.go`
- Modify: `internal/profile/vectors_test.go`
- Generate: `internal/store/sqlc/models.go` via `make sqlc`

If another migration lands before execution, rename `00009_grounded_explanations.sql` to the next unused number before running it. Never rewrite an applied migration.

**Interfaces:**
- Produces: `recommendations.explanation_source text NOT NULL` constrained to `ai|deterministic|none` with default `deterministic`.
- Produces: nullable `recommendations.explanation_failure_reason text` constrained to `disabled|rate_limited|timeout|provider_unavailable|refusal|invalid_output|invalid_citation|unsafe_text|unknown`.
- Produces: `type SignalWeights struct { Positive, Negative, Playtime, Recent, Achievement, Wishlist, Feedback float64 }` in `profile`.
- Produces: `func CalculateSignalWeights(input []Signal, asOf time.Time) map[uuid.UUID]SignalWeights`.
- Preserves: `func BuildVectors(input []Signal, asOf time.Time) Vectors`, now implemented from `CalculateSignalWeights` with byte-for-byte-equivalent vector, confidence, and evidence-count behavior.

- [ ] **Step 1: Add failing migration coverage.** Extend `TestMigrateCreatesRequiredTables` to require both new recommendation columns and assert invalid source/reason values are rejected. Add `TestGroundedExplanationMigrationBackfillsLegacySources` in `migration_upgrade_test.go`: migrate an isolated schema through version 8, insert two complete recommendation rows with `explanation_generated=true/false`, migrate to latest, and assert sources `ai/deterministic` with null reasons.

- [ ] **Step 2: Run the store test red.** Run `GOWORK=off go test -race ./internal/store -run TestMigrateCreatesRequiredTables -count=1` with `TEST_DATABASE_URL`. Expected: FAIL because the columns do not exist.

- [ ] **Step 3: Add the migration.** Add both columns, backfill source from `explanation_generated`, add named check constraints, and provide a down migration that drops the constraints and columns without touching existing explanation data.

- [ ] **Step 4: Regenerate sqlc and prove reproducibility.** Run `make sqlc`; then rerun it into the repository and confirm `git diff -- internal/store/sqlc` is stable after the first generated change. Do not hand-edit generated files.

- [ ] **Step 5: Add failing profile-weight tests.** Add `TestCalculateSignalWeightsMatchesBuildVectors` and table cases for recent, old, missing-recency, wishlist, like, dislike, achievement, and invalid-vector signals. Assert missing recency receives the existing `.25` playtime recency weight, a ten-year-old signal is weaker than a recent one, and `BuildVectors` outputs remain unchanged for the current fixtures.

- [ ] **Step 6: Run the profile tests red.** Run `GOWORK=off go test -race ./internal/profile -run 'TestCalculateSignalWeights|TestBuildVectors' -count=1`. Expected: FAIL because `CalculateSignalWeights` is missing.

- [ ] **Step 7: Extract the shared weight calculation.** Implement the exact interface above and refactor `BuildVectors` to consume it. Keep stable GameID ordering and all current constants, including the two-year half-life and missing-recency weight.

- [ ] **Step 8: Run Task 1 green.** Run `GOWORK=off go test -race ./internal/store ./internal/profile -count=1` with `TEST_DATABASE_URL`; expected zero failures.

- [ ] **Step 9: Commit the isolated schema/profile change if the selected execution workflow uses commits.** Stage only the migration, store test, profile files, and generated sqlc output. Commit message: `feat: add explanation status persistence`.

### Task 2: Build and select concrete deterministic evidence

**Files:**
- Create: `internal/recommendations/evidence.go`
- Create: `internal/recommendations/evidence_test.go`
- Modify: `internal/recommendations/types.go`
- Modify: `internal/ai/client.go`
- Modify: `internal/ai/client_test.go`

**Interfaces:**
- Produces: `type EvidenceKind string` with `feature_overlap`, `similar_game`, `playtime_anchor`, `recent_activity_anchor`, `achievement_anchor`, `positive_feedback`, and `wishlist_context`.
- Produces: public `type Evidence struct { ID string; Kind EvidenceKind; Text string }`.
- Produces: `type FeatureValues struct { Themes, Mechanics, Modes, Tone []string }`.
- Produces: `type CandidateEvidence struct { GameID uuid.UUID; Vector []float32; Features FeatureValues }`.
- Produces: `type EvidenceAnchor struct { Name string; Signal profile.Signal; Features FeatureValues }`.
- Produces: private persisted `type GroundingEvidence struct { ID string; Kind EvidenceKind; PublicText string; Strength float64; SourceGameIDs []uuid.UUID }` and `type StoredEvidence struct { Version int; Items []GroundingEvidence }`, with version `2`.
- Produces: `func BuildGroundingEvidence(r Recommendation, candidate CandidateEvidence, anchors []EvidenceAnchor, asOf time.Time) []GroundingEvidence`.
- Produces: `func SelectGroundingEvidence(input []GroundingEvidence) []GroundingEvidence`.
- Produces: `func PublicEvidence(input []GroundingEvidence) []Evidence`.
- Produces: `func DeterministicExplanation(selected []GroundingEvidence) string`.
- Changes: `ai.DeterministicExplanation` is no longer the recommendation copy authority; keep or narrow it only for callers/tests that still require a generic capability-disabled value.

- [ ] **Step 1: Write evidence-builder tests red.** Cover different feature overlap producing different `feature_overlap` text; nearest positive semantic anchors; highest recency-adjusted playtime, recent, and achievement anchors; direct same-game positive feedback; exclusion of disliked/negative anchors; wishlist context; and deterministic tie order by normalized name, raw name, then UUID.

- [ ] **Step 2: Pin selection policy.** Add tests asserting contribution below `0.5` or below 5% of the strongest substantive contribution is omitted; at most two substantive kinds survive; a second item of a different kind wins over a duplicate kind; wishlist context never consumes a substantive slot; and membership-only input returns only `wishlist_context`.

- [ ] **Step 3: Pin safety and neutral behavior.** Add table tests for empty arrays, malformed/blank values, duplicates with case/whitespace differences, strings over the selected public-text limit, JSON-looking text, technical factor names, invalid vectors, and missing anchors. Assert no unsafe value is emitted and `DeterministicExplanation(nil)` is exactly `Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно.`

- [ ] **Step 4: Pin unsupported novelty.** Construct `Recommendation{Factors: Factors{Novelty: 1}, Weights: DefaultWeights()}` with no other evidence and assert neither built evidence, public evidence, nor deterministic copy contains novelty or `Добавляет новое`.

- [ ] **Step 5: Run Task 2 red.** Run `GOWORK=off go test -race ./internal/recommendations ./internal/ai -run 'Test(Build|Select|Public|Deterministic).*Evidence|TestUnsupportedNovelty|TestDeterministicExplanation' -count=1`. Expected: compile/test failure because the types and functions are missing.

- [ ] **Step 6: Implement the evidence domain.** Use `profile.CalculateSignalWeights` for anchor eligibility and existing cosine behavior for embedding proximity. For `feature_overlap`, intersect the candidate's four allowed feature arrays with positive anchors, rank values by summed positive anchor weight, then case-insensitive value/raw value, and keep three. For `similar_game`, rank positive non-disliked anchors by cosine times positive weight and keep two names. For playtime/recent/achievement, rank anchors by feature overlap times that signal's component weight and keep one name. Direct positive feedback requires an anchor with the candidate GameID and positive feedback weight. Give each kind the matching ranking-factor contribution as private strength; on equal strength use kind priority `positive_feedback`, `feature_overlap`, `similar_game`, `recent_activity_anchor`, `playtime_anchor`, `achievement_anchor`. Create opaque sequential IDs such as `feature-overlap-1`; never derive IDs from names or UUIDs.

- [ ] **Step 7: Implement deterministic copy.** Use the templates `Совпадают темы и механики: %s.`, `Игры с похожим профилем: %s.`, `Есть сходство с игрой %s, которой вы уделили много времени.`, `Есть сходство с игрой %s, которую вы запускали недавно.`, `Есть сходство с игрой %s, где у вас заметный прогресс достижений.`, and `Вы уже отмечали эту игру как понравившуюся.` Join at most two selected facts without adding a generic lead-in. Membership-only copy is exactly `Вы уже добавили эту игру в желаемое — следим за датой выхода.` No evidence returns the exact neutral copy from Step 3.

- [ ] **Step 8: Run Task 2 green.** Run `GOWORK=off go test -race ./internal/recommendations ./internal/ai -count=1`; expected zero failures.

- [ ] **Step 9: Commit if applicable.** Stage only evidence domain/types and AI fallback changes. Commit message: `feat: build grounded recommendation evidence`.

### Task 3: Load immutable explanation corpus data

**Files:**
- Modify: `internal/recommendations/retrieval.go`
- Modify: `internal/recommendations/repository_integration_test.go`
- Modify: `internal/recommendations/types.go`

**Interfaces:**
- Consumes: `CandidateEvidence`, `EvidenceAnchor`, and `FeatureValues` from Task 2.
- Produces: `type EvidenceCorpus struct { Candidates map[uuid.UUID]CandidateEvidence; Anchors []EvidenceAnchor }`.
- Produces: `func (r *SQLRepository) ExplanationCorpus(ctx context.Context, userID uuid.UUID, gameIDs []uuid.UUID) (EvidenceCorpus, error)`.
- Expands: recommendation `Repository` with `ExplanationCorpus(context.Context, uuid.UUID, []uuid.UUID) (EvidenceCorpus, error)`.

- [ ] **Step 1: Write a failing repository integration test.** Create two candidate games and profile-source games covering recent, old, missing-recency, wishlist, positive feedback, negative feedback, achievements, features, and embeddings. Assert requested candidates contain only their configured-model features/vector and anchors contain names plus the same raw signals used by `profile.LoadSignals`.

- [ ] **Step 2: Add isolation and absence cases.** Assert another user's library/feedback never appears; missing configured-model features or embedding yields an empty candidate field rather than an error; duplicate feedback is aggregated exactly like profile signals; an empty `gameIDs` input returns an empty corpus without querying personal rows.

- [ ] **Step 3: Run Task 3 red.** Run `GOWORK=off go test -race ./internal/recommendations -run TestExplanationCorpus -count=1` with `TEST_DATABASE_URL`. Expected: compile failure because the method is missing.

- [ ] **Step 4: Implement bounded corpus queries.** Load candidate rows in one query scoped to the requested IDs and configured models. Load anchor rows with the same relevant-game union and feedback aggregation as `profile.LoadSignals`; keep raw signal values and perform explanation selection in `evidence.go`, not SQL. Preserve `last_played_at` as UTC and copy vectors before rows close.

- [ ] **Step 5: Run Task 3 green.** Run `GOWORK=off go test -race ./internal/recommendations -run 'TestExplanationCorpus|TestRebuildRetrieves' -count=1` with `TEST_DATABASE_URL`; expected zero failures.

- [ ] **Step 6: Commit if applicable.** Stage only retrieval/types and repository integration tests. Commit message: `feat: load recommendation grounding context`.

### Task 4: Classify optional AI explanation outcomes safely

**Files:**
- Modify: `internal/ai/prompts.go`
- Modify: `internal/ai/prompts_test.go`
- Modify: `internal/ai/openai.go`
- Modify: `internal/ai/openai_test.go`
- Modify: `internal/recommendations/service.go`
- Modify: `internal/recommendations/service_test.go`

**Interfaces:**
- Consumes: selected substantive public evidence from Task 2; never receives `wishlist_context`.
- Produces: `type ExplanationSource string` with `ai`, `deterministic`, and `none`.
- Produces: `type ExplanationFailureReason string` with the migration's exact allowlist.
- Produces: `type ExplanationResult struct { Text string; Source ExplanationSource; FailureReason ExplanationFailureReason; EvidenceKeys []string }`.
- Produces: `func Explain(ctx context.Context, client ExplanationClient, name string, selected []GroundingEvidence) ExplanationResult`.
- Produces: `func classifyExplanationFailure(err error) ExplanationFailureReason`.
- Updates: `ExplanationPromptVersion` to `recommendation-explanation-v3` and the structured-output schema name to `recommendation-explanation-v2`.

- [ ] **Step 1: Write failure-classification tests red.** Table-test disabled capability, HTTP 429, deadline/408, HTTP 5xx/transport, refusal, invalid evidence, invalid output, unsafe Russian text, and unclassified errors. Assert the exact reasons `disabled`, `rate_limited`, `timeout`, `provider_unavailable`, `refusal`, `invalid_citation`, `invalid_output`, `unsafe_text`, and `unknown`.

- [ ] **Step 2: Write AI boundary tests red.** Assert zero substantive evidence and membership-only evidence skip the client; accepted output cites at least one supplied unique substantive ID; duplicate/unknown/empty citations fall back; AI input contains only public ID/text and contains no novelty, wishlist context, strength, UUID, factor value, or weight.

- [ ] **Step 3: Pin prompt/schema behavior.** Update tests to require v3 prompt language forbidding unsupported novelty and wishlist-as-affinity. Assert the strict schema still requires text, facts, inferences, uncertainty, and evidence keys and dynamically enumerates only supplied opaque keys.

- [ ] **Step 4: Run Task 4 red.** Run `GOWORK=off go test -race ./internal/ai ./internal/recommendations -run 'Test.*Explanation|TestClassifyExplanationFailure' -count=1`. Expected: failures for missing result types/reason mapping and old prompt version.

- [ ] **Step 5: Implement typed outcomes.** Map only safe error metadata; never store `err.Error()`. Build deterministic text before calling the client so every failure path returns the same specific fallback. Return source `none` only for no evidence, `deterministic` for concrete or membership-only fallback, and `ai` only after all validation passes.

- [ ] **Step 6: Run Task 4 green.** Run `GOWORK=off go test -race ./internal/ai ./internal/recommendations -count=1`; expected zero failures.

- [ ] **Step 7: Commit if applicable.** Stage only prompts/OpenAI validation and recommendation explanation service/tests. Commit message: `feat: classify grounded explanation outcomes`.

### Task 5: Persist v2 evidence, publish mixed outcomes, and sanitize legacy reads

**Files:**
- Modify: `internal/recommendations/service.go`
- Modify: `internal/recommendations/types.go`
- Modify: `internal/recommendations/retrieval.go`
- Modify: `internal/recommendations/repository_integration_test.go`
- Modify: `internal/httpapi/ranking_handlers_test.go`

**Interfaces:**
- Consumes: `EvidenceCorpus` from Task 3 and `ExplanationResult` from Task 4.
- Produces: `type ExplanationRecord struct { Text string; Source ExplanationSource; FailureReason ExplanationFailureReason; Supplied []GroundingEvidence; CitedIDs []string }`.
- Changes: repository method to `SaveExplanation(ctx context.Context, id uuid.UUID, gameName string, record ExplanationRecord) error`.
- Produces: `type ExplanationStats struct { AIGenerated, Deterministic, None int; FailureReasons map[ExplanationFailureReason]int }`.
- Changes: rebuild's internal return to include `ExplanationStats`; public `Rebuild` keeps returning recommendations and error.
- Public `Recommendation` adds `ExplanationSource ExplanationSource` and continues deriving `GeneratedExplanation bool` for compatibility.

- [ ] **Step 1: Write service tests red.** Use a fake repository returning a corpus with two different candidates and anchors. Assert one AI success plus one rate-limited fallback both publish; deterministic texts differ; stats equal AI 1/deterministic 1 with one `rate_limited`; and wishlist-only input skips the model.

- [ ] **Step 2: Write persistence integration tests red.** Rebuild a ranking with AI disabled and assert v2 JSON evidence, exact source/reason columns, v2 input hash changes when concrete evidence changes, no private source refs in API JSON, and `generatedExplanation == (explanationSource == "ai")`.

- [ ] **Step 3: Pin atomic failure behavior.** Force corpus loading or explanation persistence to fail after an older ranking is published. Assert the new run stays unpublished and `Latest` still returns the prior ranking. An AI error alone must not cause this failure.

- [ ] **Step 4: Pin legacy compatibility.** Insert safe generated v1, generic deterministic v1, malformed JSON, unknown kind/version, technical text, and empty evidence rows. Assert endpoint reads never fail; only safe generated legacy output survives; all other rows return source `none`, the exact neutral copy, and an empty public evidence array.

- [ ] **Step 5: Run Task 5 red.** Run `GOWORK=off go test -race ./internal/recommendations ./internal/httpapi -run 'Test(Rebuild.*Explanation|ExplanationPersistence|ExplanationAtomicity|LegacyExplanation|RankingRead)' -count=1` with `TEST_DATABASE_URL`. Expected: compile/assertion failures for old signatures and read behavior.

- [ ] **Step 6: Integrate the build flow.** After scoring and before publication, load one corpus for all recommendation game IDs; build/select evidence using `p.CreatedAt` as the recency snapshot time; call optional AI; persist `StoredEvidence{Version:2}` plus source/reason/text/hash; collect stats; publish only after every row is saved.

- [ ] **Step 7: Version the persistence hash.** Hash the text model, `recommendation-explanation-v3` prompt version, evidence schema version `2`, game name, and selected public evidence. Store null rather than an empty string for no failure reason, and keep the compatibility boolean derived from source on every write.

- [ ] **Step 8: Implement strict read mapping.** Decode persisted evidence into the private v2 type, validate version/kind/text/citations, map only public ID/kind/text, and derive the compatibility boolean. Never call `BuildGroundingEvidence` from `Latest` or `Get`.

- [ ] **Step 9: Record aggregate job result.** Add `explanations` to the existing recommendation rebuild result with keys `aiGenerated`, `deterministic`, `none`, and `failureReasons`; omit zero failure-reason entries and sort/encode deterministically.

- [ ] **Step 10: Run Task 5 green.** Run `GOWORK=off go test -race ./internal/recommendations ./internal/httpapi -count=1` with `TEST_DATABASE_URL`; expected zero failures.

- [ ] **Step 11: Commit if applicable.** Stage only recommendation service/repository/types and their tests. Commit message: `feat: persist grounded recommendation explanations`.

### Task 6: Expose aggregate explanation diagnostics without personal data

**Files:**
- Modify: `internal/httpapi/data_handlers.go`
- Modify: `internal/httpapi/data_handlers_test.go`
- Modify: `internal/httpapi/data_service_integration_test.go`
- Modify: `web/src/api/types.ts`
- Modify: `web/src/pages/DiagnosticsPage.tsx`
- Modify: `web/src/pages/DiagnosticsPage.test.tsx`
- Modify: `web/src/ui/labels.ts`

**Interfaces:**
- Consumes: recommendation job-result `explanations` object from Task 5.
- Produces: `type ExplanationDiagnostic struct { AIGenerated int; Deterministic int; None int; FailureReasons map[string]int }` in Go and the matching TypeScript type.
- Adds: optional `JobDiagnostic.Explanations *ExplanationDiagnostic` / `explanations?: ExplanationDiagnostic` only for validated `recommendations.rebuild` results.

- [ ] **Step 1: Write backend diagnostics tests red.** Insert a successful rebuild result containing valid aggregate counts plus injected extra keys, a raw game name, UUID, prompt-like text, negative counts, and an unknown failure reason. Assert the API returns only nonnegative counts and the exact allowlisted failure keys, with no extra strings or identifiers.

- [ ] **Step 2: Add absent/malformed cases.** Assert other job kinds, missing results, malformed result shapes, and dead jobs without aggregate results omit `explanations` while preserving existing sanitized error behavior.

- [ ] **Step 3: Run backend diagnostics red.** Run `GOWORK=off go test -race ./internal/httpapi -run 'Test.*ExplanationDiagnostic|TestDataService' -count=1` with `TEST_DATABASE_URL`. Expected: failure because the DTO/query parser is missing.

- [ ] **Step 4: Implement allowlist parsing.** Select `result` in `Jobs`, decode only for kind `recommendations.rebuild`, copy integer counts through a nonnegative clamp, and rebuild the failure map by iterating the fixed allowlist. Never pass arbitrary JSON through.

- [ ] **Step 5: Write frontend diagnostics tests red.** Assert Russian aggregate labels show AI/deterministic/neutral counts and localized allowlisted failure counts; unknown keys and raw identifiers are never rendered; jobs without explanation diagnostics retain current UI.

- [ ] **Step 6: Run frontend diagnostics red.** Run `pnpm --dir web run test -- src/pages/DiagnosticsPage.test.tsx`; expected failure for missing rendering/types.

- [ ] **Step 7: Implement diagnostics UI.** Add fixed labels for all nine failure reasons and render aggregate counts only inside the recommendation rebuild job entry. Do not show per-game details or raw provider text.

- [ ] **Step 8: Run Task 6 green.** Run backend tests with `-race`, then `pnpm --dir web run test -- src/pages/DiagnosticsPage.test.tsx` and `pnpm --dir web run typecheck`; expected zero failures.

- [ ] **Step 9: Commit if applicable.** Stage only diagnostics Go/TypeScript/UI files. Commit message: `feat: report explanation fallback diagnostics`.

### Task 7: Render one source-aware explanation surface per card

**Files:**
- Modify: `web/src/api/types.ts`
- Modify: `web/src/api/client.ts`
- Modify: `web/src/api/client.test.ts`
- Modify: `web/src/components/RecommendationCard.tsx`
- Modify: `web/src/components/RecommendationCard.test.tsx`
- Modify: `web/src/styles.css`
- Modify when present after discovery work: shared recommendation fixtures used by `web/src/pages/DashboardPage.test.tsx` and overview normalization tests.

**Interfaces:**
- Consumes: `evidence: {id, kind, text}[]`, `explanationSource: 'ai'|'deterministic'|'none'`, `explanation`, and compatibility `generatedExplanation`.
- Produces: normalized recommendations default missing/unknown source to `none`, discard evidence with unknown kinds or unsafe text, and derive no new evidence client-side.
- AI presentation: main paragraph, optional collapsed `На чём основано`, source label `AI-сводка по вашим игровым данным`.
- Deterministic presentation: one visible list of up to two substantive reasons and no duplicate paragraph/drawer; membership-only shows the saved watchlist sentence once; source label `По вашим игровым данным`.
- None presentation: exact neutral paragraph and no evidence control/source claim.

- [ ] **Step 1: Update typed fixtures and normalization tests red.** Require `kind` and `explanationSource`; assert absent source becomes `none`, unknown kinds are discarded, malformed evidence arrays normalize to empty, and `generatedExplanation` alone does not cause the frontend to trust unsafe content.

- [ ] **Step 2: Write card state tests red.** Add table cases for AI with two citations, deterministic with two concrete reasons, deterministic membership-only, none, legacy unsafe strings, compact cards, and an evidence text identical to the AI paragraph. Assert each user-facing fact appears once in deterministic mode, AI has one paragraph plus a collapsed traceability control, and none/membership-only never show a misleading `Почему вам может понравиться` heading.

- [ ] **Step 3: Pin existing behavior.** Retain assertions for cover fallback, Steam link, wishlist badge, feedback form, release precision, keyboard-accessible disclosure, and compact-card omission of explanation details.

- [ ] **Step 4: Run Task 7 red.** Run `pnpm --dir web run test -- src/api/client.test.ts src/components/RecommendationCard.test.tsx` plus any overview/dashboard fixture tests present after discovery work. Expected: type/assertion failures for the old contract/rendering.

- [ ] **Step 5: Implement contract normalization.** Keep the allowlist in one exported TypeScript constant/type guard; never infer source from prose. Preserve `generatedExplanation` only as a compatibility field and use `explanationSource` for rendering.

- [ ] **Step 6: Implement source-aware card markup.** Use semantic list markup for deterministic reasons, `<details>` only for AI traceability, and existing safe-text filtering as defense in depth. Update styles without changing card layout, media, feedback, or discovery/wishlist section ownership.

- [ ] **Step 7: Run Task 7 green.** Run `pnpm --dir web run test`, `pnpm --dir web run typecheck`, `pnpm --dir web run lint`, and `pnpm --dir web run build`; expected zero failures.

- [ ] **Step 8: Commit if applicable.** Stage only API contract/normalization, card tests/component, affected typed fixtures, and styles. Commit message: `feat: present grounded recommendation reasons`.

### Task 8: Prove the end-to-end explanation matrix and finish verification

**Files:**
- Modify: `cmd/e2e-fakes/main.go`
- Modify: `web/tests/e2e/smoke.spec.ts`
- Create: `web/tests/e2e/explanations.spec.ts`
- Modify: `Makefile`
- Modify: `docs/local-demo.md`
- Modify only if verification exposes a defect: files from Tasks 1–7.

**Interfaces:**
- Adds fake OpenAI explanation modes behind the existing test harness only: `success`, `rate_limited`, `invalid_citation`, `unsafe_text`, and `disabled`/no-key startup.
- Adds no production fixture route and no new external service.
- `make verify` runs both the existing smoke suite and `tests/e2e/explanations.spec.ts`.

- [ ] **Step 1: Add browser tests red.** Build fixture data with at least two recommendations having intentionally different feature overlap/anchors. With OpenAI unavailable, assert different deterministic reasons, no novelty copy, no repeated paragraph/list, and correct wishlist-only/neutral states. With success, assert AI prose plus `На чём основано`; with rate limit/invalid citation/unsafe text, assert specific deterministic fallback remains visible.

- [ ] **Step 2: Add diagnostics browser assertions.** After mixed outcomes, open authenticated diagnostics and assert aggregate AI/deterministic/none and allowlisted failure counts. Assert no game names, UUIDs, prompt text, raw errors, or provider bodies appear in the diagnostic explanation summary.

- [ ] **Step 3: Run browser tests red.** Run `make prepare-e2e-db`, then `pnpm --dir web exec playwright test tests/e2e/smoke.spec.ts tests/e2e/explanations.spec.ts`. Expected: failures for missing fake modes/new UI.

- [ ] **Step 4: Implement bounded fake modes.** Return strict structured-output responses using only request-supplied opaque evidence keys. Rate-limit mode returns 429 without a body dependency; invalid/unsafe modes contain no secrets. Reset fake state at every test start.

- [ ] **Step 5: Update verification wiring and run focused browser green.** Make `verify` invoke both browser files. Run the two-file Playwright command against the isolated E2E database; expected zero failed tests. If the pinned browser download is unavailable, use only the README-documented explicit `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` fallback and record its path/version.

- [ ] **Step 6: Update the local demo runbook.** Document the new source labels, deterministic/AI/card states, safe aggregate explanation diagnostics, and a manual check that two different games do not repeat generic top-three reasons. Do not add credentials, live account data, or raw provider responses.

- [ ] **Step 7: Refresh CodeGraph and select regressions.** Run `codegraph sync .`, then `codegraph affected internal/profile/vectors.go internal/recommendations/evidence.go internal/recommendations/retrieval.go internal/recommendations/service.go internal/ai/openai.go internal/httpapi/data_handlers.go web/src/api/types.ts web/src/components/RecommendationCard.tsx web/src/pages/DiagnosticsPage.tsx -p .`. Run every additional focused test the graph identifies; source and test output override stale graph results.

- [ ] **Step 8: Run full verification.** Run `make verify` and `git diff --check`. Expected: migration/sqlc reproduction clean; all Go race, frontend test/typecheck/lint/build, Docker, and both Playwright suites pass with zero failures.

- [ ] **Step 9: Inspect the final diff.** Confirm no `.env`, CodeGraph database/state, browser downloads, build output, raw prompts, provider bodies, personal IDs, or unrelated working-tree changes are staged. Confirm every new migration, generated sqlc change, Go/TypeScript contract, and test belongs to this feature.

- [ ] **Step 10: Request independent review using the selected execution workflow.** Review specifically for deterministic ordering, grounding leaks, legacy compatibility, atomic publication, allowlisted diagnostics, and duplicate card copy. Resolve every critical or important finding and rerun the affected focused tests plus `make verify` before completion.

- [ ] **Step 11: Commit final fixtures/docs if applicable.** Stage only E2E fakes/tests, Makefile, and local-demo documentation. Commit message: `test: verify grounded recommendation explanations`.

## Acceptance Coverage and Handoff Audit

| Spec acceptance | Owning tasks/tests |
| --- | --- |
| Different saved inputs produce different fallback explanations | Tasks 2, 5, and 8 |
| Every displayed reason is concrete and grounded | Tasks 2–5 and 7 |
| Constant novelty never reaches user copy or AI | Tasks 2, 4, and 8 |
| Wishlist is context, not an affinity slot | Tasks 2, 4, 5, and 7 |
| Deterministic card does not duplicate paragraph/list | Tasks 7 and 8 |
| AI receives only selected public evidence | Task 4 |
| Complete OpenAI outage still publishes useful recommendations | Tasks 4, 5, and 8 |
| Safe aggregate failure diagnostics | Tasks 5, 6, and 8 |
| Legacy malformed/technical content degrades safely | Tasks 5 and 7 |
| Go/TypeScript source/evidence contracts agree | Tasks 1, 5–7 |
| Ranking, release precision, and datastore scope remain unchanged | Tasks 1–5 and final `make verify` |

Self-review: every spec section maps to at least one task above. The Go and TypeScript contracts use the same three explanation sources, seven evidence kinds, and nine failure reasons. Stored v2 grounding data is private; public evidence exposes only ID/kind/text. The plan deliberately leaves novelty scoring unchanged, excludes it from explanation inputs, uses `p.CreatedAt` for immutable recency interpretation, and keeps optional AI failures outside ranking publication failure. No task introduces Redis, a second database, multi-user behavior, or a ranking authority outside the deterministic scorer.
