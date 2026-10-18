# Activity Profile and Preference Overlay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Separate the played-activity profile from explicit preference feedback, keep the last published profile visible during refresh, and let the latest effective reactions adjust deterministic recommendations immediately.

**Architecture:** Keep `preference_profiles` as the versioned Activity Profile store, but split deterministic draft construction from atomically fenced summary publication. Add a deterministic, versioned Preference Overlay for latest direct reactions and effective corrections; every ranking binds one exact Activity Profile and Overlay pair, while application entry uses a backend-enforced 24-hour Steam freshness decision. Preserve legacy reads during rollout, last-known-good state on optional-upstream failure, and the existing PostgreSQL job/idempotency boundaries.

**Tech Stack:** Go 1.27.2 (`GOWORK=off`), PostgreSQL with pgvector, PostgreSQL-backed jobs, goose migrations, sqlc v1.31.1, React 19, TypeScript, Vitest/Testing Library, Playwright; existing dependencies only.

**Spec:** `docs/superpowers/specs/2026-10-18-activity-profile-preference-overlay-design.md`

## Global Constraints

- The Activity Profile uses only saved positive play evidence: `playtime_minutes > 0`, `recently_played_minutes > 0`, `achievement_summaries.achieved > 0`, or `user_game_completion_projections.weight > 0`.
- Wishlist, direct feedback, feedback classification, preference adjustments, unplayed ownership, and catalog-only membership have zero direct or indirect Activity Profile, affinity-anchor, explanation-anchor, and confidence weight.
- Completion remains historical Activity Profile evidence even after a dislike; feedback may counter it only in ranking and must never set `completion_overridden` in the Activity Profile or its summary evidence.
- The Preference Overlay uses only the latest effective direct `like`, `dislike`, or legacy `exclude` per game, ordered by `created_at DESC, id DESC`; `remind_me` never supersedes a direct reaction.
- Effective corrections must be enabled and sourced by the current effective direct negative reaction; superseded and disabled corrections stay in immutable history but do not affect default API reads or ranking.
- The Preference Overlay is deterministic, makes no OpenAI calls, persists no fabricated zero vectors, and records only safe aggregate metadata and hashes without raw free text or identifiers.
- Final ranking remains deterministic. Use weights `activitySimilarity=25`, `positivePreferenceSimilarity=10`, `negativePreferenceSimilarity=-10`, `positiveDirectFeedback=20`, `negativeDirectFeedback=-30`, `negativeModePreference=-5`, and `wishlist=0`; clamp the final score to `[0,100]` and retain stable name/game-ID tie-breakers.
- OpenAI may enrich game semantics and write grounded prose, but it cannot choose candidate eligibility, scores, weights, or ordering. Missing vectors preserve exact deterministic effects and candidate membership.
- A published profile has a nonblank validated summary, `publication_state='published'`, non-null `published_at`, and `summary_source IN ('openai','deterministic','legacy')`; readers never mix fields from different profile versions.
- A newest `building` profile exposes the previous published document with `updating=true`; `failed` preserves it with `updating=false`; newest `insufficient` is authoritative and returns no visible profile.
- Steam/OpenAI failure preserves last-known-good state. Private, unavailable, malformed, and failed Steam responses must never become successful empty activity.
- The authenticated owner entry freshness boundary is exactly 24 hours using server time and the newest succeeded root `steam.sync.updated_at`; manual sync remains available regardless of freshness, and active workflows are reused.
- Keep single-user allowlisting, owner-only authenticated mutations, CSRF protection, demo read-only behavior, deletion tombstones/fencing, sanitized diagnostics, release-date precision, and SMTP `ambiguous` semantics unchanged.
- Use PostgreSQL/pgvector as the only persistent service. The spec expected migration 00017, but the live tree already reserves 00017 for `00017_steam_search_release_window.sql`; add the next free `db/migrations/00018_activity_profile_preference_overlay.sql`. Never rewrite an applied or parallel migration or hand-edit `internal/store/sqlc/`.
- Keep Go/API/TypeScript/normalization/contracts aligned. Use only isolated `TEST_DATABASE_URL` and Compose `test-db`; never clean the application database.
- The profile summary prompt target remains 650–900 Unicode characters with tolerant nonblank/valid-UTF-8/maximum-4000-rune sanity validation.
- Inline `[signal-*]` cleanup is an adjacent bounded follow-up from spec section 19. It is not silently included in this migration; do not change prompt citation cleanup or wishlist/direct-like wording here.
- Preserve unrelated dirty files, especially the current `RecommendationCard`, `DashboardPage`, stylesheet, and Steam search plan edits. Do not modify `docs/steam-release-recommender-idea.md`, push, deploy, or expose secrets.

## Review Focus

1. Two direct reactions share a timestamp or a reminder follows one: `(created_at DESC, id DESC)` must select exactly one direct action and ignore the reminder; Task 4 adds `TestEffectiveDirectReactionUsesCreatedAtThenIDAndIgnoresReminder`.
2. A user once had a published profile and later has no qualifying activity: newest `insufficient` must suppress the old profile rather than resurrect it; Task 3 adds `TestLatestPublishedProfileTreatsInsufficientAsAuthoritativeEmpty`.
3. Feedback and Steam rebuilds overlap: every published ranking must reference one coherent exact profile/overlay pair and a build-key collision must not accept different inputs; Tasks 5 and 6 add `TestRankingSnapshotBindsCoherentProfileOverlayPairDuringRace`.
4. A summary response arrives after lease loss, terminal failure, or prior publication: it must not publish or overwrite the draft; Task 3 adds `TestProfilePublicationRejectsLateLeaseAndTerminalResult`.
5. Multiple tabs enter at exactly the freshness boundary while preflight fails or another sync becomes active: create at most one job, reuse active work, and keep saved UI data renderable; Task 7 adds `TestRefreshIfStaleBoundaryConcurrentTabsAndPreflightFailure`.

---

## File Structure and Delivery Boundaries

