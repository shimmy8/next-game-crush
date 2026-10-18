# Feedback Preference Corrections and Calendar Actions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn explicit negative mode feedback into bounded, reversible deterministic preference penalties, and expose the same feedback workflow, posters, and safe Steam links throughout the calendar.

**Architecture:** A small shared `internal/preferences` domain owns the mode vocabulary, adjustments, and disable workflow. Steam categories provide authoritative mode evidence; feedback classification produces immutable normalized targets; recommendation retrieval calculates a separate soft factor. Existing profile/ranking jobs and authenticated diff polling remain the workflow backbone, with compact presentations using one shared accessible dialog.

**Tech Stack:** Go 1.27.2 (`GOWORK=off`), PostgreSQL/pgvector, PostgreSQL jobs, goose migrations, sqlc v1.31.1, React 19, TypeScript, Vitest/Testing Library, Playwright; existing dependencies only.

**Spec:** `docs/superpowers/specs/2026-10-18-feedback-preference-corrections-calendar-design.md`; presentation integration follows `docs/superpowers/specs/2026-10-18-discovery-wishlist-presentation-design.md` and `docs/superpowers/plans/2026-10-18-discovery-wishlist-presentation.md`.

## Global Constraints

- Only a `dislike`, negative polarity, `actionable=true`, confidence at least `0.70`, and a supported `mode / avoid` target may create an AI-derived adjustment.
- Specialized modes are `co-op`, `competitive-multiplayer`, `massively-multiplayer`, `local-multiplayer`, `online-multiplayer`, and `single-player`; `multiplayer` is the broad target. Positive mode rules and arbitrary dimensions are out of scope.
- Category penalties use `max(confidence * strength)`, clamped to `[0,1]`, with default weight `-5`; they never change eligibility. Exclude each adjustment's own `source_game_id` before taking the maximum.
- Canonical ranking evidence comes only from successfully normalized Steam categories, never titles, LLM game features, store tags, HTML scraping, or a third-party provider.
- Missing modes mean zero penalty. Failed upstream calls preserve the last successful projection. Failed rebuilds preserve the last published ranking.
- Direct actions contribute once; classification polarity adds no second direct-game like/dislike contribution. Existing repeated direct-dislike exclusion remains.
- Preserve immutable feedback and classification history, deletion fencing, single-user allowlisting, authenticated ownership, and CSRF protection for browser mutations.
- Feedback text is at most 500 Unicode code points. `remind_me` submits immediately without reason or comment.
- Calendar grid posters are `32×48` px; list/bucket posters are `48×72` px; lazy-load, preserve aspect ratio, wrap long names, and use deterministic fallback on missing/failed images.
- Steam links use the server-generated URL for a positive numeric AppID, `target="_blank"`, `rel="noopener noreferrer"`, and an accessible external-link label. No image proxy, download, persistent cache, or new poster column.
- No SteamID, user/recommendation/adjustment IDs, feedback text, raw AI output/prompts, job payloads, session values, or credentials in general logs. Read `.env.example` for variable names; never read `.env`.
- Use only isolated `TEST_DATABASE_URL` with Compose `test-db`; never clean the application database. Generate `internal/store/sqlc/` with `make sqlc`, never edit it manually.
- Current parallel source changes belong to another task. Preserve them, especially `profile-v2`, `BuildVectors(signals, now)`, last-played recency, profile-summary-v3, dashboard copy, and their tests. This plan does not authorize reverting them.
- No commits, pushes, deployments, or changes to existing specs or `docs/steam-release-recommender-idea.md` as part of this planning task. During execution, commit checkpoints require the user's separate authorization.

## Review Focus

1. Two different source games create the same rule: skip the candidate's own event before aggregation, but still apply another game's rule; Tasks 5 and 7 pin this down.
2. A classifier fake bypasses the HTTP client or returns a contradictory valid-enum target: repeat validation at the service boundary; Tasks 4 and 5 prove no adjustment appears.
3. A disable races another disable, deletion, or an unrelated ranking: return the stored job/version and the workflow's exact completed version, never a later global maximum; Tasks 8 and 9 cover this.
4. Reranking replaces recommendation IDs while a dialog is pending or its opener disappears: keep the accepted workflow alive, avoid resubmission, and restore focus to a surviving calendar control; Tasks 10 and 11 cover this.
5. A Steam response has generic multiplayer evidence or ambiguous category metadata: broad rules may match generic multiplayer, specific rules must not infer a subtype; Tasks 1 and 3 cover this.

## Delivery Boundaries and File Ownership

The backend tasks are independent of presentation collection splitting. The migration is reserved as **`00010_feedback_preference_adjustments.sql`** after catalog discovery's `00009`; if executing independently before that migration exists, choose the next free number once before application and update this plan/test filename references. Never renumber or rewrite an applied migration. Canonical category projection must be called from the successful metadata writer that exists after catalog work; do not duplicate its fetch loop.

Only Tasks 11–12 require the presentation plan's calendar/filter and overview reload changes to have landed. Task 10 develops isolated dialog components against the existing `Recommendation` fields. Task 7 touches scoring/explanation files also touched by presentation: apply the additive factor after its wishlist-weight patch when both ship together; preserve wishlist weight `0` if already implemented. Use algorithm version `ranking-v3-mode-preferences` for the combined result; standalone execution uses that same new version and retains the then-current wishlist policy. The feature never silently resets a previous weight.

| Unit | Files/responsibility | Dependencies |
|---|---|---|
| Shared vocabulary | new `internal/preferences/modes.go`, tests; Steam adapter `modes.go` | Task 1 |
| Persistence | new migration, `db/queries/preference_adjustments.sql`, `game_modes.sql`; generated bindings; new preference repository | Tasks 1–2 |
| Steam projection | new focused catalog projection helper; successful catalog metadata transaction | Tasks 1–3; catalog writer integration |
| Classifier/workflow | AI types/schema/prompt, feedback service/repository | Tasks 2, 4–5 |
| Profile/ranking | direct SQL count correction; separate factor/build key/evidence helpers | Tasks 3, 5–7 |
| Disable/API | new preference service/workflow, diff source adapter, handlers and TS contracts | Tasks 2, 7–9 |
| Reusable compact UI | new feedback hook/dialog/launcher and compact presentation helpers | Tasks 9–10 |
| Calendar integration | `ReleaseCalendar`, `ReleaseBuckets`, `CalendarPage`, dashboard callback wiring | Presentation plan and Tasks 10–11 |
| Profile corrections UI | new adjustment section plus minimal profile/dashboard wiring | Tasks 9, 12; presentation dashboard wiring |

Read README, both specs, and this plan before execution. Run `codegraph status .`, sync if stale, and `codegraph context "feedback preference corrections calendar actions" -p .`. Before shared changes, inspect `codegraph impact LoadSignals -p .`, `codegraph impact Candidates -p .`, and the relevant exact symbols; graph output supplements source inspection. After each source task, `codegraph sync .` before further graph queries, and use `codegraph affected <that-task-files> -p .` to select additional tests. Never add local graph artifacts to a commit.

## Test Commands

Start the isolated database with `make wait-test-db`. In the shell used for integration tests set only the documented test URL:

```bash
export TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable'
```

Every task's RED command must fail for its stated missing behavior, not a service/tool outage; resolve infrastructure first. GREEN means zero failures and no skipped database tests. Existing fixtures use private per-test schemas; retain that isolation. The final gate is `make verify`, which includes sqlc drift checks, all race tests, frontend checks, Docker build/config, and the existing browser smoke entry point.

