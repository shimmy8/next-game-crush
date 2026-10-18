# Achievement Completion Signals Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Infer persistent game-completion signals from Steam achievements and use them deterministically in profiles, recommendations, explanations, and profile summaries without adding manual completion controls.

**Architecture:** Extend the existing bounded achievement sync to persist normalized per-achievement metadata and historical unlock facts, using `GetPlayerAchievements` first and `GetSchemaForGame` only when metadata is missing. A new `internal/completion` subsystem builds atomic semantic generations from unchanged-result reuse, a versioned conservative English lexical classifier, and validated OpenAI output; it then owns deterministic Go projections consumed by profiles and recommendations, while explicit direct negative feedback suppresses positive use without deleting history.

**Tech Stack:** Go 1.27.2, PostgreSQL 16 with pgvector, sqlc 1.31.1, existing PostgreSQL job queue, OpenAI Responses structured outputs, React/TypeScript with Vitest and Playwright, Docker Compose.

**Spec:** `docs/superpowers/specs/2026-10-18-achievement-completion-signals-design.md`

## Global Constraints

- Use Go `1.27.2` with `GOWORK=off`; do not add dependencies or persistent services.
- Execute only from a clean baseline or isolated worktree that already contains the current approved prerequisite work; the present checkout has unrelated modified and untracked files that must not be staged or overwritten.
- Keep the MVP single-user and local-first; do not add manual completion fields, endpoints, actions, correction controls, or UI.
- Use the existing Steam `playtime_2weeks` value persisted as `recently_played_minutes`; a configurable period or period UI is out of scope.
- Use `GetPlayerAchievements/v1?l=en` as the primary metadata source and `GetSchemaForGame/v2?l=en` only when a successful player response lacks usable names or descriptions.
- Count only distinct historically unlocked `base_game_ending` and `base_game_completion` achievements; DLC, meta all-endings, partial, optional, other, and unknown classes contribute zero.
- The deterministic completion weight is `1.50` for 100%-only or one ending, `1.85` for two endings, `2.05` for three endings, and `min(2.25, 2.05 + 0.10*(n-3))` for `n >= 4`; 100% and ending bases use `max`, never addition.
- A positive completion component replaces the same game's achievement-ratio component, never recency-decays, and remains independent of playtime, recent activity, and positive feedback.
- The latest direct `dislike` or legacy `exclude` suppresses positive completion use without deleting the historical projection; a later authoritative `like` removes that current override.
- OpenAI may return only versioned semantic classes. Go owns projection, weights, affinities, score, eligibility, evidence selection, and deterministic ordering.
- Apply `lexical-completion-v1` before OpenAI: only anchored English phrases for completing/finishing the game/main game/main story/story/campaign or reaching an ending/epilogue qualify; chapter/act/mission/level, DLC/expansion, NG+, difficulty, speedrun, collection, all-endings, and meta language veto a match; it returns only a qualifying class or no-match/unknown, never a weight.
- Reuse unchanged classifications by definition content hash. A complete generation may merge reused, lexical, and validated OpenAI results; without an active generation an unavailable provider may leave unresolved definitions as `unknown`, but with an active generation provider failure cannot publish a weaker replacement or change the projection.
- `achievement.classify` is shared game-scoped work with no `UserID` or queue ownership; activation fans out to one user-owned, deletion-fenced `completion.project` job per existing affected user.
- Preserve the last successful Steam snapshot, active semantic generation, completion projection, profile, and ranking on optional upstream or rebuild failure; never represent unavailable/private/failed data as a successful empty set.
- Add `db/migrations/00015_achievement_completion_signals.sql`; do not rewrite migrations `00001`–`00014`.
- Change SQL under `db/queries/`, run `make sqlc`, and never hand-edit `internal/store/sqlc/`.
- Keep Go API DTOs, persisted factor/evidence JSON, `web/src/api/types.ts`, frontend normalization, and contract tests aligned.
- Never log or expose SteamID, API achievement names, descriptions, raw upstream bodies, raw prompts, job payloads, credentials, UUIDs, or numeric internal weights in public evidence or diagnostics.
- Use the Compose `test-db` and an isolated `TEST_DATABASE_URL` for database tests; never clean or recreate the application database.
- Before every task, record which scoped paths are already modified or untracked. Before every commit, inspect `git diff -- <scoped paths>` and `git diff --no-index /dev/null <new file>` as applicable; use `git add -p -- <path>` for every file that was already modified at task start and plain `git add -- <path>` only for files newly created by that task. Then run `git diff --cached --check` and `git diff --cached --name-status`, verify only the task's hunks/files are staged, and commit. Never use broad `git add .` or whole-file staging for a pre-existing modified file.

## Review Focus

1. **Task 5 — partial, local-only, or failed classifier generation:** reused and lexical results may seed the first complete generation with unresolved unknowns when OpenAI is unavailable, but a staged subset or weaker failure-derived generation must never replace an active complete generation or weaken a projection; pinned by `TestClassificationGenerationActivatesAtomicallyAndPreservesLastGood` and `TestClassificationUnavailablePublishesLocalFirstGenerationOnly`.
2. **Task 3 — schema fallback failure:** valid player unlock state must commit even when schema metadata is unavailable, while prior shared definitions remain; pinned by `TestAchievementSyncSchemaFallbackFailurePreservesPlayerSnapshotAndDefinitions`.
3. **Task 6 — publisher adds achievements after observed 100%:** the ratio may fall but the historical `1.50` completion basis must remain; pinned by `TestProjectCompletionRetainsHistoricalHundredPercentAfterCatalogGrowth`.
4. **Task 7 — contradictory explicit feedback:** latest direct dislike/exclude must suppress positive completion and a later like must restore it without deleting negative history; pinned by `TestLoadSignalsLatestDirectActionOverridesCompletionOnly`.
5. **Task 8 — evidence cap over 100 games:** positive and overridden completion facts must precede Steam two-week activity, which precedes other explicit feedback, wishlist, and history, while overrides remain explainable; pinned by `TestLoadProfileEvidenceProtectsTwoWeekActivityAndCompletionFromLimit`.

---

## File Map

### Steam acquisition

- Modify `internal/steam/types.go` — achievement item/definition/schema types, operation and cache key.
- Modify `internal/steam/client.go` — expose the schema fallback call.
- Modify `internal/steam/http_client.go` — parse localized player achievement metadata and `GetSchemaForGame/v2`.
- Modify `internal/steam/http_client_test.go` — primary/fallback contracts and malformed-response tests.
- Modify `internal/steam/testdata/achievements.json` — localized player-achievement fixture.
- Create `internal/steam/testdata/achievement_schema.json` — schema fallback fixture.

### Persistence and completion subsystem

- Create `db/migrations/00015_achievement_completion_signals.sql` — six new completion/achievement tables, constraints, indexes, and legacy 100% backfill.
- Create `db/queries/achievement_completion.sql` — generated CRUD and current-generation queries.
- Modify `internal/store/sqlc/models.go` and create/modify generated `internal/store/sqlc/achievement_completion.sql.go` via `make sqlc` only.
- Modify `internal/store/migration_upgrade_test.go` and `internal/store/store_test.go` — upgrade/backfill/schema coverage.
- Create `internal/completion/types.go` — persisted domain values and repository/service interfaces.
- Create `internal/completion/repository.go` — transactional snapshot, classification-generation, evidence, and projection persistence.
- Create `internal/completion/repository_integration_test.go` — PostgreSQL snapshot/generation/projection tests.
- Create `internal/completion/projection.go` and `projection_test.go` — pure deterministic projection formula.
- Create `internal/completion/lexical.go` and `lexical_test.go` — versioned English-only conservative semantic fallback.
- Create `internal/completion/service.go` and `service_test.go` — classification/projection jobs and downstream rebuild orchestration.