| Unit | Files and responsibility | Depends on |
|---|---|---|
| Storage boundary | `db/migrations/00018_activity_profile_preference_overlay.sql`, `db/queries/profiles.sql`, new `db/queries/preference_overlays.sql`, `db/queries/recommendations.sql`, `db/queries/feedback.sql`, `db/queries/jobs.sql`, generated `internal/store/sqlc/` | Task 1 |
| Activity Profile model | `internal/profile/vectors.go`, `repository.go`, `service.go`, focused tests | Task 1 |
| Summary publication | `internal/ai/repository.go`, `service.go`, `types.go`; profile published-read tests | Tasks 1–2 |
| Preference Overlay | new `internal/preferences/overlay.go`, `overlay_repository.go`, tests; existing preference repository/service | Task 1 |
| Ranking integration | `internal/recommendations/types.go`, `scoring.go`, `retrieval.go`, `service.go`, `evidence.go`, `mode_preferences.go`, tests | Tasks 2, 4 |
| Workflow split | `internal/feedback/service.go`, `internal/preferences/service.go`, `internal/sync/service.go`, job tests | Tasks 2, 4–5 |
| Entry freshness | `internal/sync/service.go`, `db/queries/jobs.sql`, `internal/httpapi/sync_handlers.go`, `router.go`, tests | Task 1 |
| Profile/API contracts | `internal/httpapi/profile_handlers.go`, preference handlers/tests, `web/src/api/types.ts`, `client.ts` | Tasks 3–5 |
| UI state | `web/src/api/hooks.ts`, `PreferenceProfile.tsx`, `PreferenceAdjustments.tsx`, `DashboardPage.tsx`, styles/tests | Tasks 7–8 |
| Rollout/lifecycle | new `internal/profileoverlay/backfill.go`, `internal/app/app.go`, `internal/httpapi/data_handlers.go`, `internal/observability/events.go`, tests | Tasks 1–6 |
| Acceptance | new `web/tests/e2e/activity-profile-overlay.spec.ts`, optional focused `cmd/e2e-fakes/main.go` fixtures, and `docs/local-demo.md` only if commands or behavior changed | All prior tasks |

Before implementation, read the spec and this plan, run `codegraph status .`, sync only if stale, then run `codegraph context "activity profile preference overlay" -p .`. Before changing shared symbols inspect `codegraph impact LoadSignals -p .`, `codegraph impact RankingInputs -p .`, `codegraph impact RegisterHandlers -p .`, and the exact symbol node. After every source task, run `codegraph sync .`, then `codegraph affected <changed-files...> -p .` to supplement the focused tests below. Never commit `.codegraph` data.

Start the isolated database with `make wait-test-db`. Database test commands below use:

```bash
TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable'
```

Each RED command must fail for the named missing behavior, not for unavailable infrastructure. Each task ends with a review of its diff, focused tests, `git diff --check`, and a scoped commit; do not include unrelated working-tree changes.

### Task 1: Add publication, overlay, provenance, and effective-state storage

**Files:** Create `db/migrations/00018_activity_profile_preference_overlay.sql`, `db/queries/preference_overlays.sql`; modify `db/queries/profiles.sql:1-6`, `db/queries/recommendations.sql:1-12`, `db/queries/feedback.sql:1-22`, `db/queries/jobs.sql:1-120`, `internal/store/migration_upgrade_test.go:1-end`; generate `internal/store/sqlc/` through `make sqlc`.

**Interfaces:**

- Produces sqlc queries `GetLatestActivityProfileState(user_id)`, `GetLatestPublishedActivityProfile(user_id)`, `GetActivityProfileByID(id,user_id)`, `GetActivityProfileByBuildKey(user_id,build_key)`, `InsertActivityProfile(...)`, `InsertActivityProfileEmbedding(...)`, `PublishActivityProfile(...)`, and `FailBuildingActivityProfile(...)`.
- Produces `ListEffectiveDirectReactions(user_id)`, `ListEffectivePreferenceAdjustments(user_id)`, `GetLatestPreferenceOverlay(user_id)`, `GetPreferenceOverlayByBuildKey(user_id,build_key)`, `InsertPreferenceOverlay(...)`, `InsertPreferenceOverlayEmbedding(...)`.
- Produces internal `activity_profile_evidence` rows keyed by `(profile_id,game_id)` with the draft-time playtime/recent/last-played/achievement/completion snapshot; this keeps summary evidence version-coherent without placing game IDs in aggregate metadata.
- Produces ranking persistence fields `recommendation_runs.preference_overlay_id` and `recommendations.preference_overlay_id`; legacy rows may be null, all new writes must be non-null.
- Produces `GetLatestSucceededRootSteamSync(user_id)` and `GetActiveRootSteamSync(user_id)`; root means `kind='steam.sync' AND parent_job_id IS NULL`.

- [ ] **Step 1 — RED migration tests:** Add `TestActivityProfilePreferenceOverlayMigration` and `TestActivityProfilePublicationChecks` in `internal/store/migration_upgrade_test.go`. Upgrade from migration 16 with one nonblank and one blank legacy profile; assert nonblank → `published/published_at=created_at/legacy`, blank → `failed`, exact publication checks reject inconsistent rows, legacy recommendation provenance remains nullable, overlay version/build-key uniqueness holds, and down migration removes only new objects/columns.
- [ ] **Step 2 — RED lifecycle tests:** In the same test file add `TestPreferenceOverlayDeletionGuards`. Assert user deletion cascades activity evidence, overlays, and overlay embeddings; inserts into either overlay table after a tombstone fail closed; shared game embeddings survive.
- [ ] **Step 3 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/store -run 'Test(ActivityProfilePreferenceOverlayMigration|ActivityProfilePublicationChecks|PreferenceOverlayDeletionGuards)'`. Expected: missing migration objects/columns and checks.
- [ ] **Step 4 — Implement migration:** Add publication columns using the nullable-add/backfill/check/default sequence from spec section 6. Create `activity_profile_evidence` with profile/game FKs and copied normalized activity fields; it is internal and contains no wishlist/feedback columns. Create `preference_overlays` and `preference_overlay_embeddings` with the exact columns, vector dimension 1536, polarity check, unique keys, FKs, and indexes. Add nullable overlay FKs to both recommendation tables. Add tombstone-aware insert triggers for the overlay row and its embedding lookup; retain cascade deletion.
- [ ] **Step 5 — Implement effective-state and profile queries:** `ListEffectiveDirectReactions` uses `DISTINCT ON (rec.game_id)` and `ORDER BY rec.game_id,f.created_at DESC,f.id DESC`, filters direct actions, and returns saved latest embedding/hash when present. `ListEffectivePreferenceAdjustments` joins only the same effective negative source feedback and `active=true`. Profile publish/fail queries require the row to remain `building`; publish updates summary/model/hash/source/state/timestamp in one statement.
- [ ] **Step 6 — Generate sqlc:** Run `make sqlc`; inspect the generated diff, do not edit it. Ensure nullable database fields map to pgtype values and vector rows use pgvector types.
- [ ] **Step 7 — Verify GREEN and review gate:** Repeat Step 3, run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/store`, `codegraph sync .`, `codegraph affected db/migrations/00018_activity_profile_preference_overlay.sql db/queries/profiles.sql db/queries/preference_overlays.sql db/queries/recommendations.sql db/queries/feedback.sql db/queries/jobs.sql -p .`, and `git diff --check`.
- [ ] **Step 8 — Commit:** `git add db/migrations/00018_activity_profile_preference_overlay.sql db/queries/profiles.sql db/queries/preference_overlays.sql db/queries/recommendations.sql db/queries/feedback.sql db/queries/jobs.sql internal/store/migration_upgrade_test.go internal/store/sqlc && git commit -m "feat: add activity profile overlay storage"`.