---

### Task 1: Define one canonical mode vocabulary and explicit Steam aliases

**Files:** Create `internal/preferences/modes.go`, `internal/preferences/modes_test.go`, `internal/steam/modes.go`, `internal/steam/modes_test.go`.

**Interfaces:**

```go
// internal/preferences: no AI, Steam, profile, or recommendations imports.
type Mode string
const ( ModeCoop Mode = "co-op"; ModeCompetitive Mode = "competitive-multiplayer";
  ModeMassive Mode = "massively-multiplayer"; ModeLocal Mode = "local-multiplayer";
  ModeOnline Mode = "online-multiplayer"; ModeSinglePlayer Mode = "single-player";
  ModeMultiplayer Mode = "multiplayer" )
func Modes() []Mode // fresh, deterministically ordered seven-value copy
func ValidMode(Mode) bool
func NormalizeModeAlias(string) (Mode, bool)
func MatchesMode(target Mode, evidence []Mode) bool
func ModeLabel(Mode) string
func ModeEvidence(Mode) string
// internal/steam: consumes the existing Category type.
func CanonicalModes([]Category) []preferences.Mode // sorted, deduplicated
```

- [ ] **Step 1 — RED tests:** Add table tests `TestNormalizeModeAlias`, `TestModeMatching`, and `TestCanonicalModes`. Assert `кооп`, `кооператив`, `coop`, `co-op` → `co-op`; unknown input → false. Steam IDs/aliases: `2 / Single-player` → single-player; `1 / Multi-player` and `27 / Cross-Platform Multiplayer` → broad multiplayer; `9 / Co-op` → co-op; `38 / Online Co-op` → co-op plus online-multiplayer; `39 / Shared/Split Screen Co-op` → co-op plus local-multiplayer; `36 / Online PvP` → online-multiplayer plus competitive-multiplayer; `37 / Shared/Split Screen PvP` → local-multiplayer plus competitive-multiplayer; `24 / Shared/Split Screen` → local-multiplayer; `20 / MMO` → massively-multiplayer. Recognized `PvP` description supplies competitive-multiplayer. Unknown IDs may use only these explicit normalized description aliases. Unknown/contradictory descriptions never broaden a recognized single-player category into multiplayer. Duplicate categories produce one mode each.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/preferences ./internal/steam -run 'Test(NormalizeModeAlias|ModeMatching|CanonicalModes)'` → missing symbols/assertion failures.
- [ ] **Step 3 — Implement:** Keep all seven values in the Go domain; Steam mappings only return domain values. Generic `Multi-player` is real broad evidence, not proof of co-op/online/competitive play. `MatchesMode(multiplayer, [any multiplayer mode])` is true; it is false for single-player alone. Specific targets match only their own canonical evidence. Do not parse game names or arbitrary feedback text deterministically. Emit Russian labels, including `Кооперативные игры`, and grounded evidence `Вы просили реже предлагать кооперативные игры`; provide equivalent fixed Russian copy for every other mode.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2; assert matching is order independent and unknown-only categories return an empty slice. No frontend copy of the canonical enum allowlist: frontend targets remain opaque strings with server labels.

### Task 2: Add constrained adjustment/mode storage and generated SQL queries

**Files:** Create `db/migrations/00010_feedback_preference_adjustments.sql`, `db/queries/preference_adjustments.sql`, `db/queries/game_modes.sql`, `internal/preferences/repository.go`, `internal/preferences/repository_integration_test.go`; modify `db/queries/feedback.sql`, `internal/store/migration_upgrade_test.go`; generate `internal/store/sqlc/`.

**Interfaces:**

```go
type Target struct { Dimension string `json:"dimension"`; Target Mode `json:"target"`; Direction string `json:"direction"` }
type Adjustment struct {
  ID, UserID, SourceFeedbackID, SourceGameID uuid.UUID
  Dimension string; Target Mode; Direction string
  Confidence, Strength float64; Active bool
  CreatedAt, UpdatedAt time.Time; DisabledAt *time.Time
  DisableRebuildJobID *uuid.UUID; DisablePreviousRankingVersion *int64
}
func NewSQLRepository(*store.Store) (*SQLRepository, error)
func (*SQLRepository) List(ctx context.Context, userID uuid.UUID, includeInactive bool) ([]Adjustment, error)
func (*SQLRepository) ForFeedback(ctx context.Context, userID, feedbackID uuid.UUID) (*Adjustment, error)
```

- [ ] **Step 1 — RED tests:** `TestPreferenceAdjustmentsMigration` upgrades the previous schema and preserves existing classifications/rankings. `TestAdjustmentConstraintsAndScope` rejects invalid dimension/target/direction, confidence/strength outside `[0,1]`, partial target triplets, and mismatched source ownership. Assert one `source_feedback_id` can create only one row, different events may share a target, foreign-user reads yield no row, and user deletion cascades user adjustments without removing shared seed game modes.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/store ./internal/preferences -run 'Test(PreferenceAdjustmentsMigration|AdjustmentConstraintsAndScope)'` → missing table/constraint or API failures.
- [ ] **Step 3 — Implement migration:** `game_modes`: `game_id uuid REFERENCES games(id) ON DELETE CASCADE`, `mode text`, `source text CHECK(source='steam_category')`, `source_content_hash text NOT NULL`, `last_successful_fetch_at timestamptz NOT NULL`, PK `(game_id,mode,source)`; mode check uses the seven shared values (generic multiplayer is permitted evidence). `preference_adjustments`: UUID PK; required user, source feedback, and source game FKs with deletion cascade; unique source feedback; `dimension='mode'`, the seven-value target check, `direction='avoid'`, finite confidence/strength checks in `[0,1]`, strength default `1`, active default true, timestamps and the three nullable disable metadata fields from the spec. Enforce source user ownership using a unique `(id,user_id)` constraint on feedback events and composite `(source_feedback_id,user_id)` FK; source game comes from the event's recommendation inside the insert query, never the browser. Add `(user_id,active,target)` index and a consistency check: active rows have no disable metadata; inactive rows have `disabled_at`, job ID, and previous version. `disable_rebuild_job_id` references `sync_jobs(id)`; user deletion removes both in the same cascade. Extend classifications with nullable preference triplet and all-null-or-all-valid checks, including taxonomy/target pairing. Old rows stay all-null; no speculative reclassification/backfill.
- [ ] **Step 4 — Implement named SQL queries and repository:** In `preference_adjustments.sql`, add `ListPreferenceAdjustments :many` (`user_id`, `include_inactive`, order created_at/id), `GetPreferenceAdjustmentForFeedback :one` (user + feedback), `UpsertPreferenceAdjustmentFromFeedback :one` (INSERT SELECT event/recommendation ownership; `ON CONFLICT(source_feedback_id) DO UPDATE SET source_feedback_id=EXCLUDED.source_feedback_id`, preserving inactive state), `LockPreferenceAdjustment :one` (user + ID, `FOR UPDATE`), and `MarkPreferenceAdjustmentDisabled :execrows` (user, ID, job, previous version, disabled timestamp). In `game_modes.sql`, add `DeleteSteamGameModes :exec`, `InsertSteamGameMode :exec`, `ListGameModes :many` for a UUID array, and `ListStoredSteamCategorySnapshots :many` for validated stored Steam snapshots. Extend `UpsertFeedbackClassification` and add `GetFeedbackClassification :one` with the three nullable fields. Use generated parameter structs and `Queries.WithTx(tx)`; repository boundaries convert pgtype values to domain UUID/Mode/time types.
- [ ] **Step 5 — Generate and verify GREEN:** Run `make sqlc`, then Step 2. Add `TestDatabaseModeChecksMatchDomainVocabulary` that inserts each `Modes()` value and rejects an unknown one for both tables. Run `git diff --check`; review the generated diff. Down migration reverses only newly added objects/columns/constraints.