### Synchronization and AI classification

- Modify `internal/sync/service.go`, `achievements.go`, and tests — schema fallback, recorder boundary, availability preservation, safe diagnostics.
- Modify `internal/ai/client.go`, `types.go`, `prompts.go`, `openai.go`, and tests — strict achievement semantic structured output.
- Modify `internal/app/app.go` and app tests — construct/register completion service and attach the sync recorder.
- Modify `internal/observability/events.go` and tests — allowlisted shared classification and user-owned projection stages/reasons without per-game or per-user identifiers.

### Profile, summaries, and recommendations

- Modify `internal/profile/vectors.go`, `repository.go`, `service.go`, and tests — completion signal, negative override, non-decaying weight, algorithm version.
- Modify `internal/ai/repository.go`, `repository_integration_test.go`, `prompts.go`, and `prompts_test.go` — deterministic capped evidence selector and completion-aware summary contract.
- Modify `internal/recommendations/types.go`, `scoring.go`, `retrieval.go`, `evidence.go`, and tests — `completionAffinity`, `completion_anchor`, no achievement double count, ranking/evidence version bumps.
- Modify `internal/httpapi/overview_contract_test.go` and ranking handler tests — public JSON compatibility.
- Modify `web/src/api/types.ts`, `client.ts`, and tests — factor/evidence contract and leak filtering.
- Modify `web/src/components/RecommendationCard.test.tsx` only if needed to pin rendering of `completion_anchor`; no new completion control is added.

### Lifecycle and end-to-end

- Modify `internal/httpapi/data_service_integration_test.go` — user cascade, shared-definition retention, deletion fencing.
- Modify `cmd/e2e-fakes/main.go` — localized achievements, schema fallback, and semantic-classification fixtures.
- Create `web/tests/e2e/completion-signals.spec.ts` and modify `Makefile` — end-to-end completion/profile/recommendation assertions included in `make verify`.
- Modify `README.md` and `docs/local-demo.md` — document automatic historical completion behavior and fallback diagnostics without exposing internals.

## Task Decomposition

### Task 1: Import Localized Achievement Metadata and Schema Fallback

**Files:**
- Modify: `internal/steam/types.go`
- Modify: `internal/steam/client.go`
- Modify: `internal/steam/http_client.go`
- Modify: `internal/steam/http_client_test.go`
- Modify: `internal/steam/testdata/achievements.json`
- Create: `internal/steam/testdata/achievement_schema.json`

**Interfaces:**
- Consumes: existing `HTTPClient.get`, `Availability`, resource-scoped cache metadata, and `Achievements(ctx, steamID, appID)` contract.
- Produces: `type Achievement struct { APIName, Name, Description string; Achieved bool; UnlockedAt time.Time }`; `AchievementSummary.Achievements []Achievement`; `type AchievementDefinition struct { APIName, Name, Description string }`; `type AchievementSchema struct { AppID int64; Achievements []AchievementDefinition }`; `Client.AchievementSchema(context.Context, int64) (AchievementSchema, Availability, error)`; `AchievementSchemaCacheKey(appID int64) CacheKey`.

- [ ] **Step 1: Extend the player-achievement contract test**

  Add `TestAchievementContracts/available localized metadata` asserting three stable unique API names, preserved English name/description, exact achieved flags and unlock times, `Total == 3`, `Unlocked == 2`, and request query `l=en`.

- [ ] **Step 2: Add failing malformed metadata cases**

  Extend `TestStructuredResponseValidation` with `achievement empty api name`, `achievement duplicate api name`, `achievement invalid achieved flag`, `locked achievement nonzero unlock time`, and `achievement trailing member`; assert `availability == RequestFailed`, `errors.Is(err, ErrInvalidResponse)`, and no cache metadata is published.

- [ ] **Step 3: Add the failing schema fallback client test**

  Add `TestAchievementSchemaContracts` asserting path `/ISteamUserStats/GetSchemaForGame/v2/`, `appid=620`, `l=en`, exact API-name/name/description parsing, app-scoped cache metadata, explicit unavailable handling, and rejection of duplicate/blank API names.

- [ ] **Step 4: Run the Steam tests to verify red**

  Run: `GOWORK=off go test -race ./internal/steam -run 'TestAchievement(Contracts|SchemaContracts)|TestStructuredResponseValidation' -count=1`

  Expected: FAIL to compile because `AchievementSummary.Achievements`, `AchievementSchema`, and `Client.AchievementSchema` do not exist.

- [ ] **Step 5: Add the achievement domain types and cache identity**

  Implement the exact produced types, add `AchievementSchemaOperation = "achievement_schema"`, and implement `func AchievementSchemaCacheKey(appID int64) CacheKey` using only the decimal AppID as the resource key.

- [ ] **Step 6: Parse and validate the primary player response**

  Keep `func (c *HTTPClient) Achievements(ctx context.Context, steamID string, appID int64) (AchievementSummary, Availability, error)` and populate the item slice only after validating the complete response; allow missing name/description for later fallback, but require unique nonblank API names and consistent achieved/unlock fields.

- [ ] **Step 7: Implement the schema endpoint**

  Add `func (c *HTTPClient) AchievementSchema(ctx context.Context, appID int64) (AchievementSchema, Availability, error)` using the Web API base, `GetSchemaForGame/v2`, `l=en`, the new cache key, and the same explicit availability/error rules as other Steam sources.

- [ ] **Step 8: Run the focused Steam package**

  Run: `GOWORK=off go test -race ./internal/steam -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 9: Commit the Steam boundary safely**

  ```bash
  git diff -- internal/steam/types.go internal/steam/client.go internal/steam/http_client.go internal/steam/http_client_test.go internal/steam/testdata/achievements.json internal/steam/testdata/achievement_schema.json
  git diff --no-index -- /dev/null internal/steam/testdata/achievement_schema.json || test $? -eq 1
  git add -p -- internal/steam/types.go internal/steam/client.go internal/steam/http_client.go internal/steam/http_client_test.go internal/steam/testdata/achievements.json
  git add -- internal/steam/testdata/achievement_schema.json
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: import achievement metadata"
  ```

### Task 2: Add the Completion Persistence Schema and Generated Queries

**Files:**
- Create: `db/migrations/00015_achievement_completion_signals.sql`
- Create: `db/queries/achievement_completion.sql`
- Modify: `internal/store/migration_upgrade_test.go`
- Modify: `internal/store/store_test.go`
- Generate: `internal/store/sqlc/achievement_completion.sql.go`
- Generate: `internal/store/sqlc/models.go`

**Interfaces:**
- Consumes: migrations `00001`–`00014`, `users`, `games`, `achievement_summaries`, and sqlc configuration.
- Produces: `game_achievement_definitions`, `user_achievement_unlocks`, `achievement_semantic_classification_generations`, `achievement_semantic_classifications`, `user_game_completion_evidence`, and `user_game_completion_projections`; sqlc methods named in `db/queries/achievement_completion.sql` for later repository tasks.

- [ ] **Step 1: Start the isolated PostgreSQL test service**

  Run: `make wait-test-db`

  Expected: Compose reports `test-db` healthy on `127.0.0.1:55432`.

- [ ] **Step 2: Write the failing migration upgrade test**

  Add `TestAchievementCompletionMigrationBackfillsHundredPercentAndConstraints`: migrate an isolated schema through version 14, seed one `10/10` and one `9/10` summary, migrate up, and assert exactly one `hundred_percent` evidence row and one projection with `weight == 1.50`, `ending_count == 0`, `basis == "hundred_percent"`, and null `completion_at`; assert invalid weights above `2.25`, invalid bases/classes/statuses/sources, blank lexical rule versions, and malformed ending evidence violate constraints.

- [ ] **Step 3: Run the migration test to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/store -run TestAchievementCompletionMigrationBackfillsHundredPercentAndConstraints -count=1`

  Expected: FAIL because migration 15 and its tables do not exist.