### Task 2: Build activity-only profile drafts and authoritative insufficient snapshots

**Files:** Create `internal/profile/repository_integration_test.go`; modify `internal/profile/vectors.go:11-163`, `internal/profile/repository.go:31-184`, `internal/profile/service.go:16-131`, `internal/profile/vectors_test.go:1-end`, `internal/profile/service_test.go:1-end`.

**Interfaces:**

```go
const AlgorithmVersion = "activity-profile-v5"
type PublicationState string // building, published, failed, insufficient
type SummarySource string // openai, deterministic, legacy
type BuildRequest struct { UserID uuid.UUID; SourceWorkflowID uuid.UUID; BuildKey string }
type BuildResult struct { Profile Profile; SummaryRequired bool }
type Signal struct { GameID uuid.UUID; Vector []float32; PlaytimeMinutes, RecentMinutes int64; LastPlayedAt time.Time; AchievementRatio, CompletionWeight float64 }
type Vectors struct { Positive []float32; PositiveStrength, Confidence float64; Evidence int; SignalCounts map[string]int }
func BuildVectors([]Signal, time.Time) Vectors
func (*Service) Build(context.Context, BuildRequest) (BuildResult, error)
func (*SQLRepository) LoadActivitySignals(context.Context, uuid.UUID) ([]Signal, error)
func (*SQLRepository) BuildDraft(context.Context, BuildRequest, time.Time, string) (Profile, bool, error)
```

- [ ] **Step 1 — RED activity selection tests:** Add `TestLoadActivitySignalsExcludesWishlistFeedbackAndUnplayedOwnership`. Seed played, recent-only, achieved-only, completed-only, unplayed owned, wishlist-only, feedback-only, and catalog-only games. Assert only the first four load, a disliked completed game retains completion weight, and no feedback/wishlist fields exist at the domain boundary.
- [ ] **Step 2 — RED vector tests:** Replace wishlist/feedback centroid expectations with `TestBuildVectorsUsesOnlyPlayedActivity`, `TestEachActivityClassQualifiesIndependently`, and `TestBuildVectorsWritesNoNegativeActivityVector`. Assert confidence/evidence count use vector-bearing eligible activity only and deterministic order remains stable.
- [ ] **Step 3 — RED draft/insufficient tests:** Add `TestBuildCreatesBuildingDraftAndQueuesSummary`, `TestActivityWithoutEmbeddingCreatesBuildingDraftWithoutProfileVector`, and `TestBuildCreatesInsufficientWithoutEmbeddingOrSummaryJob`. Assert any qualifying activity creates a building draft and snapshotted `activity_profile_evidence`, even when no game vector exists; only zero qualifying activity is insufficient. Metadata contains aggregate signal counts and source workflow identity but no game/user IDs; the same build key reuses one version.
- [ ] **Step 4 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/profile -run 'Test(LoadActivitySignals|BuildVectors|EachActivityClass|BuildCreates)'`. Expected: mixed inputs still contribute, negative vector exists, and publication state is absent.
- [ ] **Step 5 — Implement activity-only loading and vectors:** Restrict SQL at source to the four eligibility predicates, left-join only the latest saved embedding, preserve historical recency/completion formulas, ignore feedback override, remove wishlist/feedback signal kinds, count activity classes across all eligible signals, calculate confidence/evidence count only from usable-vector signals, set semantic coverage denominator to four, and never write a negative profile embedding.
- [ ] **Step 6 — Implement one-transaction fenced draft build:** `BuildDraft` opens one transaction, acquires `profile:<user>`, loads the eligible snapshot, computes vectors, deduplicates `BuildKey = source workflow identity + AlgorithmVersion + completion projection version`, and writes the profile plus `activity_profile_evidence` plus optional positive embedding before commit. Insert `building` whenever at least one eligible activity signal exists, even with no vector; insert `insufficient` only when the eligible set is empty. Check the users row/tombstone before commit.
- [ ] **Step 7 — Implement job result contract:** Bump profile job payload/version only if needed for `source_workflow_id`; record `{profileId, profileVersion, publicationState, summaryRequired}`. Queue `ai.profile_summary` only for a newly/reused `building` draft that lacks a completed summary job.
- [ ] **Step 8 — Verify GREEN and review gate:** Repeat Step 4, run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/profile`, `codegraph sync .`, `codegraph affected internal/profile/vectors.go internal/profile/repository.go internal/profile/service.go -p .`, and `git diff --check`.
- [ ] **Step 9 — Commit:** `git add internal/profile && git commit -m "feat: build activity-only profile drafts"`.

### Task 3: Publish summaries atomically and serve stale-while-revalidate profiles

**Files:** Modify `internal/ai/types.go:87-98`, `internal/ai/prompts.go:3-15`, `internal/ai/prompts_test.go`, `internal/ai/repository.go:21-279`, `internal/ai/service.go:43-52,212-218,340-376`, `internal/ai/service_test.go:1-end`, `internal/profile/repository.go:126-184`, `internal/profile/service.go:23-58,91-93`, `internal/profile/service_test.go:1-end`; add `internal/ai/profile_fallback.go`, `internal/ai/profile_fallback_test.go`.

**Interfaces:**

```go
type ActivityFacts struct { PlayedGames, RecentlyPlayedGames, GamesWithAchievements, CompletedGames int }
func DeterministicActivitySummary(ActivityFacts) string
type Profile struct { /* existing fields */ PublicationState PublicationState; SummarySource SummarySource; PublishedAt *time.Time; Updating bool }
func (*SQLRepository) LatestPublishedProfile(context.Context, uuid.UUID) (Profile, error)
func (*SQLRepository) PublishProfileSummary(context.Context, jobs.Lease, uuid.UUID, string, string, ProfileSummary, SummarySource) error
func (*SQLRepository) HandleTerminalProfileSummaryFailure(context.Context, jobs.Lease, uuid.UUID, string) (publishedFallback bool, err error)
```