### Task 3: Persist category mode projections only on successful metadata refresh

**Files:** Create `internal/catalog/game_modes.go`, `internal/catalog/game_modes_integration_test.go`; modify `internal/catalog/service.go` and `internal/catalog/service_test.go`; after catalog qualification lands also modify `internal/catalog/discovery_repository.go` and `internal/catalog/discovery_test.go` at its successful metadata transaction, with generated queries from Task 2.

**Interfaces:**

```go
func replaceSteamModes(ctx context.Context, q *dbgen.Queries, gameID uuid.UUID,
  categories []steam.Category, sourceContentHash string, fetchedAt time.Time) error
func (s *Service) backfillStoredSteamModes(ctx context.Context) error
```

- [ ] **Step 1 — RED tests:** `TestSteamModesProjectionIsAtomicAndPreservesLastSuccess` refreshes co-op → single-player and asserts old modes are replaced, hash/time updated, and snapshot/modes roll back together after an injected transaction error. Unavailable, malformed, failed, and not-modified calls retain old mode rows. Successful empty/unknown categories replace the projection with no rows. `TestSteamModeBackfillUsesOnlyValidatedStoredCategories` projects existing valid `steam_store` snapshots, skips seed/invalid snapshots, and never infers a mode from a title or LLM features.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/catalog ./internal/steam -run 'Test(SteamModes|SteamModeBackfill|CanonicalModes)'` → missing projection/backfill behavior.
- [ ] **Step 3 — Implement:** Use Task 1 canonicalization and Task 2 queries within the same successful game-metadata transaction. Delete only source `steam_category`, insert the normalized set, and commit together with the trusted snapshot/hash. Errors must not delete successful modes. Backfill stored successful Steam snapshots once by comparing the stored snapshot hash to projection hashes; for a successful empty projection repeated work is harmless and does not invent evidence. Reuse the catalog refresh entry point; no new scheduler/upstream provider. When catalog qualification is available, invoke the same helper inside its lease-fenced transaction from the catalog plan's Task 8, and inside `Service.RefreshGame`/known refresh. Mark its existing candidate dirty-state in the same transaction when canonical modes change; never let an older qualification revision regress newer modes. No duplicate metadata writer or fetch loop.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2 and full `GOWORK=off go test -race ./internal/catalog ./internal/steam`; verify a generic multiplayer category matches only the broad rule unless a specialized category is present.

### Task 4: Version and validate strict feedback structured output

**Files:** Modify `internal/ai/types.go`, `internal/ai/openai.go`, `internal/ai/prompts.go`, `internal/ai/openai_test.go`, `internal/ai/prompts_test.go`; add `internal/ai/feedback_validation_test.go`.

**Interfaces:** Add field `Preference *preferences.Target` with JSON tag `preference` to `ai.FeedbackClassification`. Add `FeedbackSchemaVersion = "feedback-structured-output-v2"`, bump `FeedbackPromptVersion = "feedback-classification-v2"`, use response format name `feedback_classification_v2`; leave feature/profile/explanation versions intact. Export `ValidateFeedbackClassification(output FeedbackClassification, evidenceKey string) error` for both HTTP and workflow boundaries.

- [ ] **Step 1 — RED tests:** `TestFeedbackPreferenceStructuredSchema` asserts strict object, `additionalProperties=false` at both levels, all root fields required including nullable preference, nested enums generated from `preferences.Modes()`, and no evidence text/name/IDs in schema. `TestFeedbackPreferenceValidation` covers valid `.70` and `.92` co-op; `.699` non-actionable; null preference; unsupported dimension/target/direction; unknown and duplicate evidence keys; NaN/Inf; malformed JSON, extra fields, refusal, incomplete response; `single-player/co-op`, `multiplayer/single-player`, and `genre/co-op` contradictions. Errors cannot create a usable output. Verify `кооп`/`coop` user text is sent as text, while model output target `coop` is rejected because output must already be canonical.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/ai -run 'TestFeedback(Preference|Length)'` → missing schema/validation/version assertions.
- [ ] **Step 3 — Implement:** Nullable preference is represented by a strict enum object or null in JSON Schema. The runtime validator repeats mode allowlists, requires negative/actionable/confidence `>=.70` for any non-null preference, checks taxonomy pairing, and validates exactly supplied opaque evidence keys without duplicates. It may retain a well-formed low-confidence broad classification with preference null; contradictory target objects are invalid. Keep source action checks in feedback service because AI receives only `FeedbackText{EvidenceKey:"feedback-text", Text:...}`. Required nullable `preference` must be present in raw structured JSON; distinguish omitted fields from null during decoding. Bump the feedback content hash using `FeedbackSchemaVersion`; do not bump global schema for unrelated AI tasks.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2 and `GOWORK=off go test -race ./internal/ai`; request-capture tests assert only feedback text/opaque key are supplied and errors/log output contain no request/response body or identifiers.

### Task 5: Materialize one adjustment per event through retry-safe classification

**Files:** Modify `internal/feedback/service.go`, `internal/feedback/repository.go`, `internal/feedback/service_test.go`, `internal/feedback/repository_integration_test.go`; use Task 2 generated queries.

**Interfaces:** `feedback.Classification` gains `Preference *preferences.Target`; `ClassificationSchemaVersion=2`. Retain `Repository.SaveClassification(context.Context, Classification) error`, but define its stronger contract: insert immutable classification and derive/upsert its adjustment in one transaction. Cached `Classification` must go through this same projection method on resume. Add `Event.GameID uuid.UUID` from the server-side recommendation join. Do not add an AI call on a cached classification.