- [ ] **Step 4: Create migration 15 with exact ownership and checks**

  Make shared definition/generation/classification rows reference `games(id) ON DELETE CASCADE`; make unlock/evidence/projection rows also reference `users(id) ON DELETE CASCADE`; persist generation `lexical_rule_version` and result `classification_source` (`reused|lexical|openai|unknown`) plus rule version; constrain semantic classes, generation states `staged|active|superseded`, evidence kinds `hundred_percent|ending_marker`, projection bases `hundred_percent|ending|both`, nonnegative counts, and weight `0 < weight AND weight <= 2.25`; add the partial unique active-generation index and the deterministic legacy 100% backfill.

- [ ] **Step 5: Define sqlc query names used by later tasks**

  Add `AdvanceAchievementSnapshot`, `UpsertAchievementDefinition`, `UpsertAchievementUnlock`, `MarkAchievementUnlocksAbsent`, `GetCurrentAchievementDefinitionSet`, `GetReusableAchievementClassifications`, `CreateClassificationGeneration`, `StageAchievementClassification`, `ActivateClassificationGeneration`, `GetActiveAchievementClassifications`, `InsertCompletionEvidence`, `GetCompletionProjectionInput`, `UpsertCompletionProjection`, `DeleteCompletionProjection`, and `ListCompletionUsersForGame` with deterministic `ORDER BY` clauses.

- [ ] **Step 6: Generate bindings**

  Run: `make sqlc`

  Expected: sqlc exits zero and produces bindings without manual edits.

- [ ] **Step 7: Run store migration and schema tests**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/store -run 'TestAchievementCompletionMigrationBackfillsHundredPercentAndConstraints|TestSchemaTablesExist' -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 8: Commit the schema and generated bindings safely**

  ```bash
  git diff -- db/migrations/00015_achievement_completion_signals.sql db/queries/achievement_completion.sql internal/store/migration_upgrade_test.go internal/store/store_test.go internal/store/sqlc/achievement_completion.sql.go internal/store/sqlc/models.go
  git diff --no-index -- /dev/null db/migrations/00015_achievement_completion_signals.sql || test $? -eq 1
  git diff --no-index -- /dev/null db/queries/achievement_completion.sql || test $? -eq 1
  git diff --no-index -- /dev/null internal/store/sqlc/achievement_completion.sql.go || test $? -eq 1
  git add -- db/migrations/00015_achievement_completion_signals.sql db/queries/achievement_completion.sql internal/store/sqlc/achievement_completion.sql.go
  git add -p -- internal/store/migration_upgrade_test.go internal/store/store_test.go internal/store/sqlc/models.go
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: persist achievement completion schema"
  ```

### Task 3: Persist Detailed Snapshots and Integrate Schema Fallback

**Files:**
- Create: `internal/completion/types.go`
- Create: `internal/completion/repository.go`
- Create: `internal/completion/repository_integration_test.go`
- Modify: `internal/sync/service.go`
- Modify: `internal/sync/service_test.go`
- Modify: `internal/sync/achievements.go`
- Modify: `internal/sync/achievements_test.go`

**Interfaces:**
- Consumes: Task 1 `steam.AchievementSummary`/`AchievementSchema`; Task 2 generated queries.
- Produces: `type SnapshotChange struct { DefinitionSetHash string; DefinitionsChanged bool; HundredPercentObserved bool }`; `func NewSQLRepository(*store.Store) (*SQLRepository, error)`; `func (r *SQLRepository) SaveSteamSnapshot(context.Context, uuid.UUID, uuid.UUID, steam.AchievementSummary, time.Time) (SnapshotChange, error)`; sync-local `type AchievementSnapshotRecorder interface { RecordAchievementSnapshot(context.Context, uuid.UUID, uuid.UUID, steam.AchievementSummary, time.Time) error }`; `func (s *Service) SetAchievementSnapshotRecorder(AchievementSnapshotRecorder)`.

- [ ] **Step 1: Write the failing transactional snapshot tests**

  Add `TestSaveSteamSnapshotAdvancesGenerationAndPreservesEverUnlocked` asserting current `present_in_latest`/`achieved_in_latest` reflect the second snapshot, a removed prior unlock retains `ever_achieved=true`, aggregate summary counts only current rows, definition hashes are deterministic across input order, and replay does not duplicate rows or evidence.

- [ ] **Step 2: Write the failing schema-fallback preservation test from Review Focus 2**

  Add `TestAchievementSyncSchemaFallbackFailurePreservesPlayerSnapshotAndDefinitions`: seed prior definition text, return a valid player response missing metadata, fail `AchievementSchema`, and assert the new unlock snapshot/summary commits, prior definition text/hash remain unchanged, availability records player success plus schema failure, and no empty definition overwrites prior state.

- [ ] **Step 3: Write the failing unavailable-player preservation test**

  Add `TestAchievementSyncUnavailablePreservesDetailedSnapshot` asserting private/request-failed/not-modified player responses do not alter definitions, current-generation flags, `ever_achieved`, aggregate summary, or raw snapshot.

- [ ] **Step 4: Run focused DB tests to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/completion ./internal/sync -run 'TestSaveSteamSnapshot|TestAchievementSyncSchemaFallbackFailure|TestAchievementSyncUnavailable' -count=1`

  Expected: FAIL because `internal/completion`, the recorder boundary, and detailed snapshot persistence do not exist.

- [ ] **Step 5: Implement normalized snapshot hashing and persistence**

  Implement `SaveSteamSnapshot` as one database transaction: lock the user-game, increment snapshot generation, mark old rows non-current, upsert definitions only from nonblank metadata, monotonically OR `ever_achieved`, upsert the aggregate summary, append 100% evidence when applicable, and return the canonical sorted definition-set hash.

- [ ] **Step 6: Add schema fallback to `syncAchievements`**

  On a valid player snapshot with any blank name/description, call `AchievementSchema`, record app-scoped source state, merge only exact API-name matches, retain player availability independently, then pass the merged snapshot to the recorder; do not expose AppID or achievement data in aggregate logs.

- [ ] **Step 7: Preserve compatibility when no recorder is configured**

  Keep the current aggregate `UpsertAchievementSummary` path as the nil-recorder fallback for narrowly constructed tests, while production wiring in Task 6 must always install the completion service.

- [ ] **Step 8: Run completion and synchronization tests**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/completion ./internal/sync -count=1`

  Expected: PASS with zero failures and existing concurrency/order tests unchanged.

- [ ] **Step 9: Commit snapshot persistence safely**

  ```bash
  git diff -- internal/completion/types.go internal/completion/repository.go internal/completion/repository_integration_test.go internal/sync/service.go internal/sync/service_test.go internal/sync/achievements.go internal/sync/achievements_test.go
  git diff --no-index -- /dev/null internal/completion/types.go || test $? -eq 1
  git diff --no-index -- /dev/null internal/completion/repository.go || test $? -eq 1
  git diff --no-index -- /dev/null internal/completion/repository_integration_test.go || test $? -eq 1
  git add -- internal/completion/types.go internal/completion/repository.go internal/completion/repository_integration_test.go
  git add -p -- internal/sync/service.go internal/sync/service_test.go internal/sync/achievements.go internal/sync/achievements_test.go
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: preserve detailed achievement snapshots"
  ```