- [ ] **Step 1 — RED evidence tests:** Update AI repository and prompt tests so `LoadProfileEvidence(profileID)` returns only games represented by that draft's eligible activity set and exposes playtime/recent/achievement/completion fields only. Assert no wishlist, direct feedback, correction, or `completion_overridden` field/value can enter evidence or the profile instruction; retain the 650–900 target and do not fold in the separate inline-citation cleanup.
- [ ] **Step 2 — RED read-state tests:** Add `TestLatestPublishedProfileKeepsPublishedWhileBuilding`, `TestLatestPublishedProfileClearsUpdatingAfterFailure`, and Review Focus test `TestLatestPublishedProfileTreatsInsufficientAsAuthoritativeEmpty`. Assert all returned summary/confidence/evidence/version/source timestamps belong to one published row.
- [ ] **Step 3 — RED publication/fallback tests:** Add `TestDisabledOpenAIPublishesDeterministicActivityFallback`, `TestTerminalSummaryFailurePublishesFallbackOnlyOnFirstRun`, and Review Focus test `TestProfilePublicationRejectsLateLeaseAndTerminalResult`. The fallback may report only the four aggregate activity counts; assert it contains no game title, genre, mechanic, wishlist, or reaction claim.
- [ ] **Step 4 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/ai ./internal/profile -run 'Test(LoadProfileEvidence|ProfileInstructions|LatestPublishedProfile|DisabledOpenAI|TerminalSummary|ProfilePublication)'`. Expected: disabled capability leaves summary blank and newest draft hides v1.
- [ ] **Step 5 — Implement atomic publication:** Pass `jobs.Lease` into publication. In one transaction verify the active job ID/worker/fencing token/lease, require draft state `building`, write summary/model/input hash/source/state/published timestamp together, and affect exactly one row. A cached valid summary must still complete publication rather than returning early.
- [ ] **Step 6 — Implement disabled and terminal fallback:** On `ErrCapabilityDisabled`, publish `DeterministicActivitySummary` immediately with source `deterministic`. On the final retry (`lease.Attempts >= lease.MaxAttempts`), publish deterministic fallback and return success only when no older published profile exists; otherwise mark the draft failed under the same fence and return the original error so the job is dead. Nonterminal errors leave `building` for retry.
- [ ] **Step 7 — Implement public read semantics:** Inspect newest profile state first. Return newest published; for building return newest older published with `Updating=true`; for failed return older published with false; for insufficient return `pgx.ErrNoRows`. `LatestVectorComplete` remains a separate internal method and may return building/insufficient for ranking without exposing draft prose.
- [ ] **Step 8 — Verify GREEN and review gate:** Repeat Step 4; run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/ai ./internal/profile`, `codegraph sync .`, `codegraph affected internal/ai/prompts.go internal/ai/repository.go internal/ai/service.go internal/profile/repository.go internal/profile/service.go -p .`, and `git diff --check`.
- [ ] **Step 9 — Commit:** `git add internal/ai internal/profile && git commit -m "feat: publish activity profiles atomically"`.

### Task 4: Build deterministic latest-reaction Preference Overlays

**Files:** Create `internal/preferences/overlay.go`, `internal/preferences/overlay_repository.go`, `internal/preferences/overlay_test.go`, `internal/preferences/overlay_repository_integration_test.go`; modify `internal/preferences/service.go:1-56`, `internal/preferences/repository.go:14-84`, `internal/preferences/disable_integration_test.go:1-end`, `internal/feedback/repository_integration_test.go:1-end`.

**Interfaces:**

```go
const OverlayAlgorithmVersion = "preference-overlay-v1"
const OverlayJobKind = "preferences.rebuild"
type DirectReaction struct { FeedbackID, GameID uuid.UUID; Action string; CreatedAt time.Time; EmbeddingHash string; Vector []float32 }
type OverlaySnapshot struct { Reactions []DirectReaction; Corrections []Adjustment }
type OverlayVectors struct { Positive, Negative []float32; PositiveSources, NegativeSources, MissingVectors, EffectiveCorrections int; InputContentHash string }
type Overlay struct { ID, UserID uuid.UUID; Version int64; AlgorithmVersion, BuildKey, InputContentHash string; Positive, Negative []float32; Metadata OverlayMetadata; CreatedAt time.Time }
func BuildOverlay(OverlaySnapshot) OverlayVectors
func (*SQLRepository) LoadOverlaySnapshot(context.Context, uuid.UUID) (OverlaySnapshot, error)
func (*SQLRepository) RebuildOverlay(context.Context, uuid.UUID, string) (Overlay, error)
func (*SQLRepository) LatestOverlay(context.Context, uuid.UUID) (Overlay, error)
func (*Service) EnsureCurrent(context.Context, uuid.UUID) (Overlay, error)
```

- [ ] **Step 1 — RED effective-reaction tests:** Add Review Focus test `TestEffectiveDirectReactionUsesCreatedAtThenIDAndIgnoresReminder`, plus `TestDislikeThenLikeProducesOnlyPositiveOverlay`, `TestLikeThenDislikeProducesOnlyNegativeOverlay`, and `TestIdempotentFeedbackDoesNotStack`. Assert deterministic ordering by game/feedback ID.
- [ ] **Step 2 — RED correction/vector tests:** Add `TestSupersededAndDisabledCorrectionsAreNotEffective`, `TestMissingEmbeddingPreservesDirectEffectWithoutVector`, and `TestOverlayUsesCentroidsAndNoFabricatedZeroRows`. Repeated corrections for one target retain `max(confidence*strength)` in structured metadata; they do not add source-game embeddings beyond the direct reaction.
- [ ] **Step 3 — RED persistence tests:** Add `TestOverlayBuildIsVersionedIdempotentAndDeletionFenced`. Assert stable canonical empty hash, one empty overlay version with no embeddings, same build key/hash reuses version, changed effective input advances once, and retries cannot recreate data after deletion.
- [ ] **Step 4 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/preferences ./internal/feedback -run 'Test(EffectiveDirect|DislikeThenLike|LikeThenDislike|IdempotentFeedback|Superseded|MissingEmbedding|OverlayBuild)'`.
- [ ] **Step 5 — Implement builder and one-transaction repository:** Build centroids from available saved embeddings once per effective reaction. `RebuildOverlay` opens one repeatable-read transaction, acquires distinct `overlay:<user>`, loads effective reactions/corrections, computes the canonical hash, and saves/reuses the overlay before commit. Hash input includes effective feedback identity/action/game, embedding content hash, and effective correction values in deterministic order, but persisted metadata contains counts only. Check the tombstone before commit.
- [ ] **Step 6 — Register job and empty boundary:** `preferences.rebuild` accepts `{version:1,user_id}`; under a repeatable-read snapshot load effective reactions/corrections, build/save, and record `{preferenceOverlayId, preferenceOverlayVersion, inputContentHash}`. `EnsureCurrent` uses the same idempotent builder so a new user gets a durable empty overlay before the first ranking. Neither path calls AI.
- [ ] **Step 7 — Verify GREEN and review gate:** Repeat Step 4, run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/preferences ./internal/feedback`, `codegraph sync .`, `codegraph affected internal/preferences/overlay.go internal/preferences/overlay_repository.go internal/preferences/service.go -p .`, and `git diff --check`.
- [ ] **Step 8 — Commit:** `git add internal/preferences internal/feedback/repository_integration_test.go && git commit -m "feat: build deterministic preference overlays"`.