- [ ] **Step 1 — RED tests:** `TestCoopDislikeCreatesExactlyOneAdjustment` submits `dislike / не люблю коопы`, valid negative `.92` output and asserts one active mode/co-op/avoid row with confidence `.92`, strength `1`, and correct source IDs. `TestClassificationResumeRepairsMissingProjectionWithoutRecallingAI` resumes a saved v2 classification lacking an adjustment; replay creates one row, classifier call count stays one, and an already disabled row stays inactive. `TestNonActionableFeedbackNeverCreatesAdjustment` tables like/remind, positive/neutral, actionable=false, `.699`, unsupported target, taxonomy contradiction, bad/duplicate citations, refusal, malformed, incomplete, and free-text-only unavailable classifications.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/feedback -run 'Test(CoopDislike|ClassificationResume|NonActionable|MissingOpenAI)'` → missing projection/validation behavior.
- [ ] **Step 3 — Implement:** Repeat Task 4 validation even for injected classifier implementations, then check source `ActionDislike`. Save the immutable safe classification and insert adjustment using an event/recommendation SELECT in the same transaction. After classification INSERT ON CONFLICT, reread the stored winning classification and derive the adjustment from that value, not from the losing caller's in-memory value. For old cached v1 rows with null targets, preserve history and do not infer a new semantic target. Invalid output is stored as a safe non-actionable fallback classification with null preference; never store unsupported enum values or raw responses. A selected reason `multiplayer` creates deterministic `mode=multiplayer/avoid, confidence=1` only for dislike with no text, or when OpenAI is disabled/unavailable (capability disabled, HTTP/request timeout). A valid but non-actionable classification, refusal, invalid evidence/output, and incomplete output do not activate this fallback. Free text alone cannot create a semantic rule without valid AI. Retain deterministic model provenance `deterministic-fallback`; do not label fallback as model-generated. Do not add fallback for other predefined reasons in this iteration.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2 plus `GOWORK=off go test -race ./internal/feedback ./internal/preferences`. Inject transaction failure between classification and adjustment and assert both roll back; race two valid but different classifications and assert the one persisted adjustment matches the immutable winning classification. Classification errors preserve direct event and still permit profile/ranking children. Every feedback root/child spec must carry `UserID` so deletion tombstones reject resumed work. No general log contains event IDs/text/output.

### Task 6: Remove duplicate direct-game profile contributions

**Files:** Modify `internal/profile/repository.go`, `internal/feedback/repository_integration_test.go`; add `internal/profile/repository_integration_test.go` only if a focused independent fixture is needed.

**Interfaces:** Keep `LoadSignals(context.Context, uuid.UUID) ([]profile.Signal,error)` and the current recency-aware `BuildVectors([]Signal,time.Time) Vectors` unchanged.

- [ ] **Step 1 — RED test:** Replace `TestOnlyActionableClassificationAddsStructuredProfileWeight` (which currently expects two likes) with `TestClassificationDoesNotDuplicateDirectGameSignals`. For high/low-confidence positive classification assert a single direct like produces `Likes==1`; for high negative and contradictory positive classification assert one direct dislike produces `Dislikes==1`, regardless of classification polarity. Two direct dislike events still produce `Dislikes==2`. Compare vectors against `BuildVectors` with exactly one direct signal, preserving the current fixed-clock recency inputs.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/feedback ./internal/profile -run 'TestClassificationDoesNotDuplicateDirectGameSignals'` → current doubled count assertion fails.
- [ ] **Step 3 — Implement:** Remove classification counts and unnecessary classification join from the direct-feedback CTE. Do not alter playtime/last-played/recency weights from parallel work. Bump the current profile algorithm to `profile-v3-direct-feedback-once` so a rebuild identifies the changed vector semantics; do not regress `profile-v2` recency behavior.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2, then `GOWORK=off go test -race ./internal/profile ./internal/feedback`; retain the existing one-dislike soft/two-dislikes exclusion test.

### Task 7: Add bounded deterministic ranking factor, evidence, and build identity

**Files:** Create `internal/recommendations/mode_preferences.go`, `internal/recommendations/mode_preferences_test.go`; modify `types.go`, `scoring.go`, `scoring_test.go`, `retrieval.go`, `service.go`, `service_test.go`, and `repository_integration_test.go` in that directory; modify `web/src/components/RecommendationCard.tsx` and `RecommendationCard.test.tsx` only for technical-text rejection; use generated adjustment/mode queries.

**Interfaces:**

```go
// Add JSON fields without changing existing fields.
// Factors.NegativeModePreference float64 `json:"negativeModePreference"`
// Weights.NegativeModePreferenceWeight float64 `json:"negativeModePreferenceWeight"`
type RankingInputs struct { Candidates []Candidate; ActiveAdjustments []preferences.Adjustment }
// Candidate additionally carries Modes []preferences.Mode and ModePreferenceEvidence *Evidence.
// Add this build-input method to Repository and update fakes; retain Candidates below.
func (*SQLRepository) RankingInputs(context.Context, uuid.UUID, profile.Profile, int) (RankingInputs,error)
func (*SQLRepository) Candidates(context.Context, uuid.UUID, profile.Profile, int) ([]Candidate,error)
func ModePreferenceFactor(candidateID uuid.UUID, modes []preferences.Mode, active []preferences.Adjustment) float64
func ModePreferenceEvidence(candidateID uuid.UUID, modes []preferences.Mode, active []preferences.Adjustment) *Evidence
func ActiveAdjustmentsDigest([]preferences.Adjustment) string
func RankingBuildKey(requestKey, algorithmVersion, adjustmentDigest string) string
```

- [ ] **Step 1 — RED tests:** `TestModePreferenceFactorUsesMaxAfterSourceExclusion` asserts `.8×.5` and `.9×1` → `.9`, not `1.3`; source's own adjustment only → `0`; source plus another game's rule → the other rule's strength; inactive/unknown/no modes → `0`; broad multiplayer matches co-op but not single-player. Test finite clamping. `TestModePenaltyIsSoftAndCalibrated`: non-wishlist baseline `{PositiveSimilarity:.4, Novelty:1}` scores `15`, confidence `1` with factor `1` scores `10`, delta `5`, and remains eligible; zero-mode baseline stays `15`. Add persistence, active digest order-independence, source-ID sensitivity, disable/version change, and old JSON missing-factor tests.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/recommendations -run 'Test(ModePreference|ModePenalty|ActiveAdjustments|RankingBuild)'` → missing factor/build identity behavior.
- [ ] **Step 3 — Implement:** `RankingInputs` reads active adjustments once and all selected game modes in one UUID-array query, not one adjustment/mode query per candidate. Use one read-only repeatable-read transaction for the candidate set plus adjustment/mode snapshot. Retain `Candidates` as a compatibility wrapper returning `RankingInputs.Candidates`; catalog/presentation callers keep their exact signature, union-source behavior, deduplication and limit guarantees. Recommendation rebuild uses the complete input result once. Keep provenance rows until after per-candidate source exclusion, then take maximum across all matching targets (including broad/specific overlap). The new factor is independent of negative semantic similarity and `Dislikes`; do not feed it into `Eligible`. Score adds the clamped factor times `-5`, then uses existing total clamping/rounding. Persist both JSON fields, set `AlgorithmVersion="ranking-v3-mode-preferences"`, and preserve presentation's wishlist weight zero if present.
- [ ] **Step 4 — Implement reproducible build identity:** Digest canonical sorted active row tuples `(id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength)`, using stable JSON/float encoding and SHA-256; never include query-order/timestamps or feedback text. Build key format is `mode-v1:<sha256(requestKey)>:<algorithmVersion>:<adjustmentDigest>`. Keep `SaveRanking(..., buildKey string)` signature; under its existing user advisory lock, recognize an existing run with the same `mode-v1:<requestHash>:<algorithmVersion>:` prefix. A published run is replayed with its exact version regardless of later changed inputs. An unpublished run may be rebuilt at the same ranking version with its key replaced by the current inputs' key; rows/explanations are replaced before publication. This prevents a resumed workflow from publishing a second version after input changes while a different algorithm still creates a new build. Legacy non-prefixed build keys keep exact-key semantics. Add a concurrency test for same request and different adjustment digests. Independent requests get different prefixes; a new disable request must produce a new version. Replace presentation's private `rankingBuildKey(workflowKey)` transformation at its single call site with `RankingBuildKey` rather than nesting two transformations; queue/finalization keys remain unchanged.
- [ ] **Step 5 — Implement human evidence:** Add deterministic mode-preference evidence when the new factor is positive. `ModePreferenceEvidence` chooses the maximum matching rule with target then ID as a deterministic tie-break. It returns fixed ID `negative-mode-preference:<canonical-target>` and Task 1's human text; carry it from Candidate through the existing Recommendation.Evidence field into SaveExplanation. For legacy stored positive factors without rule identity use ID `negative-mode-preference` and truthful generic copy `Учитываем ваши пожелания реже предлагать игры такого режима`. Persist the chosen human evidence with the ranking; allow only these IDs with their exact fixed texts through `safePresentation` when the persisted factor is positive, without querying current adjustments (old rankings retain their own explanation). Reserve one of the three evidence slots for the correction; for wishlist items retain presentation's membership slot and highest substantive affinity in the other two slots. Include the correction despite its negative contribution. Add `negativeModePreference`/`negativeModePreferenceWeight` to backend and card technical-text rejection so internal enum/weight text cannot leak to UI. Preserve catalog's explanation cache keyed by public grounded evidence; changed mode evidence must change that cache key. No request for an LLM to decide ranking.
- [ ] **Step 6 — Verify GREEN:** Repeat Step 2, then `GOWORK=off go test -race ./internal/recommendations ./internal/profile ./internal/feedback`. Integration tests assert saved factors/weights, bounded query count for adjustments/modes, persisted human evidence, and unchanged old published results after a failed rebuild.