### Task 4: Add Strict OpenAI Semantics and the Conservative Lexical Classifier

**Files:**
- Modify: `internal/ai/client.go`
- Modify: `internal/ai/types.go`
- Modify: `internal/ai/prompts.go`
- Modify: `internal/ai/prompts_test.go`
- Modify: `internal/ai/openai.go`
- Create: `internal/ai/achievement_semantics_test.go`
- Create: `internal/completion/lexical.go`
- Create: `internal/completion/lexical_test.go`

**Interfaces:**
- Consumes: existing `OpenAI.structured`, `ContentHash`, strict schema helpers, and safe classified errors.
- Produces: `type AchievementSemanticClass string` with the eight spec enums; `type AchievementSemanticItem struct { EvidenceKey, APIName, Name, Description string }`; `type AchievementSemanticEvidence struct { EvidenceKey, GameName string; Achievements []AchievementSemanticItem }`; `type AchievementSemanticResult struct { EvidenceKey string; Class AchievementSemanticClass }`; `type AchievementSemanticClassification struct { Results []AchievementSemanticResult }`; `Client.ClassifyAchievementSemantics(context.Context, AchievementSemanticEvidence) (AchievementSemanticClassification, error)`; `func ValidateAchievementSemanticClassification(AchievementSemanticClassification, []string) error`; `completion.LexicalRuleVersion = "lexical-completion-v1"`; `func ClassifyLexically(name, description string) (ai.AchievementSemanticClass, bool)`.

- [ ] **Step 1: Write the failing strict-schema and privacy test**

  Add `TestAchievementSemanticClassifierUsesOnlySharedMetadata`: capture the Responses request and assert the input contains game name, opaque keys, API names, English names/descriptions, and no SteamID, UUID, unlock flag/time, playtime, feedback, profile, numeric weight, or raw body; assert format name `achievement_semantics_v1` and `strict == true`.

- [ ] **Step 2: Write the failing validation table test**

  Add `TestValidateAchievementSemanticClassification` covering all eight valid classes and asserting missing/duplicate/unknown evidence keys, unknown classes, nil results, unknown JSON fields, and trailing JSON return `ErrInvalidOutput` or `ErrInvalidEvidence` without echoing untrusted values.

- [ ] **Step 3: Run the classifier tests to verify red**

  Run: `GOWORK=off go test -race ./internal/ai -run 'TestAchievementSemantic' -count=1`

  Expected: FAIL to compile because the semantic input/output types and client method are undefined.

- [ ] **Step 4: Write the failing lexical phrase and exclusion table**

  Add `TestClassifyLexicallyConservativeRules`: assert exact/narrow English forms equivalent to `complete/finish the game/main game/main story/story/campaign` return `base_game_completion`; `reach an/the ending` and `reach the epilogue` return `base_game_ending`; case, punctuation, and whitespace normalize deterministically; API-name-only hints, non-English text, vague `done`, and unrelated text return `ok=false`; any chapter/act/mission/level, DLC/expansion, NG+/New Game Plus, difficulty, speedrun, collection, all/multiple endings, or meta wording vetoes a positive phrase; no case returns a weight.

- [ ] **Step 5: Run the lexical test to verify red**

  Run: `GOWORK=off go test -race ./internal/completion -run TestClassifyLexicallyConservativeRules -count=1`

  Expected: FAIL because `LexicalRuleVersion` and `ClassifyLexically` do not exist.

- [ ] **Step 6: Add the versioned prompt and types**

  Define `AchievementSemanticsPromptVersion = "achievement-semantics-v1"`, `AchievementSemanticsSchemaVersion = "achievement-semantics-structured-output-v1"`, the exact enum values from the spec, and instructions that classify supplied metadata only and return one result per opaque key without confidence or ranking values.

- [ ] **Step 7: Implement strict normalization, schema, call, and validation**

  Add `func (client *OpenAI) ClassifyAchievementSemantics(ctx context.Context, input AchievementSemanticEvidence) (AchievementSemanticClassification, error)`; normalize bounded nonblank shared metadata, sort by evidence key, build an enum schema whose keys are limited to the supplied set, call `structured`, and validate one-to-one coverage.

- [ ] **Step 8: Implement `ClassifyLexically`**

  In `internal/completion/lexical.go`, normalize English text with a fixed punctuation/whitespace/case transform, evaluate the versioned exclusion table first, then only the anchored phrase table; return `(qualifyingClass, true)` for a match and `(ai.AchievementSemanticUnknown, false)` otherwise.

- [ ] **Step 9: Run the AI and completion packages**

  Run: `GOWORK=off go test -race ./internal/ai ./internal/completion -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 10: Commit the classifier contracts safely**

  ```bash
  git diff -- internal/ai/client.go internal/ai/types.go internal/ai/prompts.go internal/ai/prompts_test.go internal/ai/openai.go internal/ai/achievement_semantics_test.go internal/completion/lexical.go internal/completion/lexical_test.go
  git diff --no-index -- /dev/null internal/ai/achievement_semantics_test.go || test $? -eq 1
  git diff --no-index -- /dev/null internal/completion/lexical.go || test $? -eq 1
  git diff --no-index -- /dev/null internal/completion/lexical_test.go || test $? -eq 1
  git add -p -- internal/ai/client.go internal/ai/types.go internal/ai/prompts.go internal/ai/prompts_test.go internal/ai/openai.go
  git add -- internal/ai/achievement_semantics_test.go internal/completion/lexical.go internal/completion/lexical_test.go
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: classify achievement semantics"
  ```

### Task 5: Publish Semantic Classifications as Atomic Generations

**Files:**
- Modify: `internal/completion/types.go`
- Modify: `internal/completion/repository.go`
- Modify: `internal/completion/repository_integration_test.go`
- Create: `internal/completion/service.go`
- Create: `internal/completion/service_test.go`
- Modify: `internal/observability/events.go`
- Modify: `internal/observability/events_test.go`

**Interfaces:**
- Consumes: Task 3 repository and definition-set hash; Task 4 `SemanticClassifier`, `ClassifyLexically`, and `LexicalRuleVersion`.
- Produces: `const ClassificationJobKind = "achievement.classify"`; `type ClassificationSource string` with `reused|lexical|openai|unknown`; `type SemanticClassifier interface { ClassifyAchievementSemantics(context.Context, ai.AchievementSemanticEvidence) (ai.AchievementSemanticClassification, error) }`; `type JobPayload struct { Version int; GameID uuid.UUID; DefinitionSetHash string }`; `func NewService(*SQLRepository, SemanticClassifier, string, *jobs.Queue) (*Service, error)`; `func (s *Service) PlanClassificationJob(context.Context, uuid.UUID, string, int) (jobs.Spec, error)` returning a spec with `UserID == uuid.Nil`; `func (s *Service) RegisterHandlers(*jobs.Worker) error`; `func (s *Service) SetClassificationPublishedHook(func(context.Context, uuid.UUID) error)` where the hook argument is only the shared game ID.

- [ ] **Step 1: Write the failing atomic-generation test from Review Focus 1**

  Add `TestClassificationGenerationActivatesAtomicallyAndPreservesLastGood`: seed an active two-definition generation, stage reused and lexical results for a new three-definition generation, then simulate invalid OpenAI output/failure for the unresolved result; assert the old generation remains the sole active one, no partial result is returned by current queries, no failure-derived unknown generation activates, and the saved completion projection is unchanged.

- [ ] **Step 2: Write the failing successful publication test**

  Add `TestClassificationGenerationPublishesCompleteSetOnce`: seed one unchanged reusable definition, one lexical completion phrase, and one unresolved definition whose OpenAI result arrives in reverse order, then replay the same job; assert exactly one active generation, three immutable rows with sources `reused`, `lexical`, and `openai`, persisted `lexical-completion-v1`, old generation marked `superseded`, one game-only published-hook call, and no duplicate rows/jobs.

- [ ] **Step 3: Write the failing unavailable-provider first-generation test from Review Focus 1**

  Add `TestClassificationUnavailablePublishesLocalFirstGenerationOnly`: with no active generation, seed one reusable result, one lexical match, and one unresolved definition, return `ai.ErrCapabilityDisabled`, and assert a complete active generation publishes sources `reused`, `lexical`, `unknown`, invokes the game-only hook once, and exposes the lexical qualifying class; repeat with an existing active generation plus a changed unresolved definition and assert provider disabled/temporary failure publishes nothing, leaves the old generation/projection active, and does not invoke the hook.

- [ ] **Step 4: Write the failing shared-job ownership test**

  Add `TestClassificationJobIsSharedAndGameScoped`: assert encoded `JobPayload` contains only version, game ID, and definition-set hash; its `jobs.Spec.UserID == uuid.Nil`; identical game/hash/classifier/rule inputs from two users yield one idempotency key; and neither payload nor result contains either user ID.

- [ ] **Step 5: Run focused completion tests to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/completion -run 'TestClassification' -count=1`

  Expected: FAIL because classification generation repository/service methods do not exist.