### Task 5: Bind Activity Profile and Overlay into deterministic retrieval and scoring

**Files:** Modify `internal/recommendations/types.go:11-49,57-125,187-213`, `internal/recommendations/mode_preferences.go:15-79`, `internal/recommendations/retrieval.go:24-192,320-664,744-1036`, `internal/recommendations/scoring.go:1-35`, `internal/recommendations/evidence.go:46-380,507-521`, `internal/recommendations/service.go:33-150,450-571,573-end`; modify/add `internal/recommendations/scoring_test.go`, `repository_integration_test.go`, `union_integration_test.go`, `evidence_test.go`, `build_key_test.go`, `build_key_integration_test.go`, and contract fixtures in `internal/httpapi/ranking_handlers_test.go`.

**Interfaces:**

```go
const AlgorithmVersion = "ranking-v5-activity-overlay"
const ActivityNeighborLimit = 100
const PreferenceNeighborLimit = 40
type RankingSource struct { Activity profile.Profile; Overlay preferences.Overlay }
type RankingRequest struct { UserID uuid.UUID; ProfileBuildKey string; RequestKey string }
type RankingInputs struct { Source RankingSource; Candidates []Candidate; EffectiveAdjustments []preferences.Adjustment; Corpus EvidenceCorpus }
type Factors struct { ActivitySimilarity, PositivePreferenceSimilarity, NegativePreferenceSimilarity, PositiveDirectFeedback, NegativeDirectFeedback, NegativeModePreference, Wishlist, TagAffinity, PlaytimeAffinity, AchievementAffinity, RecentActivityAffinity, CompletionAffinity, Novelty, DateUncertainty, Confidence float64 }
type Weights struct { ActivitySimilarity, PositivePreferenceSimilarity, NegativePreferenceSimilarity, PositiveDirectFeedback, NegativeDirectFeedback, NegativeModePreference, Wishlist, TagAffinity, PlaytimeAffinity, AchievementAffinity, RecentActivityAffinity, CompletionAffinity, Novelty, DateUncertainty float64 }
func RankingBuildKey(requestKey, algorithm string, profile profile.Profile, overlay preferences.Overlay) string
func (*SQLRepository) ResolveRankingInputs(context.Context, RankingRequest, int) (RankingInputs, error)
func (*SQLRepository) SaveRanking(context.Context, uuid.UUID, RankingSource, []Recommendation, string) ([]Recommendation, int64, bool, error)
```

- [ ] **Step 1 — RED scoring tests:** Add `TestOverlayWeightsAreBoundedAndWishlistIsZero`. Assert exact weights from Global Constraints, positive overlay max contribution is less than activity max, negative similarity reduces but never excludes, direct dislike/exclude controls eligibility, a later like restores eligibility, and score/tie behavior stays deterministic.
- [ ] **Step 2 — RED retrieval-union tests:** Add `TestRetrievalUnionIncludesPositiveOverlayNeighborsOnly`. Assert activity leg limit 100, positive overlay leg limit 40, negative overlay performs no retrieval, wishlist/catalog mandatory candidates remain, and game-ID dedupe occurs before scoring.
- [ ] **Step 3 — RED affinity/grounding tests:** Add `TestWishlistAndReactionOnlyGamesNeverBecomeAffinityOrExplanationAnchors` and `TestCompletedDislikedGameRemainsActivityAnchor`. Assert tag/playtime/recent/achievement/completion and grounding anchors use only eligible played activity; direct reaction evidence is separately grounded.
- [ ] **Step 4 — RED persistence/compatibility tests:** Add `TestNewRankingPersistsExactProfileOverlayProvenanceAndFactorNames` and `TestLegacyFactorsDecodeWithoutWritingLegacyNames`. New JSON uses only the six split semantic/direct fields; compatibility decoding maps old `positiveSimilarity`, `negativeSimilarity`, `positiveFeedback`, `negativeFeedback` for legacy reads until republish.
- [ ] **Step 5 — RED race test:** Add Review Focus test `TestRankingSnapshotBindsCoherentProfileOverlayPairDuringRace`. Interleave overlay/profile commits around a repeatable-read ranking transaction and assert persisted rows/run/build key all use one exact pair; same request key with different source IDs cannot publish under one build key.
- [ ] **Step 6 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/recommendations ./internal/httpapi -run 'Test(OverlayWeights|RetrievalUnion|WishlistAndReaction|CompletedDisliked|NewRanking|LegacyFactors|RankingSnapshot)'`.
- [ ] **Step 7 — Implement retrieval and source resolution:** Make recommendation service depend on `*preferences.Service` and call `EnsureCurrent` before opening ranking's repeatable-read transaction. In that transaction resolve the requested profile by `ProfileBuildKey` when supplied, otherwise newest vector-complete profile, then bind the newest completed overlay. Query both neighbor legs, union mandatory candidates, and load effective direct actions/corrections from the same snapshot.
- [ ] **Step 8 — Implement factors, score, evidence, and persistence:** Apply exact weights, retain zero wishlist weight, keep exact direct eligibility, remove feedback/wishlist from activity anchors, and persist both IDs on run and rows. Ranking build key includes both IDs, versions, algorithm versions, and effective input hashes. Bump stored factor/algorithm version and keep the read-only legacy decoder.
- [ ] **Step 9 — Verify GREEN and review gate:** Repeat Step 6; run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/recommendations ./internal/httpapi`, `codegraph sync .`, `codegraph affected internal/recommendations/types.go internal/recommendations/retrieval.go internal/recommendations/scoring.go internal/recommendations/evidence.go internal/recommendations/service.go -p .`, and `git diff --check`.
- [ ] **Step 10 — Commit:** `git add internal/recommendations internal/httpapi/ranking_handlers_test.go && git commit -m "feat: rank with activity and preference sources"`.

### Task 6: Split Steam, feedback, correction, and recommendation workflows

**Files:** Modify `internal/feedback/service.go:19-29,103-132,292-end`, `internal/feedback/service_test.go`, `internal/feedback/workflow_privacy_integration_test.go`, `internal/preferences/service.go:23-56`, `internal/preferences/disable_integration_test.go`, `internal/sync/service.go:285-381`, `internal/sync/service_test.go`, `internal/completion/service.go:185-225`, `internal/completion/service_test.go`, `internal/recommendations/service.go:573-end`, `internal/app/app.go:105-270`.

**Interfaces:**