### Task 8: Atomically disable an adjustment and rerank through existing jobs

**Files:** Create `internal/preferences/service.go`, `internal/preferences/disable.go`, `internal/preferences/service_test.go`, `internal/preferences/disable_integration_test.go`; extend `internal/preferences/repository.go`; modify `internal/feedback/repository.go`, `internal/feedback/service.go`, `internal/app/app.go` only for workflow registration and shared diff support.

**Interfaces:**

```go
type DisableAccepted struct {
  AdjustmentID uuid.UUID `json:"adjustmentId"`; RebuildJobID uuid.UUID `json:"rebuildJobId"`
  PreviousRankingVersion int64 `json:"previousRankingVersion"`; AlreadyInactive bool `json:"alreadyInactive"`
}
type DisablePayload struct { Version int `json:"version"`; AdjustmentID uuid.UUID `json:"adjustment_id"` }
type DisableWorkflow struct { AdjustmentID,UserID,JobID uuid.UUID; PreviousRankingVersion int64 }
func (*SQLRepository) DisableAndEnqueue(context.Context, uuid.UUID, uuid.UUID) (DisableAccepted,error)
func (*SQLRepository) DisableWorkflow(context.Context, uuid.UUID) (DisableWorkflow,error)
func NewService(repository *SQLRepository, queue *jobs.Queue) (*Service,error)
func (*Service) Disable(ctx context.Context, userID, adjustmentID uuid.UUID) (DisableAccepted,error)
func (*Service) List(ctx context.Context, userID uuid.UUID, includeInactive bool) ([]Adjustment,error)
func (*Service) RegisterHandlers(*jobs.Worker) error
```

- [ ] **Step 1 — RED tests:** `TestDisableAdjustmentAtomicallyEnqueuesOnce` makes simultaneous calls and asserts one `active=false` transition, one root job, stored previous ranking `7`, first `alreadyInactive=false`, repeats true with the identical job/version even after global ranking `9`. Inject queue insert failure and assert the row remains active. `TestDisableWorkflowReranksAndRetainsHistory` runs profile then ranking children, checks mode factor disappears and feedback/classification remain. `TestDisableDiffPinsItsWorkflowVersion` publishes unrelated ranking later and still returns the disable child's actual version. Foreign/missing IDs are indistinguishable not-found; deleted users cannot enqueue/recreate rows.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/preferences ./internal/feedback -run 'TestDisable'` → missing transactional disable/diff workflow behavior.
- [ ] **Step 3 — Implement transaction:** Acquire the existing lifecycle advisory lock for this user, verify user exists, then lock `(user_id,id)`. Inactive returns stored metadata without enqueuing. Active reads max published ranking (must be `>=1`), enqueues with `Queries.WithTx(tx).EnqueueJob` on the same transaction, kind `preferences.disable`, stage `preferences`, payload version `1`, key `adjustment:<id>:disable:v1`, max attempts `3`, user ID set, foreground priority `100` from catalog's migration. If catalog's `jobs.EnqueueInTx(ctx, queries, spec)` helper has landed, use that helper on the same transaction with the identical spec instead of bypassing its shared validation. Mark inactive with job ID/version/timestamp and commit. Do not use `queue.Enqueue` outside this transaction. Missing/foreign return `pgx.ErrNoRows`; failures roll back every mutation.
- [ ] **Step 4 — Implement worker/diff:** Root `preferences.disable` loads its server-owned adjustment, awaits required profile child key `adjustment:<id>:disable:profile:v1`, then required ranking child key `adjustment:<id>:disable:ranking:v1`; both existing payload version `1`, and all jobs carry `UserID`. When catalog's queue-priority API is present, set all new feedback/disable user root and descendant jobs to priority `100`, preserving foreground claim behavior. Record only bounded version/result metadata with the queue's existing fencing. Extend `feedback.Repository.RankingDiff` to resolve either user-owned feedback root or user-owned disable root by `(userID,previousVersion,jobID)`; use each workflow's resulting version from its known child job/result, never the latest unrelated ranking. For no `jobId`, preserve the old feedback-only compatibility behavior. Reuse common ranking comparison logic rather than duplicating it. Failed/dead children preserve published ranking, surface terminal failure, and allow polling retry without a new disable event.
- [ ] **Step 5 — Verify GREEN:** Repeat Step 2, then `GOWORK=off go test -race ./internal/preferences ./internal/feedback ./internal/jobs ./internal/httpapi ./internal/recommendations`. Include stale-worker fencing and deletion-race cases. Disabled records stay inactive even if their source feedback workflow resumes.

### Task 9: Align authenticated Go/TypeScript contracts and safe diagnostics

**Files:** Create `internal/httpapi/preference_handlers.go`, `internal/httpapi/preference_handlers_test.go`; modify `internal/httpapi/profile_handlers.go`, `router.go`, `feedback_handlers.go`, `ranking_handlers_test.go`, `feedback_handlers_test.go`, `internal/app/app.go`, `web/src/api/types.ts`, `client.ts`, `client.test.ts`.

**Interfaces:**

```go
type PreferenceAdjustmentService interface {
  List(context.Context,uuid.UUID,bool) ([]preferences.Adjustment,error)
  Disable(context.Context,uuid.UUID,uuid.UUID) (preferences.DisableAccepted,error)
}
// Define in internal/feedback/service.go, not in httpapi (avoid an import cycle).
type AdjustmentSummary struct { ID uuid.UUID `json:"id"`; Label string `json:"label"`; Effect string `json:"effect"`; Active bool `json:"active"` }
// feedback.RankingDiff gains Adjustment *AdjustmentSummary `json:"adjustment,omitempty"`.
// GET profile response embeds profile.Profile and adds non-null []AdjustmentView.
type AdjustmentView struct {
  ID uuid.UUID `json:"id"`; Dimension string `json:"dimension"`; Target preferences.Mode `json:"target"`
  Label string `json:"label"`; Effect string `json:"effect"`; Confidence float64 `json:"confidence"`
  Active bool `json:"active"`; CreatedAt time.Time `json:"createdAt"`
}
```