- [ ] **Step 6: Implement generation loading and deterministic reuse**

  Add repository methods `LoadDefinitionGenerationInput(ctx, gameID, model, promptVersion, schemaVersion, lexicalRuleVersion) (ai.AchievementSemanticEvidence, map[string]StoredClassification, string, error)` and `PublishClassificationGeneration(ctx, gameID, model, promptVersion, schemaVersion, lexicalRuleVersion, definitionSetHash string, results []StagedClassification, now time.Time) (bool, error)`; reuse only identical definition content hashes under unchanged classifier/rule identities, persist each result's source/rule version, and flip active/superseded states in one transaction only when the complete expected hash/key set still matches.

- [ ] **Step 7: Implement the idempotent shared classification job**

  Build the logical key from game ID, definition-set hash, model, prompt version, schema version, and lexical rule version with no user identity; merge unchanged reuse, lexical matches, and validated OpenAI results. On unavailable OpenAI, activate explicit unknowns only when no active generation exists; otherwise preserve the active generation and retry temporary errors. Invoke the game-only hook only after a newly active complete generation commits.

- [ ] **Step 8: Add safe shared-job observability**

  Allow `achievement.classify` and logical stage `achievement_classification` in existing job-event validation; tests assert aggregate kind/stage/reason only and reject AppID, API name, description, prompt, and provider body canaries.

- [ ] **Step 9: Run completion and observability tests**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/completion ./internal/observability -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 10: Commit atomic classification generations safely**

  ```bash
  git diff -- internal/completion/types.go internal/completion/repository.go internal/completion/repository_integration_test.go internal/completion/service.go internal/completion/service_test.go internal/observability/events.go internal/observability/events_test.go
  git diff --no-index -- /dev/null internal/completion/service.go || test $? -eq 1
  git diff --no-index -- /dev/null internal/completion/service_test.go || test $? -eq 1
  git add -- internal/completion/service.go internal/completion/service_test.go
  git add -p -- internal/completion/types.go internal/completion/repository.go internal/completion/repository_integration_test.go internal/observability/events.go internal/observability/events_test.go
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: publish atomic achievement classifications"
  ```

### Task 6: Project Persistent Completion and Orchestrate Rebuilds

**Files:**
- Create: `internal/completion/projection.go`
- Create: `internal/completion/projection_test.go`
- Modify: `internal/completion/repository.go`
- Modify: `internal/completion/repository_integration_test.go`
- Modify: `internal/completion/service.go`
- Modify: `internal/completion/service_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/sync/service.go`
- Modify: `internal/sync/service_test.go`
- Modify: `internal/observability/events.go`
- Modify: `internal/observability/events_test.go`

**Interfaces:**
- Consumes: Task 3 snapshot recorder boundary and Task 5 game-only classification-published hook plus shared-generation repository.
- Produces: `const ProjectionAlgorithmVersion = "completion-v1"`; `const ProjectionJobKind = "completion.project"`; `type ProjectionInput struct { EverHundredPercent bool; HundredPercentAt time.Time; EndingUnlocks []time.Time }`; `type Projection struct { EndingCount int; HundredPercentBasis bool; Weight float64; CompletionAt time.Time; Basis string }`; `func CalculateProjection(ProjectionInput) (Projection, bool)`; `func (r *SQLRepository) RebuildProjection(context.Context, uuid.UUID, uuid.UUID, string, time.Time) (Projection, bool, error)`; `func (r *SQLRepository) ListCompletionUsersForGame(context.Context, uuid.UUID) ([]uuid.UUID, error)`; `func (s *Service) EnqueueProjectionJobsForGame(context.Context, uuid.UUID) error`; `func (s *Service) RecordAchievementSnapshot(context.Context, uuid.UUID, uuid.UUID, steam.AchievementSummary, time.Time) error`.

- [ ] **Step 1: Write the failing projection formula table**

  Add `TestCalculateProjectionWeights` with exact assertions: none returns `ok=false`; 100%-only and one ending `1.50`; two `1.85`; three `2.05`; four `2.15`; five and fifty `2.25`; 100% plus one uses `1.50`; 100% plus two uses `1.85`; ending-only and `both` use the earliest known qualifying ending unlock; if endings exist but every ending date is unknown, `CompletionAt` is zero even when `HundredPercentAt` is known; only 100%-without-endings uses known `HundredPercentAt`; all unknown dates remain zero.

- [ ] **Step 2: Write the failing historical-100% test from Review Focus 3**

  Add `TestProjectCompletionRetainsHistoricalHundredPercentAfterCatalogGrowth`: save `10/10`, then a successful `10/12` snapshot with no qualifying endings, rebuild twice, and assert `basis == "hundred_percent"`, `weight == 1.50`, one durable 100% evidence row, and no recency decay or duplicate evidence.

- [ ] **Step 3: Write the failing semantic evidence filter test**

  Add `TestRebuildProjectionCountsDistinctUnlockedBaseGameEndingsOnly`: seed duplicate/replayed API names plus locked, DLC, meta, partial, optional, other, and unknown classes; assert only distinct `ever_achieved` base-game ending/completion rows count and the exact weight matches their count.

- [ ] **Step 4: Write the failing shared-classification fan-out test**

  Add `TestClassificationActivationFansOutUserOwnedProjectionJobs`: seed two existing users with observations for one game plus an unrelated user, invoke the Task 5 game-only hook, and assert exactly two `completion.project` specs, each with its own nonzero `UserID`, deletion-fenced ownership, and idempotency key derived from user/game/generation/projection version; replay creates no duplicates and the shared `achievement.classify` job remains unowned.

- [ ] **Step 5: Write the failing rebuild-orchestration test**

  Add `TestProjectionJobRebuildsProfileThenRecommendationsOnce`: after a changed projection, assert a user-owned `profile.rebuild` child completes before `recommendations.rebuild` is created, replay is idempotent, unchanged projection creates no rebuilds, and projection failure keeps the prior projection/profile/ranking.