```go
type recommendations.JobPayload struct { Version int `json:"version"`; UserID uuid.UUID `json:"user_id"`; ProfileBuildKey string `json:"profile_build_key,omitempty"`; RequestKey string `json:"request_key"` }
type preferences.JobPayload struct { Version int `json:"version"`; UserID uuid.UUID `json:"user_id"` }
```

- [ ] **Step 1 — RED trigger tests:** Add `TestFeedbackQueuesOverlayThenRankingWithoutProfile`, `TestCorrectionDisableQueuesOverlayThenRankingWithoutProfile`, and `TestReminderQueuesNeitherProfileNorOverlay`. Assert stage order and idempotency keys; no `profile.rebuild` or `ai.profile_summary` child may appear.
- [ ] **Step 2 — RED Steam tests:** Add `TestSuccessfulSteamSyncBuildsActivityThenRankingWithoutWaitingForSummary`. Assert profile draft completion gates ranking, the ranking payload carries the exact profile child build key, summary is optional/independent, and root sync may succeed while summary is still building.
- [ ] **Step 3 — RED failure/race tests:** Extend Review Focus `TestRankingSnapshotBindsCoherentProfileOverlayPairDuringRace` at workflow level: feedback arriving during Steam rebuild produces coherent published rankings, overlay failure leaves prior ranking and workflow retryable/dead, and repeated job attempts do not advance versions twice.
- [ ] **Step 4 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/feedback ./internal/preferences ./internal/sync ./internal/completion ./internal/recommendations -run 'Test(FeedbackQueues|CorrectionDisable|ReminderQueues|SuccessfulSteam|RankingSnapshot)'`.
- [ ] **Step 5 — Implement feedback/correction flows:** Keep classification/correction persistence first; then await `preferences.rebuild`, then await `recommendations.rebuild`, finalize ranking/diff only after publication. Correction disable uses the same two stages. Preserve immutable event/history and existing retry response IDs.
- [ ] **Step 6 — Implement Steam handoff:** After successful source/achievement/enrichment work, await activity profile build; queue recommendations using that profile build key and newest completed overlay resolved by ranking. Enqueue summary from the profile job as optional independent work and never wait on it in root Steam success.
- [ ] **Step 7 — Update wiring/job allowlists:** Register overlay before workflows that enqueue it, wire repositories/services in `app.New`, and add `preferences.rebuild` plus safe stages to observability/UI job-kind allowlists. Preserve fail-closed test overrides.
- [ ] **Step 8 — Verify GREEN and review gate:** Repeat Step 4; run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/feedback ./internal/preferences ./internal/sync ./internal/completion ./internal/recommendations ./internal/app`, `codegraph sync .`, `codegraph affected internal/feedback/service.go internal/preferences/service.go internal/sync/service.go internal/completion/service.go internal/recommendations/service.go internal/app/app.go -p .`, and `git diff --check`.
- [ ] **Step 9 — Commit:** `git add internal/feedback internal/preferences internal/sync internal/completion internal/recommendations internal/app internal/observability web/src/components/syncProgressModel.ts web/src/ui/labels.ts && git commit -m "feat: split activity and preference workflows"`.

### Task 7: Enforce 24-hour application-entry freshness on the backend

**Files:** Modify `internal/sync/service.go:24-159,798-903`, `internal/sync/service_test.go`, `internal/httpapi/sync_handlers.go:18-126`, `internal/httpapi/sync_handlers_test.go`, `internal/httpapi/router.go:16-70`, `internal/httpapi/router_test.go`; use Task 1 job queries.

**Interfaces:**

```go
type RefreshReason string // missing, stale, fresh, active
type RefreshDecision struct { Enqueued bool `json:"enqueued"`; Reason RefreshReason `json:"reason"`; JobID *uuid.UUID `json:"jobId,omitempty"`; LastSuccessfulAt *time.Time `json:"lastSuccessfulAt,omitempty"` }
func (*Service) RefreshIfStale(context.Context, uuid.UUID) (RefreshDecision, error)
```

- [ ] **Step 1 — RED service tests:** Add `TestRefreshIfStaleAtTwentyThreeFiftyNineIsFresh`, `TestRefreshIfStaleAtExactlyTwentyFourHoursEnqueues`, `TestRefreshIfStaleWithoutSuccessEnqueues`, `TestRefreshIfStaleReusesActiveWorkflow`, and `TestManualStartIgnoresFreshness` using injected `now`.
- [ ] **Step 2 — RED concurrency/security test:** Add Review Focus test `TestRefreshIfStaleBoundaryConcurrentTabsAndPreflightFailure`. Assert concurrent callers return one job ID/reason active-or-stale, a blocked/failed preflight enqueues nothing and exposes only sanitized status, demo and non-owner sessions receive the existing fail-closed response, and saved profile/recommendation reads remain unaffected.
- [ ] **Step 3 — RED handler contract tests:** Assert `POST /api/sync/refresh-if-stale` requires auth+owner+CSRF; returns 200 for fresh, 202 for created or active, and exact optional JSON fields/reason enum.
- [ ] **Step 4 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/sync ./internal/httpapi -run 'Test(RefreshIfStale|ManualStart|SyncRefresh)'`.
- [ ] **Step 5 — Implement decision flow:** Read active root first, then newest succeeded root `updated_at`; stale comparison is `<= now-24h`. For missing/stale, perform the same privacy preflight as manual sync, then call the existing lock-protected start path, which rechecks active state after preflight. Do not trust browser time.
- [ ] **Step 6 — Implement endpoint:** Add the route with owner and CSRF middleware; map existing preflight blocked/failed errors through sanitized sync error handling. Log only reason, status, duration, and workflow ID.
- [ ] **Step 7 — Verify GREEN and review gate:** Repeat Step 4; run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/sync ./internal/httpapi`, `codegraph sync .`, `codegraph affected internal/sync/service.go internal/httpapi/sync_handlers.go internal/httpapi/router.go -p .`, and `git diff --check`.
- [ ] **Step 8 — Commit:** `git add internal/sync internal/httpapi && git commit -m "feat: refresh Steam data on stale entry"`.

### Task 8: Align profile, adjustment, and recommendation API contracts

**Files:** Create `internal/httpapi/profile_handlers_test.go`; modify `internal/httpapi/profile_handlers.go:15-49`, `internal/httpapi/preference_handlers.go`, `internal/httpapi/preference_handlers_test.go`, `internal/httpapi/ranking_handlers_test.go`, `web/src/api/types.ts:1-170`, `web/src/api/client.ts:1-260`, `web/src/api/client.test.ts`.

**Interfaces:**