```ts
export type AdjustmentSummary = { id: string; label: string; effect: string; active: boolean }
export type PreferenceAdjustment = AdjustmentSummary & {
  dimension: 'mode'; target: string; confidence: number; createdAt: string
}
export type DisableAdjustmentAccepted = { adjustmentId: string; rebuildJobId: string; previousRankingVersion: number; alreadyInactive: boolean }
// PreferenceProfileData.adjustments: PreferenceAdjustment[]
// RankingDiff.adjustment?: AdjustmentSummary
// RecommendationFactors.negativeModePreference?: number
export function disablePreferenceAdjustment(id: string, csrfToken: string, signal?: AbortSignal): Promise<DisableAdjustmentAccepted>
export function getPreferenceAdjustments(includeInactive: boolean, signal?: AbortSignal): Promise<PreferenceAdjustment[]>
```

- [ ] **Step 1 — RED tests:** Handler tests assert DELETE unauthenticated `401`, missing/bad CSRF or origin `403`, bad UUID `400`, missing/foreign `404`, storage failure sanitized `500`, valid and repeat `202` with exact stored fields. Profile returns only this user's active views and `adjustments:[]` for none; diff includes applied adjustment summary only when a row exists. Add authenticated `GET /api/preference-adjustments?includeInactive=true` for local diagnostics (no mutation); it returns bounded public fields for active and inactive rules, no source IDs/text/classification/job payload. Unrecognized includeInactive values return `400`. Client tests assert same-origin credentials, DELETE method/header, URL encoding, empty-array normalization, and safe error handling.
- [ ] **Step 2 — Verify RED:** `GOWORK=off go test -race ./internal/httpapi -run 'Test(Preference|Disable|ProfileResponse|RankingDiff)'` and `pnpm --dir web run test -- src/api/client.test.ts` → absent routes/fields.
- [ ] **Step 3 — Implement:** Add dependency `Preferences PreferenceAdjustmentService`; DELETE uses `Auth.Middleware` plus `Auth.RequireCSRF`; GET uses auth and derives ownership solely from session. Compose profile response at HTTP boundary, preserving profile service signature and summary status. Use `effect="понижаем в рекомендациях"` and Task 1 server labels. Feedback diff includes row active state, including if disabled after submission; absence of adjustment cannot be presented as a new rule merely because broad classification.actionable is true. Add TS fields and normalize missing/null adjustments to `[]` for old responses; keep target opaque, no duplicate enum list. Preserve existing factor/weight names, calendar normalization and presentation overview normalization. General logging remains metadata-only and does not capture body or these IDs.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2, then `GOWORK=off go test -race ./internal/httpapi ./internal/feedback ./internal/preferences ./internal/auth` and all frontend checks listed in Task 13. Capture handler/job logs with canary feedback text and fake identifiers; assert absent. Keep fixture auth fail-closed tests unchanged.

### Task 10: Share feedback state and build the accessible compact dialog

**Files:** Create `web/src/components/rankingWorkflow.ts`, `rankingWorkflow.test.ts`, `useFeedbackWorkflow.ts`, `useFeedbackWorkflow.test.tsx`, `FeedbackDialog.tsx`, `FeedbackDialog.test.tsx`, `FeedbackLauncher.tsx`, `FeedbackLauncher.test.tsx`, `GamePoster.tsx`, `GamePoster.test.tsx`, `CompactGameIdentity.tsx`; modify `FeedbackForm.tsx`, `FeedbackForm.test.tsx`, and scoped rules in `web/src/styles.css`.

**Interfaces:**

```ts
export type FeedbackWorkflowOptions = {
  recommendationId: string; rankingVersion: number; csrfToken: string
  onRankingUpdated?: (diff: RankingDiff) => void
  submit?: typeof submitFeedback; readDiff?: typeof getRankingDiff
  wait?: (milliseconds: number, signal: AbortSignal) => Promise<void>
}
export type RankingWorkflow = { rebuildJobId: string; previousRankingVersion: number }
export function pollRankingWorkflow(workflow: RankingWorkflow, signal: AbortSignal,
  readDiff?: typeof getRankingDiff,
  wait?: (milliseconds: number, signal: AbortSignal) => Promise<void>): Promise<RankingDiff>
export function useFeedbackWorkflow(options: FeedbackWorkflowOptions): {
  phase: 'editing' | 'submitting' | 'polling' | 'succeeded' | 'error'
  accepted: FeedbackAccepted | null; diff: RankingDiff | null; error: string
  send(input: Omit<FeedbackInput, 'idempotencyKey'>): Promise<void>; retryStatus(): void
}
export type FeedbackLauncherProps = FeedbackWorkflowOptions & { gameName: string; coverImageUrl?: string }
export default function FeedbackLauncher(props: FeedbackLauncherProps): React.ReactElement
export type FeedbackDialogProps = FeedbackLauncherProps & { open: boolean; onClose(): void; returnFocusTo?: HTMLElement | null }
export default function FeedbackDialog(props: FeedbackDialogProps): React.ReactElement | null
export function GamePoster(props: { name: string; coverImageUrl?: string; size: 'grid' | 'list'; decorative?: boolean }): React.ReactElement
export function CompactGameIdentity(props: { item: Recommendation; size: 'grid' | 'list' }): React.ReactElement
```

- [ ] **Step 1 — RED tests:** Assert one launcher opens a named modal, puts focus inside, traps Tab/Shift+Tab, Escape/close returns focus to opener, background is inert, and title/poster/actions exist. Assert two mounted forms have unique label/counter IDs. 500 astral Unicode characters are allowed; 501 rejected with accessible error (do not rely on HTML UTF-16 `maxLength=500`). `Напомнить` sends only remind action, never fields. Duplicate same-frame clicks make one request; save retry uses the same key and frozen payload; changed payload uses a new key. Pending, success, low-confidence/no-adjustment copy, read failure/retry, dead rebuild, close/reopen while polling, unmount abort, and stale-ID replacement are tested. GamePoster handles absence, image error and new URL reset without removing link; correct dimensions/lazy-loading/alt rules are asserted.
- [ ] **Step 2 — Verify RED:** `pnpm --dir web run test -- src/components/FeedbackForm.test.tsx src/components/useFeedbackWorkflow.test.tsx src/components/FeedbackDialog.test.tsx src/components/FeedbackLauncher.test.tsx src/components/GamePoster.test.tsx` → missing shared behavior/components.
- [ ] **Step 3 — Implement workflow extraction:** Move existing submit/status retry logic into the hook, preserving dashboard FeedbackForm behavior. `pollRankingWorkflow` owns the existing `300ms`, multiplier `1.5`, maximum `3000ms` backoff; it returns only `succeeded` or `dead` diffs, propagates safe read errors, and aborts with AbortError. Both feedback and disable callers reuse it. Synchronous ref guard prevents duplicate requests before React rerenders. Keep accepted job/version and idempotency key across close/reopen and read retries; disable active submit, never repost when retrying status. A changed draft is a new command/key after a failed save. Keep polling in the mounted launcher/workflow owner while dialog is closed; on true unmount abort. Use `useId` for fields. Success rule copy derives from `diff.adjustment?.active`; otherwise explicitly say `Комментарий сохранён без изменения правил предпочтений`, retaining direct-action save confirmation.
- [ ] **Step 4 — Implement dialog and identity helpers:** Use native modal dialog behavior with explicit focus restoration; test real focus/inert behavior in Task 13's browser because jsdom alone is insufficient. One shared component implementation serves compact surfaces; CalendarPage owns one selected-item dialog so the grid does not render a textarea/modal per table cell. `FeedbackLauncher` defaults to self-contained mode for isolated consumers but also supports a context coordinator from Task 11 to open that single dialog. `CompactGameIdentity` uses only passed server `storeUrl`, verifies it is the canonical positive-AppID Steam URL before rendering an anchor, adds `aria-label="<name> — открыть в Steam, новая вкладка"`, and includes decorative GamePoster. Do not manufacture links from upstream text; invalid/missing canonical server URL renders unlinked text. Deterministic cover fallback uses the same existing initial-based design.
- [ ] **Step 5 — Verify GREEN:** Repeat Step 2 and frontend typecheck/lint/build. Include keyboard-only tests and a long Cyrillic/Latin name fixture. Preserve full-card inline form; do not restructure discovery/wishlist sections here.