- [ ] **Step 6: Write the failing projection observability privacy test**

  Add `TestCompletionProjectionJobObservabilityIsAllowlistedAndPrivate`: assert `completion.project` and stage `completion_projection` pass allowlist validation with aggregate status/reason, while SteamID, user/game UUID, AppID, API name, achievement description, job payload, numeric completion weight, and provider-body canaries are rejected or absent.

- [ ] **Step 7: Run projection tests to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/completion ./internal/observability -run 'TestCalculateProjection|TestProjectCompletion|TestRebuildProjection|TestProjectionJob|TestClassificationActivationFansOut|TestCompletionProjectionJobObservability' -count=1`

  Expected: FAIL because projection calculation, repository publication, and projection jobs do not exist.

- [ ] **Step 8: Implement the pure projection formula**

  Deduplicate ending inputs before constructing `ProjectionInput`, calculate the exact section-6 formula with cap `2.25`, choose basis `hundred_percent|ending|both`, and set the date by strict precedence: earliest known ending date whenever endings exist, otherwise known 100% date, otherwise zero; never read playtime or last-played timestamps.

- [ ] **Step 9: Implement versioned projection publication**

  `RebuildProjection` reads durable 100% evidence plus historically unlocked rows joined only to the complete active configured semantic generation, appends normalized ending evidence idempotently, and transactionally upserts a positive projection or removes a now-invalid semantic-only projection after an authoritative complete reclassification; failed/pending generations leave the prior projection untouched.

- [ ] **Step 10: Wire snapshot and shared-classification changes to user projection jobs**

  `RecordAchievementSnapshot` calls Task 3 persistence, synchronously publishes the deterministic 100% projection before the sync profile stage, and enqueues the unowned game-scoped classification job by definition-set hash. `EnqueueProjectionJobsForGame` loads existing affected users only after activation and enqueues one user-owned, deletion-fenced projection job per user; no user identifier enters the classification payload or key.

- [ ] **Step 11: Chain changed projections through profile then recommendations**

  Persist a fenced pending result before deferral, use `AwaitChildren` first for `profile.rebuild` and then for `recommendations.rebuild`, derive both idempotency keys from user, game, projection algorithm/version/hash, and never let the recommendation child race the profile child.

- [ ] **Step 12: Allowlist private projection observability**

  Add `completion.project` and logical stage `completion_projection` to the existing observability allowlists, emitting only aggregate kind/stage/status/safe reason and never payload or identifiers.

- [ ] **Step 13: Wire production services**

  In `internal/app/app.go`, construct one completion repository/service, register its handlers, set its published hook, and pass it to `syncService.SetAchievementSnapshotRecorder`; keep the OpenAI client behind the local `SemanticClassifier` interface.

- [ ] **Step 14: Run completion, sync, observability, and app tests**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/completion ./internal/sync ./internal/observability ./internal/app -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 15: Commit projection and orchestration safely**

  ```bash
  git diff -- internal/completion/projection.go internal/completion/projection_test.go internal/completion/repository.go internal/completion/repository_integration_test.go internal/completion/service.go internal/completion/service_test.go internal/app/app.go internal/app/app_test.go internal/sync/service.go internal/sync/service_test.go internal/observability/events.go internal/observability/events_test.go
  git diff --no-index -- /dev/null internal/completion/projection.go || test $? -eq 1
  git diff --no-index -- /dev/null internal/completion/projection_test.go || test $? -eq 1
  git add -- internal/completion/projection.go internal/completion/projection_test.go
  git add -p -- internal/completion/repository.go internal/completion/repository_integration_test.go internal/completion/service.go internal/completion/service_test.go internal/app/app.go internal/app/app_test.go internal/sync/service.go internal/sync/service_test.go internal/observability/events.go internal/observability/events_test.go
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: project persistent game completion"
  ```

### Task 7: Weight Completion in Preference Profiles with Explicit Overrides

**Files:**
- Modify: `internal/profile/vectors.go`
- Modify: `internal/profile/vectors_test.go`
- Modify: `internal/profile/repository.go`
- Modify: `internal/profile/service.go`
- Modify: `internal/profile/service_test.go`
- Modify: `internal/feedback/repository_integration_test.go`
- Modify: `internal/feedback/service_test.go`

**Interfaces:**
- Consumes: Task 6 `user_game_completion_projections.weight` and existing latest-direct feedback ordering.
- Produces: `profile.Signal.CompletionWeight float64`; `profile.Signal.CompletionOverridden bool`; `profile.SignalWeights.Completion float64`; `profile.AlgorithmVersion = "profile-v4-achievement-completion"`; completion projection algorithm identity in saved profile metadata/build identity.

- [ ] **Step 1: Extend the failing signal-weight table**

  Add cases to `TestCalculateSignalWeights`: completion-only at any age yields `Completion=1.50, Positive=1.50`; completion `1.85` plus achievement ratio `1` yields `Completion=1.85, Achievement=0`; completion plus max playtime/recent/like remains additive; `CompletionOverridden=true` yields `Completion=0` while existing `Negative` remains.

- [ ] **Step 2: Add the failing latest-action integration test from Review Focus 4**

  Add `TestLoadSignalsLatestDirectActionOverridesCompletionOnly`: seed a projection, historical dislike, later like, then later dislike/exclude variants; assert latest negative sets only `CompletionOverridden`, later like restores the saved completion weight, historical dislike count still builds negative weight, and the projection row never changes.

- [ ] **Step 3: Add the failing profile version/build test**

  Extend profile service tests to assert `AlgorithmVersion == "profile-v4-achievement-completion"`, metadata includes `completionAlgorithmVersion == "completion-v1"`, and a completion-version build cannot reuse a legacy profile build.

- [ ] **Step 4: Run profile tests to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/profile ./internal/feedback -run 'TestCalculateSignalWeights|TestLoadSignalsLatestDirectActionOverridesCompletionOnly|Test.*Profile.*Version|TestClassificationDoesNotDuplicateDirectGameSignals|TestLikeRaisesRelatedCandidate' -count=1`

  Expected: FAIL because completion fields and profile-v4 behavior are absent.

- [ ] **Step 5: Load projection and latest-direct override state**

  Extend `LoadSignals` with a deterministic `latest_direct` CTE, left join completion projection, scan saved weight separately from `CompletionOverridden`, and retain historical like/dislike counts for their existing positive/negative paths.

- [ ] **Step 6: Implement completion weighting without double count or decay**

  In `calculateSignalWeight`, clamp saved completion to `0..2.25`; when positive and not overridden set `Completion` and add it to `Positive`, skip `Achievement`; otherwise retain existing recency-adjusted achievement behavior; add completion as the sixth confidence coverage kind.

- [ ] **Step 7: Bump profile version and metadata**

  Save `completionAlgorithmVersion: completion-v1` with profile metadata and ensure rebuild idempotency includes the new algorithm identity without changing legacy profile reads.

- [ ] **Step 8: Run profile and feedback packages**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/profile ./internal/feedback -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 9: Commit profile weighting safely**

  ```bash
  git diff -- internal/profile/vectors.go internal/profile/vectors_test.go internal/profile/repository.go internal/profile/service.go internal/profile/service_test.go internal/feedback/repository_integration_test.go internal/feedback/service_test.go
  git add -p -- internal/profile/vectors.go internal/profile/vectors_test.go internal/profile/repository.go internal/profile/service.go internal/profile/service_test.go internal/feedback/repository_integration_test.go internal/feedback/service_test.go
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: weight completed games in profiles"
  ```

### Task 8: Protect Recent and Completion Profile-Summary Evidence