```ts
export type SummarySource = 'openai' | 'deterministic' | 'legacy'
export type PreferenceProfileData = { id: string; version: number; summary: string; summaryStatus: 'ready'; summarySource: SummarySource; confidence: number; evidenceCount: number; createdAt: string; publishedAt: string; updating: boolean }
export type EntryRefreshResponse = { enqueued: boolean; reason: 'missing' | 'stale' | 'fresh' | 'active'; jobId?: string; lastSuccessfulAt?: string }
export type DashboardData = { profile: PreferenceProfileData | null; adjustments: PreferenceAdjustment[]; overview: RecommendationOverview }
export function refreshSyncIfStale(csrfToken: string, signal?: AbortSignal): Promise<EntryRefreshResponse>
```

- [ ] **Step 1 — RED Go contract tests:** Assert `/api/profile` returns only one published Activity Profile, always nonblank/ready, includes `summarySource`, `publishedAt`, `updating`, returns 404 when none/insufficient, and never returns `adjustments`. Assert default `/api/preference-adjustments` returns only effective enabled corrections, excluding disabled and superseded-negative rows.
- [ ] **Step 2 — RED recommendation contract tests:** Assert new factor names round-trip consistently and legacy persisted rows normalize through the compatibility decoder without emitting legacy names in new writes or normal diagnostics.
- [ ] **Step 3 — RED TypeScript/client tests:** Assert profile normalization rejects/returns null for malformed ready payloads rather than synthesizing fallback text, adjustment loading is independent, new factor keys are preserved, legacy keys are accepted only by the compatibility normalizer, and entry refresh response validates reason/optional fields.
- [ ] **Step 4 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/httpapi -run 'Test(Profile|PreferenceAdjustment|Ranking)'` and `pnpm --dir web run test -- src/api/client.test.ts`.
- [ ] **Step 5 — Implement handlers and Go JSON:** Remove variadic preference service from `getProfile`; return `profile.Profile` only. Change default adjustment query to effective semantics from Task 1/4. Rename factor JSON consistently while keeping internal overlay IDs private.
- [ ] **Step 6 — Implement TypeScript and normalization:** Remove `adjustments` from `PreferenceProfileData`; add separate `adjustments` to dashboard data; validate new publication fields and safe factor maps; add `refreshSyncIfStale` with CSRF.
- [ ] **Step 7 — Verify GREEN and review gate:** Repeat Step 4; run `pnpm --dir web run typecheck`, `codegraph sync .`, `codegraph affected internal/httpapi/profile_handlers.go internal/httpapi/preference_handlers.go web/src/api/types.ts web/src/api/client.ts -p .`, and `git diff --check`.
- [ ] **Step 8 — Commit:** `git add internal/httpapi web/src/api/types.ts web/src/api/client.ts web/src/api/client.test.ts && git commit -m "feat: expose published activity profile contract"`.

### Task 9: Keep the old profile visible and separate explicit settings in the UI

**Files:** Create `web/src/components/PreferenceProfile.test.tsx`, `web/src/components/ExplicitRecommendationSettings.tsx`, `web/src/components/ExplicitRecommendationSettings.test.tsx`; modify `web/src/api/hooks.ts:1-191`, `web/src/api/hooks.test.ts`, `web/src/components/PreferenceProfile.tsx:1-24`, `web/src/components/PreferenceAdjustments.tsx:1-64`, `web/src/components/PreferenceAdjustments.test.tsx`, `web/src/pages/DashboardPage.tsx:1-210`, `web/src/pages/DashboardPage.test.tsx`, `web/src/styles.css:profile/preference-adjustment regions`.

**Interfaces:**

```ts
export function useEntryRefresh(user: CurrentUser, onSyncComplete: () => void): { status: 'idle' | 'checking' | 'syncing' | 'error'; sync: SyncStatus | null }
export default function PreferenceProfile({ profile }: { profile: PreferenceProfileData | null }): ReactElement
export function ExplicitRecommendationSettings(props: { adjustments: PreferenceAdjustment[]; csrfToken: string; readOnly?: boolean; onRankingUpdated?: (diff: RankingDiff) => void }): ReactElement | null
```

- [ ] **Step 1 — RED hook tests:** Extend `useDashboardData` to load profile, adjustments, and overview independently but publish one coherent dashboard snapshot; on reload failure retain all three previous resources. Add `useEntryRefresh` tests proving one owner entry call, demo skip, nonblocking failure, 202 polling/reload, and 200 fresh no polling.
- [ ] **Step 2 — RED profile-card tests:** Add `TestPreferenceProfileShowsPublishedSummaryWhileUpdating`, `TestPreferenceProfileUsesPlayedEvidenceCopyOnly`, and `TestPreferenceProfileDoesNotInventFallbackSummary`. Assert label `Профиль обновляется`, unchanged old version fields, evidence copy mentions played games/recent activity/achievements/completions only, and no wishlist/reaction language.
- [ ] **Step 3 — RED first-run/settings tests:** Assert 404 plus active sync renders progress instead of normal empty-profile copy; deterministic fallback renders as ready. Assert corrections appear under `Явные настройки рекомендаций`, outside the profile card, and disabling remains available while profile updates.
- [ ] **Step 4 — Verify RED:** Run `pnpm --dir web run test -- src/api/hooks.test.ts src/components/PreferenceProfile.test.tsx src/components/ExplicitRecommendationSettings.test.tsx src/pages/DashboardPage.test.tsx`.
- [ ] **Step 5 — Implement hooks:** Fetch three resources with one abort/request-generation guard; preserve previous data on any failed/superseded reload. Start entry refresh after the authenticated owner/CSRF data exists, never gate initial dashboard rendering, and reuse existing polling backoff.
- [ ] **Step 6 — Implement presentation:** Retitle the card to `Профиль игры`, render the published summary verbatim, show nonblocking update/failure copy, and move adjustments into a sibling section. Do not clear recommendation browsing or feedback controls.
- [ ] **Step 7 — Verify GREEN:** Repeat Step 4; run `pnpm --dir web run test`, `pnpm --dir web run typecheck`, `pnpm --dir web run lint`, and `pnpm --dir web run build`.
- [ ] **Step 8 — Review gate and commit:** Run `codegraph sync .`, `codegraph affected web/src/api/hooks.ts web/src/components/PreferenceProfile.tsx web/src/components/ExplicitRecommendationSettings.tsx web/src/pages/DashboardPage.tsx -p .`, and `git diff --check`. Then `git add web/src/api web/src/components web/src/pages/DashboardPage.tsx web/src/pages/DashboardPage.test.tsx web/src/styles.css && git commit -m "feat: keep activity profile visible during refresh"`.

### Task 10: Add boundary backfill, lifecycle fencing, and safe observability

**Files:** Create `internal/profileoverlay/backfill.go`, `internal/profileoverlay/backfill_test.go`, `internal/app/profile_overlay_backfill_test.go`; modify `internal/app/app.go:39-273`, `internal/httpapi/data_handlers.go:95-150,153-240`, `internal/httpapi/data_handlers_test.go`, `internal/observability/events.go:1-end`, `internal/observability/events_test.go`, `internal/store/migration_upgrade_test.go`.

**Interfaces:**

```go
const BackfillJobKind = "profile_overlay.boundary_backfill"
const BackfillVersion = 1
type BackfillPayload struct { Version int `json:"version"`; UserID uuid.UUID `json:"user_id"` }
func EnsureBoundaryBackfill(context.Context, *store.Store, *jobs.Queue) error
func RegisterBoundaryBackfill(*jobs.Worker, *jobs.Queue) error
```

- [ ] **Step 1 — RED rollout tests:** Add `TestBoundaryBackfillIsPerUserIdempotentAndUsesSavedSnapshots`. Assert no user → no-op; one user → one root job key containing boundary version; restarts reuse it; no Steam/OpenAI refetch occurs; workflow awaits activity profile, overlay, then ranking.
- [ ] **Step 2 — RED legacy/empty tests:** Assert a nonblank legacy profile stays published/source `legacy` with `updating=true` during backfill; successful new publication replaces it atomically. No played evidence produces authoritative insufficient plus empty overlay and no fabricated public profile.
- [ ] **Step 3 — RED deletion tests:** Extend data deletion coverage to overlays/embeddings/provenance/jobs. Hold an in-flight overlay insert/profile publication/ranking transaction across deletion and assert tombstone/FK/fencing prevents resurrection; shared game/features/embeddings remain.
- [ ] **Step 4 — RED observability/privacy tests:** Assert events expose only versions, counts, durations, boolean newer-draft, algorithms, and enumerated reasons for profile build/publication, overlay build, freshness, and ranking publication. Feed SteamID, UUIDs, feedback text, prompt-like text, and model output; assert none appears in general logs/diagnostics.
- [ ] **Step 5 — Verify RED:** Run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/profileoverlay ./internal/app ./internal/httpapi ./internal/observability -run 'Test(BoundaryBackfill|DataDeletion|Observability|ProfileOverlay)'`.
- [ ] **Step 6 — Implement coordinator:** Register the backfill root after profile/overlay/ranking handlers are registered; enqueue once at startup for existing local users. Sequence children with versioned idempotency keys and exact profile build-key handoff; summary stays independent.
- [ ] **Step 7 — Implement lifecycle and diagnostics:** Ensure new rows/jobs cascade or fail closed, diagnostics distinguish profile build failure from optional summary publication failure, and dead optional summary jobs never imply saved activity/ranking loss.
- [ ] **Step 8 — Verify GREEN and review gate:** Repeat Step 5; run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/profileoverlay ./internal/app ./internal/httpapi ./internal/observability`, `codegraph sync .`, `codegraph affected internal/profileoverlay/backfill.go internal/app/app.go internal/httpapi/data_handlers.go internal/observability/events.go -p .`, and `git diff --check`.
- [ ] **Step 9 — Commit:** `git add internal/profileoverlay internal/app internal/httpapi/data_handlers.go internal/httpapi/data_handlers_test.go internal/observability internal/store/migration_upgrade_test.go && git commit -m "feat: backfill activity profile overlay boundary"`.

### Task 11: Prove end-to-end behavior and run the release gate

**Files:** Create `web/tests/e2e/activity-profile-overlay.spec.ts`; modify `cmd/e2e-fakes/main.go` for deterministic delayed/disabled profile fixtures. Validate `docs/local-demo.md` against the new behavior and modify it only when a referenced command or profile/entry-sync statement is stale. Do not edit the accepted design or add inline evidence-key cleanup.

**Interfaces:** Consumes the public/API/job contracts from Tasks 1–10; produces no new product interface.

- [ ] **Step 1 — RED product-loop E2E:** Add/extend a focused test proving Steam sync → activity-only draft/ranking → asynchronous published profile. While summary is delayed, assert the prior summary/version stays visible with `Профиль обновляется`; wishlist/unplayed/feedback fixtures cannot alter profile version/vector/evidence/summary.
- [ ] **Step 2 — RED feedback E2E:** Submit dislike then like for one game. Assert Activity Profile version/summary remain byte-for-byte unchanged, overlay and ranking versions advance, only the latest reaction is effective, correction from the old dislike disappears from default settings, and a positive overlay neighbor can enter recommendations.
- [ ] **Step 3 — RED disabled-provider/first-run E2E:** With OpenAI disabled, assert first sync publishes deterministic played-activity fallback, feedback still builds overlay and ranking, missing embeddings preserve exact effects, and no profile card is blank.
- [ ] **Step 4 — RED entry freshness E2E:** Enter as the owner with a stale saved root workflow and assert automatic refresh starts without blocking the already-saved profile/recommendations; open a second page and assert it observes the same active job. Assert demo entry makes no mutation. Exact 23h59m/24h server-clock boundaries remain pinned by Task 7 integration tests, not a new production clock override.
- [ ] **Step 5 — Verify focused E2E:** Run `pnpm --dir web exec playwright test tests/e2e/activity-profile-overlay.spec.ts`, then `E2E_OPENAI_DISABLED=1 pnpm --dir web exec playwright test tests/e2e/activity-profile-overlay.spec.ts`. If the pinned browser is unavailable, use only the documented explicit `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` fallback and report it.
- [ ] **Step 6 — Run affected and package gates:** Run `codegraph sync .`; run `codegraph affected` over all changed source files and execute any additional suggested focused tests. Then run `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/profile ./internal/ai ./internal/preferences ./internal/recommendations ./internal/feedback ./internal/sync ./internal/httpapi ./internal/app ./internal/profileoverlay`, `pnpm --dir web run test`, `pnpm --dir web run typecheck`, `pnpm --dir web run lint`, `pnpm --dir web run build`, `make sqlc`, and `git diff --check`.
- [ ] **Step 7 — Fresh final release gate:** Run one fresh `make verify` after all focused suites and reviews are stable. Expected: zero failures; only documented expected skips. Do not repeat unless a subsequent change can affect the gate or the run was interrupted/invalidated.
- [ ] **Step 8 — Final review:** Re-read every acceptance criterion in the spec and map it to passing tests. Confirm no `.env`, CodeGraph data, raw identifiers/prompts, build output, or unrelated dirty files are staged. Confirm inline `[signal-*]` cleanup remains outside this diff.
- [ ] **Step 9 — Commit:** Stage only `web/tests/e2e/activity-profile-overlay.spec.ts`, required `cmd/e2e-fakes/main.go` fixture changes, and any validated `docs/local-demo.md` update, then commit: `git commit -m "test: cover activity profile preference overlay"`. Do not push or deploy without explicit authorization.