### Task 11: Wire feedback, posters and links into every calendar presentation

**Prerequisite:** Presentation plan's overview/calendar contract and three-state `Все / Новые для меня / В желаемом` source filter are complete; Task 10 components and Task 9 contracts are available. This task owns calendar files after those patches, not concurrently with them.

**Files:** Modify `web/src/components/ReleaseCalendar.tsx`, `ReleaseBuckets.tsx`, `RecommendationCard.tsx` only for compact rendering, `web/src/pages/CalendarPage.tsx`, `CalendarPage.test.tsx`, `DashboardPage.tsx` only for callbacks, and scoped `web/src/styles.css`; add `web/src/components/CalendarFeedbackProvider.tsx`, `ReleaseCalendar.test.tsx`, `ReleaseBuckets.test.tsx`.

**Interfaces:** Extend `CalendarPageProps` with `csrfToken?: string` and `onRankingUpdated?: (diff:RankingDiff)=>void`. Define `CalendarFeedbackProvider({csrfToken,onRankingUpdated,children})` and `useCalendarFeedback(): { open(item:Recommendation, opener:HTMLElement): void } | null`. Extend `FeedbackLauncherProps` with optional `item?: Recommendation`; in provider context, the launcher calls the coordinator using that server item instead of mounting another dialog. `ReleaseCalendar` and `ReleaseBuckets` continue consuming their existing calendar/items props, add optional auth/callback props only if needed for non-provider use. Provider owns selected recommendation, accepted workflow and fallback focus target for the current calendar.

- [ ] **Step 1 — RED tests:** Parameterize day grid, exact-date list, month, quarter, year, comingSoon, and tba fixtures: each has poster/fallback, linked game name, correct external-link attributes, and `Оценить` launcher; exactly one dialog opens. Rerender with new ranking and removed source item while request is pending; accepted polling continues once and focus falls back to the calendar's visible view button if the opener disappears. After success callback/reload, assert visible month, grid/list mode, source filter, precision and minimum score are unchanged. Read failure/dead rerank retains old items and exposes retry status; no unverified success announcement. Calendar still places only precise day releases in date cells.
- [ ] **Step 2 — Verify RED:** `pnpm --dir web run test -- src/pages/CalendarPage.test.tsx src/components/ReleaseCalendar.test.tsx src/components/ReleaseBuckets.test.tsx` → missing launchers/posters and refresh wiring.
- [ ] **Step 3 — Implement:** Use CompactGameIdentity with `size='grid'` for date cells and `size='list'` for exact list/buckets; exact list compact RecommendationCard uses the same launcher/helper and 48×72 poster, not its full cover panel. Pass user CSRF and existing rankingUpdated callback from dashboard to CalendarPage. Keep CalendarPage mounted without a key tied to ranking version and keep filter/month state local; reload only data via presentation's existing overview/profile loader after succeeded diff. Extend the existing live ranking status region instead of announcing success from multiple hidden forms. Button placement remains separate from Steam links; no nested interactive controls. CSS uses fixed poster dimensions, `min-width:0`, wrapping names, and responsive layout that does not expand grid columns.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2 plus all frontend checks. Inspect desktop and 375px mobile views in Task 13; exercise month navigation, every source/precision filter and existing counts with feedback results. Anonymous component fixtures must not render actionable unauthenticated launchers.

### Task 12: Show, disable and diagnose preference corrections in the profile

**Files:** Create `web/src/components/PreferenceAdjustments.tsx`, `PreferenceAdjustments.test.tsx`; modify `PreferenceProfile.tsx`, `web/src/pages/DiagnosticsPage.tsx`, `DiagnosticsPage.test.tsx`, `DashboardPage.tsx` only for props, and scoped CSS.

**Interfaces:** `PreferenceAdjustments({adjustments,csrfToken,onRankingUpdated}:{adjustments:PreferenceAdjustment[];csrfToken:string;onRankingUpdated?:(diff:RankingDiff)=>void})`. Extend PreferenceProfile props with optional CSRF/callback; reuse `pollRankingWorkflow` from Task 10 with the disable response's `{rebuildJobId,previousRankingVersion}` metadata rather than inventing a feedback event for disable.

- [ ] **Step 1 — RED tests:** Active rules show `Учитываем ваши корректировки`, server human label/effect, and `Не учитывать`. First/repeat disable uses stored accepted workflow, disables repeat clicking, polls correct job/version, and refreshes once after success. A save error preserves the active row; a poll error exposes retry without a second DELETE. Disabled rows disappear from normal profile only after refreshed data, and remain visible in local diagnostics with `Не учитывается`. Empty/default-normalized adjustments render without crashes; inactive data accidentally supplied to profile is filtered out. No source feedback text/UUID is shown as user-facing copy.
- [ ] **Step 2 — Verify RED:** `pnpm --dir web run test -- src/components/PreferenceAdjustments.test.tsx src/pages/DiagnosticsPage.test.tsx` → missing section/actions/diagnostic rows.
- [ ] **Step 3 — Implement:** Render active list with accessible per-rule action label, call Task 9 DELETE and existing diff endpoint, retain pending/error state until status can be checked, then use the existing dashboard callback to reload profile and overview. Diagnostics explicitly requests `includeInactive=true` from the authenticated read route; failures remain a local diagnostic state and cannot break the main dashboard. Show human labels/status/time, never raw adjustment IDs or internal numeric weight. Do not expose positive-rule editing or delete history controls.
- [ ] **Step 4 — Verify GREEN:** Repeat Step 2 and all frontend checks. Confirm this task preserves presentation's discovery/wishlist layout and current profile summary/recency changes.

### Task 13: Prove the complete workflow, failure/privacy boundaries and release checks

**Files:** Modify `cmd/e2e-fakes/main.go`, `web/tests/e2e/smoke.spec.ts`, `docs/local-demo.md`, `README.md` only for the new local workflow/commands; extend existing backend integration tests from Tasks 3, 5, 7–9 as required. Keep the browser scenario in the existing smoke file because `make verify` names that file explicitly.

**Interfaces:** Fake Steam supplies reviewed category fixtures for source co-op, another co-op, single-player and unknown-mode games. Fake OpenAI feedback output uses the v2 required-nullable preference schema; test controls remain behind existing `TEST_HARNESS_MODE=1`, with no public production override. Existing other AI fake responses remain compatible with their unchanged schemas.