**Files:**
- Modify: `internal/ai/repository.go`
- Modify: `internal/ai/repository_integration_test.go`
- Modify: `internal/ai/prompts.go`
- Modify: `internal/ai/prompts_test.go`
- Modify: `internal/ai/service_test.go`

**Interfaces:**
- Consumes: Task 6 completion projection and Task 7 latest-direct override semantics.
- Produces: package-private `type profileEvidenceSignal`; `func selectProfileEvidence([]profileEvidenceSignal, int) []profileEvidenceSignal`; `ProfilePromptVersion = "profile-summary-v4-completion"`; evidence fields `completion_basis`, `ending_count`, `completion_at`, and `completion_overridden`, with no numeric completion weight.

- [ ] **Step 1: Replace the clipping test with the failing Review Focus 5 case**

  Add `TestLoadProfileEvidenceProtectsTwoWeekActivityAndCompletionFromLimit`: seed more than 100 rows including positive completion, overridden completion, positive Steam two-week activity, standalone explicit feedback, wishlist, and history; assert exactly 100 items and ordered tier indexes `completion/overridden completion < recent < other explicit feedback < wishlist < history`, both completion facts retain basis/count/date/override state, the overridden item cannot be positive prose, at least one low-priority row is absent, order is stable across two reads, and no `completion_weight` text appears.

- [ ] **Step 2: Add the failing prompt contract test**

  Extend `TestProfilePromptContract` to assert version `profile-summary-v4-completion` and instructions containing `recent_minutes is Steam two-week activity`, `completion is historical`, `low playtime does not weaken completion`, `overridden completion is not positive`, and `unknown dates do not imply recency`.

- [ ] **Step 3: Run focused AI tests to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/ai -run 'TestLoadProfileEvidenceProtectsTwoWeekActivityAndCompletionFromLimit|TestProfilePromptContract|TestProfileSummaryIsNormalizedBeforePersistence' -count=1`

  Expected: FAIL because SQL still clips before Go selection and completion evidence/version fields are absent.

- [ ] **Step 4: Load the full bounded relevant set without semantic SQL clipping**

  Remove the initial `LIMIT 100`, include latest explicit positive/negative action and completion projection in `relevant`, and scan into package-private `profileEvidenceSignal` values before selection.

- [ ] **Step 5: Implement the single deterministic selector**

  Deduplicate by game, classify priority as positive or overridden completion first, positive Steam two-week activity second, explicit feedback not already represented third, wishlist fourth, then remaining history; within a tier sort by material value descending, `last_played_at` descending/null-last, normalized name, raw name, UUID; cap at 100 only after sorting and preserve override state on completion items.

- [ ] **Step 6: Serialize safe completion-aware evidence and bump the prompt hash**

  Add basis/count/date/override fields, omit numeric weight and internal IDs, keep unknown dates explicit, and rely on `ProfilePromptVersion` in the existing `ContentHash` so old summaries are not reused.

- [ ] **Step 7: Run the AI package**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/ai -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 8: Commit the evidence selector fix safely**

  ```bash
  git diff -- internal/ai/repository.go internal/ai/repository_integration_test.go internal/ai/prompts.go internal/ai/prompts_test.go internal/ai/service_test.go
  git add -p -- internal/ai/repository.go internal/ai/repository_integration_test.go internal/ai/prompts.go internal/ai/prompts_test.go internal/ai/service_test.go
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "fix: protect recent and completion profile evidence"
  ```

### Task 9: Add Completion Affinity, Grounded Evidence, and Public Contracts

**Files:**
- Modify: `internal/recommendations/types.go`
- Modify: `internal/recommendations/scoring.go`
- Modify: `internal/recommendations/scoring_test.go`
- Modify: `internal/recommendations/retrieval.go`
- Modify: `internal/recommendations/repository_integration_test.go`
- Modify: `internal/recommendations/evidence.go`
- Modify: `internal/recommendations/evidence_test.go`
- Modify: `internal/recommendations/presentation_test.go`
- Modify: `internal/httpapi/overview_contract_test.go`
- Modify: `internal/httpapi/ranking_handlers_test.go`
- Modify: `web/src/api/types.ts`
- Modify: `web/src/api/client.ts`
- Modify: `web/src/api/client.test.ts`
- Modify: `web/src/components/RecommendationCard.test.tsx`

**Interfaces:**
- Consumes: Task 7 `SignalWeights.Completion`/override and Task 8 safe prose rules.
- Produces: `Factors.CompletionAffinity float64` with JSON `completionAffinity`; `Weights.CompletionAffinity float64` default `5`; `EvidenceCompletionAnchor = "completion_anchor"`; `StoredEvidenceVersion = 3`; `AlgorithmVersion = "ranking-v4-achievement-completion"`; package-private `type affinities struct { Tag, Playtime, Achievement, Recent, Completion float64 }`; matching TypeScript factor/evidence members.

- [ ] **Step 1: Write the failing scoring/version test**

  Rename the current default-weight test to `TestScoreUsesAchievementCompletionWeights`; assert completion affinity `1` contributes exactly `5`, all-default positive factors now total `90`, default completion weight is `5`, and algorithm version is `ranking-v4-achievement-completion`; retain legacy JSON zero-value tests.

- [ ] **Step 2: Write the failing affinity integration test**

  Add `TestCompletionAffinityIsPersistentAndDoesNotDoubleCountAchievement`: seed a completed matching anchor, an ordinary achievement-progress anchor, an overridden completed anchor, and a ten-year-old completed anchor; assert completed rows contribute completion but not achievement affinity, overridden rows contribute neither positive affinity, ordinary progress remains achievement affinity, and old/new completion contributions are equal.

- [ ] **Step 3: Write the failing grounded-evidence test**

  Extend `TestBuildGroundingEvidenceUsesConcreteCandidateContext` with a completed source and assert one `completion_anchor` with sanitized Russian historical-completion wording, no weight/API name/achievement text, no overridden source, and deterministic tie order; assert stored evidence version `3` and legacy v2 reads remain safe.

- [ ] **Step 4: Write failing Go/TypeScript contract tests**

  Update `TestOverviewJSONContract` and `web/src/api/client.test.ts` to require `completionAffinity` and accept sanitized `completion_anchor`; assert malformed/unknown evidence remains dropped and `completionAffinity`/`completionAffinityWeight` text is rejected from public prose.

- [ ] **Step 5: Run focused backend and frontend tests to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/recommendations ./internal/httpapi -run 'TestScoreUsesAchievementCompletionWeights|TestCompletionAffinity|TestBuildGroundingEvidence|TestOverviewJSONContract' -count=1`

  Expected: FAIL because completion factor/evidence fields are undefined.

- [ ] **Step 6: Add deterministic completion scoring and retrieval**

  Replace the five-value affinity tuple with `affinities`; join completion projections and latest direct action; normalize completion overlap by eligible saved completion weight without recency; exclude completed rows from achievement affinity; assign the new factor and score weight while preserving existing deterministic candidate ordering.

- [ ] **Step 7: Add completion grounding and persistence compatibility**

  Build `completion_anchor` only from `SignalWeights.Completion > 0`, assign its contribution from the new factor/weight, place its kind after recent and before playtime in deterministic priority, bump stored evidence to v3, and keep legacy v2 decoding/fallback behavior.

- [ ] **Step 8: Align Go API and frontend normalization**

  Add the TypeScript factor and evidence kind, update technical-leak regexes, response fixtures, and card evidence rendering tests; do not add a button, mutation, status badge, or completion editor.