- [ ] **Step 1 — RED browser/integration tests:** Add a fixture-authenticated scenario that opens a calendar launcher by keyboard, submits `Не моё / не люблю коопы`, observes saved status before rebuild, then applied `Кооперативные игры`. Assert exactly one active adjustment and reranked matching game's factor/score drop at most five while source gets no own category factor, non-co-op/unknown stay unchanged, and history is retained. Repeat event/workflow and disable concurrently through API; verify one row/job and pinned version. Disable from profile, poll to completion, and assert factor removed. Verify poster error, long names, focus loop/return (including removed opener), 375px mobile width, immediate reminder, duplicate click, stale/pending/error/ranking preservation, and filter/month persistence. Capture relevant logs and assert feedback canaries/IDs/raw output absent; auth/CSRF tests remain fail-closed. Use safe synthetic IDs only in fixtures.
- [ ] **Step 2 — Verify RED:** After `make wait-test-db`, run the focused Go race packages and `pnpm --dir web exec playwright test tests/e2e/smoke.spec.ts` using the configured fixture services; expect the new assertion to fail before its missing wiring/fixture update. Do not accept unrelated old assertions failing from presentation changes; reconcile those to its intended behavior first.
- [ ] **Step 3 — Implement only remaining fixture/runbook gaps:** Update fake output format/categories and document local review steps: apply normal migrations, refresh Steam/category projection, submit explicit feedback, distinguish saved/no-rule from applied rule, inspect active/disabled corrections, retry status after rebuild failure, and disable without deleting history. Commands must target local/test services, not production or real accounts; use no secret values or personal data. Do not add new dependencies or unbounded catalog import.
- [ ] **Step 4 — Focused GREEN and full verification:** Run `codegraph sync .`, then `codegraph affected` with changed source paths. Run `GOWORK=off go test -race ./internal/preferences ./internal/steam ./internal/catalog ./internal/ai ./internal/profile ./internal/recommendations ./internal/feedback ./internal/httpapi ./internal/jobs ./internal/store`, with isolated TEST_DATABASE_URL; `pnpm --dir web run test`, `pnpm --dir web run typecheck`, `pnpm --dir web run lint`, `pnpm --dir web run build`; then `make verify`. If pinned Chromium download is unavailable, explicitly use the documented existing executable, e.g. `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' make verify` only after checking that executable exists, and report fallback. Require zero failures, no skipped DB tests, no sqlc drift, and passed smoke/Docker build.
- [ ] **Step 5 — Handoff evidence:** Run `git diff --check`, inspect the complete diff without touching unrelated edits, and report test output, migration number, preserved old rankings, actual algorithm/prompt versions, and any browser fallback. Do not claim completion from a worker report alone. Commit only if subsequently authorized, with exact owned paths; never stage `.env`, generated CodeGraph data, build output, or another task's changes.

## Acceptance and Failure Coverage Audit

| Spec requirement | Owning tasks / concrete proof |
|---|---|
| `.70+` co-op dislike creates exactly one rule; resume idempotency | 4–5: strict schema/cross-field tests, transaction retry/resume tests |
| Direct source contribution once; no own category penalty | 6 direct-count integration; 7 source-exclusion/max tests |
| Other co-op soft delta at most five, eligible and positive baseline | 7: score `15 → 10`, independent eligibility; 13 browser fixture |
| Non-co-op/unknown no penalty; aliases canonical; no title/LLM inference | 1, 3, 7: mapping and missing-evidence tests |
| Repetition and broad/specific overlap use max | 7: `.4/.9 → .9`, exclusion before max |
| Disable removes factor atomically after rerank; immutable history retained | 8, 12–13: transactional enqueue, workflow/pinned version, profile UI |
| Low/neutral/unknown/refusal/malformed/incomplete/unavailable free text no rule | 4–5 table matrix; unavailable reason fallback separately tested |
| Reason multiplayer works without OpenAI | 5 fallback provenance/allowlist tests |
| Grid/list/all five buckets share accessible feedback | 10–11 parameterized tests and real browser modal checks |
| Refresh preserves calendar controls; pending/stale/dead/read retry correct | 10–13 state and browser tests |
| Poster/fallback and safe direct links on every item | 10–11 media/link tables; 13 long name/mobile/image-failure tests |
| Factor/weight persistence, algorithm/build identity, active digest and replay | 7 integration + request-prefix replay/concurrency tests |
| Ownership, auth/CSRF, tombstones, concurrent disable/deletion | 2, 8–9, 13 negative boundary tests |
| No feedback/identifier/raw-output logging, no secret/model-input leakage | 4–5, 9, 13 captured request/log canary tests |
| Last successful Steam modes and last published ranking survive failures | 3 rollback/upstream matrix; 7–8 failed-build/publish tests |
| Disabled rules available only through explicit local diagnostics view | 9 safe authenticated inactive read; 12 diagnostic-only rendering |

## Observed Baseline Mismatches and Execution Notes

- Current feedback structured schema and prompt are v1 and have no preference object. Current service validates taxonomy/polarity/confidence but trusts injected classifiers' citations; repeat validation is required.
- `profile.LoadSignals` adds classification polarity to direct event counts; the existing integration test explicitly expects two likes from one event. Replace that expectation; do not preserve it as an acceptance criterion.
- Parallel work currently changes profile algorithm to `profile-v2`, `BuildVectors` to accept a clock, last-played fields and profile-summary-v3. Re-read these files before Task 6; this plan's count correction is additive.
- Current ranking is `ranking-v1`, wishlist weight `30`; presentation changes that policy separately. This plan adds only a `-5` mode factor and must retain the presentation result when integrated.
- `publicEvidence` currently keeps only positive contributions, so merely adding a negative factor would never explain the correction. Task 7 explicitly adds persisted human evidence and compatible presentation validation.
- Current ranking build key is the worker idempotency key and does not include algorithm/adjustment inputs; retry identity and content identity both need Task 7's explicit handling.
- Current ranking diff joins only `feedback_events`; a new disable root cannot be polled without Task 8's explicit ownership-aware source resolution.
- Current feedback root/child enqueues do not consistently carry user IDs. Set them when touching this workflow; otherwise new preference work could evade lifecycle job deletion/fencing.
- Existing Steam snapshots already contain `Categories`; no supported store-tag source exists. Category projection must be updated by the successful metadata writer and backfilled from trusted stored snapshots, not populated from AI modes.
- Current compact cards hide FeedbackForm; grid/buckets render names without posters or links. Current counter has a repeated DOM ID and native maxLength counts UTF-16; shared dialog/Unicode tests must address both.
- Current calendar source filter is a wishlist checkbox; Task 11 consumes the presentation plan's three-state filter instead of implementing a competing filter. `coverImageUrl`, canonical `storeUrl`, and `steamAppId` already exist and need no schema change.
- The broad `multiplayer` evidence value is deliberately represented in the shared/DB vocabulary for Steam's generic Multi-player category. It does not imply a subtype, and a co-op adjustment does not match it. This resolves the generic-category edge of the spec without title inference.
- Migration number `00010` is a coordination reservation, not evidence that `00009` is already applied. Check the actual migration sequence before execution.

Self-review completed: all 14 acceptance bullets and failure/privacy bullets map to the tasks above; interfaces use one vocabulary and one disable response; no source implementation or existing spec is part of this planning change.