- [ ] **Step 9: Run backend recommendation/API tests**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/recommendations ./internal/httpapi -count=1`

  Expected: PASS with zero failures.

- [ ] **Step 10: Run required frontend checks**

  Run: `pnpm --dir web run test`

  Expected: PASS with zero failed tests.

- [ ] **Step 11: Run frontend typecheck**

  Run: `pnpm --dir web run typecheck`

  Expected: PASS with zero TypeScript errors.

- [ ] **Step 12: Run frontend lint**

  Run: `pnpm --dir web run lint`

  Expected: PASS with zero lint errors.

- [ ] **Step 13: Run frontend build**

  Run: `pnpm --dir web run build`

  Expected: PASS and produce the Vite production bundle.

- [ ] **Step 14: Commit ranking and public contracts safely**

  ```bash
  git diff -- internal/recommendations/types.go internal/recommendations/scoring.go internal/recommendations/scoring_test.go internal/recommendations/retrieval.go internal/recommendations/repository_integration_test.go internal/recommendations/evidence.go internal/recommendations/evidence_test.go internal/recommendations/presentation_test.go internal/httpapi/overview_contract_test.go internal/httpapi/ranking_handlers_test.go web/src/api/types.ts web/src/api/client.ts web/src/api/client.test.ts web/src/components/RecommendationCard.test.tsx
  git add -p -- internal/recommendations/types.go internal/recommendations/scoring.go internal/recommendations/scoring_test.go internal/recommendations/retrieval.go internal/recommendations/repository_integration_test.go internal/recommendations/evidence.go internal/recommendations/evidence_test.go internal/recommendations/presentation_test.go internal/httpapi/overview_contract_test.go internal/httpapi/ranking_handlers_test.go web/src/api/types.ts web/src/api/client.ts web/src/api/client.test.ts web/src/components/RecommendationCard.test.tsx
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "feat: rank from completed game anchors"
  ```

### Task 10: Verify Lifecycle, Privacy, and End-to-End Behavior

**Files:**
- Modify: `internal/httpapi/data_service_integration_test.go`
- Modify: `internal/jobs/queue_test.go`
- Modify: `cmd/e2e-fakes/main.go`
- Create: `web/tests/e2e/completion-signals.spec.ts`
- Modify: `Makefile`
- Modify: `README.md`
- Modify: `docs/local-demo.md`

**Interfaces:**
- Consumes: all prior tasks and existing user deletion/tombstone, fake-upstream, and Playwright harnesses.
- Produces: no new product interface; complete lifecycle, privacy, operational documentation, and release verification coverage.

- [ ] **Step 1: Write the failing deletion cascade test**

  Extend `TestDeleteUserDataRetainsSharedCatalogAndRemovesOwnedRows` to seed all new tables and assert user unlock/evidence/projection rows and user-owned projection jobs are gone, while shared definitions/classifications remain with the shared game.

- [ ] **Step 2: Write the failing deletion-fence race test**

  Extend `TestDeleteFencesInFlightWorkerFromRecreatingData` with a leased user-owned `completion.project` job and an unowned game-scoped `achievement.classify` job; assert deletion cancels/fences the projection job and late projection/profile/recommendation writes fail, while shared classification may finish but its affected-user query cannot enqueue a deleted user's projection.

- [ ] **Step 3: Write the failing end-to-end completion scenario**

  Add `completion-signals.spec.ts`: fake a low-playtime game with one non-DLC ending plus a high-hour open-ended game, run sync through classification/projection/profile/ranking, and assert the published profile/recommendation evidence names the completed game historically, never calls it recent without two-week minutes, and exposes no manual completion control or technical fields.

- [ ] **Step 4: Write the failing OpenAI-disabled end-to-end scenario**

  In the same Playwright file, fake a low-playtime game below 100% with an unlocked English `Reach an ending` achievement plus one unresolved definition and OpenAI disabled; assert the first complete generation stores lexical plus explicit unknown sources, sync/ranking still completes, the deterministic completion signal is `1.50` internally through backend fixture assertions, and the UI remains usable with historical deterministic prose. Keep a separate 100%-only fixture assertion proving that path is also provider-independent.

- [ ] **Step 5: Run lifecycle and E2E tests to verify red**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/httpapi ./internal/jobs -run 'TestDeleteUserDataRetainsSharedCatalogAndRemovesOwnedRows|TestDeleteFencesInFlightWorkerFromRecreatingData' -count=1`

  Expected: FAIL until new tables/jobs are covered by deletion assertions.

- [ ] **Step 6: Extend fake Steam/OpenAI responses and deletion coverage**

  Add localized player achievements, missing-metadata schema fallback, exact lexical matches and every lexical veto, all OpenAI semantic classes, invalid classifier output, and disabled-provider fixtures; update deletion assertions and shared-versus-user-owned queue fencing without logging payload contents.

- [ ] **Step 7: Add the new Playwright file to `make verify`**

  Append `tests/e2e/completion-signals.spec.ts` to the existing explicit Playwright command; preserve the documented explicit system-Chrome fallback.

- [ ] **Step 8: Update operational documentation**

  Document automatic historical completion, exact non-manual behavior, primary/fallback Steam sources, old-completion versus two-week recency wording, last-known-good preservation, and safe aggregate troubleshooting in `README.md` and `docs/local-demo.md`.

- [ ] **Step 9: Run CodeGraph impact selection on the complete change**

  Run: `codegraph sync .`

  Expected: index reports up to date with no indexing failure.

- [ ] **Step 10: Inspect the affected regression surface**

  Run: `codegraph affected internal/steam/types.go internal/steam/http_client.go db/migrations/00015_achievement_completion_signals.sql internal/completion internal/sync/service.go internal/ai/openai.go internal/profile/vectors.go internal/ai/repository.go internal/recommendations/retrieval.go internal/recommendations/types.go web/src/api/types.ts -p .`

  Expected: output includes Steam/sync, AI, profile, recommendation, API/frontend, store migration, feedback, lifecycle, and app-wiring tests; add any omitted high-confidence affected package to the next focused run.

- [ ] **Step 11: Run the affected backend packages with race detection**

  Run: `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/steam ./internal/completion ./internal/sync ./internal/ai ./internal/profile ./internal/recommendations ./internal/feedback ./internal/httpapi ./internal/jobs ./internal/app ./internal/store -count=1`

  Expected: PASS with zero failures and zero race reports.

- [ ] **Step 12: Run repository-wide verification**

  Run: `make verify`

  Expected: sqlc generated diff is empty; all Go race tests, frontend test/typecheck/lint/build, Compose config/build, and Playwright smoke/presentation/catalog-discovery/completion tests pass with zero failures. If pinned Chromium cannot download, set the documented explicit `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` system-Chrome fallback and rerun this exact target.

- [ ] **Step 13: Inspect the final diff and whitespace**

  Run: `git diff --check`

  Expected: no output and exit status zero; `git status --short` lists only intended feature files plus any separately preserved pre-existing work.

- [ ] **Step 14: Commit lifecycle, E2E, and documentation coverage safely**

  ```bash
  git diff -- internal/httpapi/data_service_integration_test.go internal/jobs/queue_test.go cmd/e2e-fakes/main.go web/tests/e2e/completion-signals.spec.ts Makefile README.md docs/local-demo.md
  git diff --no-index -- /dev/null web/tests/e2e/completion-signals.spec.ts || test $? -eq 1
  git add -p -- internal/httpapi/data_service_integration_test.go internal/jobs/queue_test.go cmd/e2e-fakes/main.go Makefile README.md docs/local-demo.md
  git add -- web/tests/e2e/completion-signals.spec.ts
  git diff --cached --check
  git diff --cached --name-status
  git commit -m "test: verify achievement completion lifecycle"
  ```
