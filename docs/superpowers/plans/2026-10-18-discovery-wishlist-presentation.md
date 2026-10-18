# Discovery and Wishlist Presentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Present deterministic non-wishlist discoveries first, preserve a separate wishlist watchlist, and keep the full published ranking available through explicitly filtered calendar views.

**Architecture:** Keep one immutable recommendation run and its persisted factor/weight snapshots. Build the overview from one read of the latest published run, partition by its wishlist factor, and attach sanitized source freshness and catalog coverage. The browser consumes server-selected collections and loads the profile in parallel; catalog ingestion remains a separate project.

**Tech Stack:** Go 1.27.2 (`GOWORK=off`), PostgreSQL/pgvector, chi, React 19/TypeScript, Vitest/Testing Library, Playwright, PostgreSQL-backed jobs.

**Spec:** `docs/superpowers/specs/2026-10-18-discovery-wishlist-presentation-design.md`; integration dependency: `docs/superpowers/specs/2026-10-18-steam-catalog-discovery-design.md`.

## Global Constraints

- `wishlist` weight becomes `0` for ordering; all other deterministic and semantic factors retain their current weights in this feature.
- Wishlist remains a boolean factor and explanation fact. Preserve existing numeric `factors.wishlist` snapshots (`0`/`1`); do not introduce a second membership field.
- Existing published rankings remain readable; a new ranking is built and atomically published.
- Owned, released, hidden, and repeatedly disliked games remain excluded.
- `discoveries.items` contains the next six discoveries; `wishlist.items` contains up to six wishlist games.
- Calendar contains the complete published ranking: the configured maximum of 100 non-wishlist candidates plus every eligible wishlist candidate.
- Order: score descending, case-insensitive name, original name, game UUID.
- No migrations or duplicated recommendation records are needed for presentation; do not rewrite old factor snapshots or scores.
- Preserve date precision, source snapshots on failures, single-user authorization, CSRF for mutations, deletion fences, and fail-closed test overrides.
- Do not add hard-exclude browser actions, pagination, catalog ingestion, new upstreams, changed horizons, or changes to other weights.
- This planning delivery makes no commit. During implementation, commits require the user's authorization; checkpoints below are review/test gates, not permission to commit.

## Review Focus

- Equal scores with case variants and identical names must produce the same hero, section order, and ranking diff on every read; pin in Tasks 1, 3, and 8.
- A private/failed wishlist refresh must preserve membership and the last successful timestamp, while a successful empty fetch must remain distinguishable; pin in Tasks 3, 4, and 9.
- Zero wishlist ordering weight must retain membership evidence; membership/novelty alone must not invite a generated affinity claim; pin in Task 2.
- Zero discoveries with a populated wishlist or incomplete catalog must show honest coverage and never promote wishlist cards into the hero; pin in Tasks 5, 6, and 9.
- A failed refresh/rebuild or overlapping browser request must retain the previously published/rendered collections without cross-classification; pin in Tasks 1, 5, 8, and 9.

---

## Boundaries, dependency contract, and file map

Presentation can ship against the existing seed catalog. It does **not** wait for automatic discovery, create discovery tables/jobs, alter Steam adapters, remove `origin='wishlist'` rows, or duplicate the catalog plan's union-candidate migration. When that plan lands, its union retrieval must preserve `Candidates(ctx, userID, profile, limit)` semantics: up to `limit=100` discoveries plus **all** eligible wishlist games, deduplicated by game ID. Deploy that union before removing wishlist-only shared catalog rows.

The agreed catalog integration contract is owned by `internal/catalog/discovery_status.go` (provided by the catalog project, or add only this aggregate contract plus the disabled implementation here if presentation ships first):

```go
type PresentationStatus struct {
    Status string `json:"status"`
    ActiveDiscoveredGames int64 `json:"activeDiscoveredGames"`
    LastSuccessfulScanAt *time.Time `json:"lastSuccessfulScanAt"`
    BackfillCompletedAt *time.Time `json:"backfillCompletedAt"`
}
type DiscoveryStatusReader interface {
    DiscoveryStatus(context.Context) (PresentationStatus, error)
}
```

Catalog diagnostics use `disabled|updating|ready|stale|degraded`; the presentation spec's public `overview.catalogStatus.status` uses `disabled|backfilling|ready|stale|unavailable`. Task 3 maps `updating → backfilling`, `degraded → unavailable`, and the other values identically, without mutating the reader's value or introducing a duplicate status struct. A nil reader means discovery disabled: `disabled`, `0`, `null`, `null`. It must never fabricate an active count from wishlist or seed counts. A configured reader failure yields `unavailable` and a safe zero/null aggregate, while retaining recommendation items. A reported `stale` value retains its actual last-success aggregate. The UI does not expose diagnostic cursor/watermark/AppID internals. `ready` means the provider has finished a bounded backfill and has a fresh incremental scan, never complete Steam coverage.

| Files | Responsibility |
| --- | --- |
| `internal/recommendations/types.go`, `scoring.go`, `service.go` | v2 score policy, build-key namespace, safe evidence/explanations |
| New `internal/recommendations/overview.go`, `overview_test.go`, `overview_repository.go`, `overview_repository_integration_test.go` | collection DTOs/partitioning, source-state reader, overview assembly |
| `internal/recommendations/retrieval.go`, `db/queries/recommendations.sql`, `internal/feedback/repository.go` | consistent persisted-read/diff tie order, unchanged candidate boundary |
| `internal/sync/service.go`, `service_test.go` | only successful wishlist state/data atomicity; no discovery ingestion |
| `internal/httpapi/recommendation_handlers.go`, `router.go`, `ranking_handlers_test.go`, `internal/app/app.go` | authenticated overview route, dependency wiring, old calendar compatibility |
| `web/src/api/types.ts`, `client.ts`, `hooks.ts` and their tests | exact shared DTOs, normalization, parallel/cancellable loading |
| `web/src/pages/DashboardPage.tsx`, new `web/src/pages/DashboardPage.test.tsx`, `web/src/components/DashboardStates.test.tsx`, new `web/src/components/CatalogCoverage.tsx` and test, `web/src/styles.css` | discovery/watchlist sections, honest coverage/freshness copy |
| `web/src/pages/CalendarPage.tsx`, `CalendarPage.test.tsx` | three-state source filter composed with existing filters |
| `internal/feedback/repository_integration_test.go`, `web/src/components/FeedbackForm.test.tsx` | both-collection feedback regression, unchanged mutation contract |
| `web/tests/e2e/smoke.spec.ts`, new `web/tests/e2e/presentation.spec.ts`, `cmd/e2e-fakes/main.go`, `Makefile`, `docs/local-demo.md` | fixture loop plus presentation state matrix and release verification |

`internal/store/sqlc/` changes only through `make sqlc` if the query tie-order change regenerates bindings. No manual edits. New tests reuse existing isolated-store helpers in their own packages; never use `DATABASE_URL` as a test fallback.

### Task 1: Version deterministic ranking without a wishlist bonus

**Files:** Modify `internal/recommendations/types.go`, `scoring.go`, `service.go`, `scoring_test.go`, `repository_integration_test.go`; create `internal/recommendations/build_key_test.go`.

**Interfaces:** Keep `DefaultWeights() Weights`, `Score(Factors, Weights) Result`, `Eligible(Candidate) bool`, and `Service.rebuild(context.Context, uuid.UUID, string) ([]Recommendation, int64, error)`. Set `AlgorithmVersion = "ranking-v2"`. Add private `rankingBuildKey(workflowKey string) string` returning `workflowKey + ":algorithm:" + AlgorithmVersion`; call exactly once inside `rebuild` before `SaveRanking`. Queue job keys/payload schemas remain unchanged, including feedback's `:ranking:v1` lookup; build identity and queue identity are separate contracts.

- [ ] **Step 1: Write failing tests.** Replace `TestScoreUsesVersionOneWeights` with `TestScoreUsesVersionTwoWeights`; retain a historical explicit-v1-weights test. Assert:

```go
// TestScoreUsesVersionTwoWeights
want := Weights{SemanticSimilarity: 25, Wishlist: 0, TagAffinity: 15, PlaytimeAffinity: 10, AchievementAffinity: 5, RecentActivityAffinity: 5, PositiveFeedback: 20, Novelty: 5, NegativeFeedback: -30, DateUncertainty: -5, NegativeSimilarityMultiplier: .5}
if DefaultWeights() != want || AlgorithmVersion != "ranking-v2" { t.Fatal("policy mismatch") }
if Score(Factors{Wishlist: 1}, want).Score != 0 { t.Fatal("wishlist adds ordering points") }
if Score(Factors{TagAffinity: .8}, want).Score != Score(Factors{TagAffinity: .8, Wishlist: 1}, want).Score { t.Fatal("membership changes order") }
if Score(Factors{PositiveSimilarity: 1}, want).Score <= Score(Factors{Wishlist: 1}, want).Score { t.Fatal("affinity discovery cannot win") }
```

Also retain clamping, negative similarity, one/two dislike eligibility, and all unchanged weight assertions. Add `TestRankingBuildKeyIncludesAlgorithmVersion` asserting `rankingBuildKey("same-workflow") == "same-workflow:algorithm:ranking-v2"`. Add integration `TestRankingV2PublishesWithoutMutatingV1`: insert a published v1 run with explicit `Wishlist:30`, rebuild with the same raw workflow key, assert a distinct run/build key and v2 weights while old `Get` still exposes v1 score/weights. Add `TestFailedV2BuildKeepsPreviousPublishedRun` using a repository wrapper that fails `SaveExplanation`; latest stays v1 and no partial v2 recommendations are returned.

- [ ] **Step 2: Run red.** `GOWORK=off go test -race ./internal/recommendations -run 'TestScoreUsesVersionTwoWeights|TestRankingBuildKeyIncludesAlgorithmVersion' -count=1`. Expected: FAIL for v1 policy/missing key helper. Run new integration names with the isolated `TEST_DATABASE_URL` after `make wait-test-db`; expect behavior assertion failures, not skipped DB tests.
- [ ] **Step 3: Implement the policy and one build-key transformation.** Keep persisted old snapshots readable and publication after every explanation is safely saved. Do not change candidate limits, exclusions, or score factors other than wishlist weight. Update the old high-factor assertion: the current all-positive v2 factors total `85`, so the previous `100` expectation no longer applies; wishlist plus date uncertainty now clamps to `0`, not `25`. Test upper clamping separately with explicit oversized test weights.
- [ ] **Step 4: Run green.** Repeat focused commands; then `GOWORK=off go test -race ./internal/recommendations -count=1` with `TEST_DATABASE_URL` set. Expected: zero failures and no new integration skips. Review diff before advancing.

### Task 2: Ground wishlist explanations independently of score contribution

**Files:** Modify `internal/recommendations/service.go`, `service_test.go`, `presentation_test.go`, `repository_integration_test.go`.

**Interfaces:** Keep `publicEvidence(Recommendation) []Evidence`, `Explain(context.Context, ExplanationClient, string, []Evidence) ai.Explanation`, and `safePresentation(Recommendation) Recommendation`. Add private `hasSubstantiveEvidence([]Evidence) bool` and `fallbackExplanation([]Evidence) ai.Explanation`; substantive IDs are exactly `semantic-affinity`, `tag-affinity`, `playtime-affinity`, `achievement-affinity`, `recent-activity`, `positive-feedback`. Negative feedback remains a ranking penalty; do not invent positive affinity evidence from it.

- [ ] **Step 1: Write failing tests.** Add `TestWishlistEvidenceSurvivesZeroOrderingWeight`: `Factors{Wishlist:1, TagAffinity:.01, Novelty:1}` must include `wishlist` and `tag-affinity` with at most three public facts, even though novelty has a larger contribution. A discovery must never receive `wishlist`. Keep positive-factor ordering descending by contribution then existing reason order; for a wishlist reserve one slot for membership, one for the highest positive substantive signal when present, and use the final slot for the highest remaining positive reason. Membership is first for watchlist explanations; retained legacy evidence IDs/text remain allowlisted.

Add `TestMembershipOnlyExplanationDoesNotCallModel` with wishlist alone and wishlist+novelty inputs: a counting fake receives zero calls; `Generated=false`; keys include only supplied evidence; exact copy is `Игра уже в вашем списке желаемого — следим за её релизом.` Add `TestGeneratedWishlistExplanationRequiresSubstantiveCitation`: with membership and tag evidence, a model response citing only `wishlist` falls back, while a safe Russian response citing `tag-affinity` (and optionally `wishlist`) is accepted. Add `TestSafePresentationRejectsMembershipOnlyGeneratedAffinity` for old stored generated text and `TestDiscoveryExplanationNeverClaimsWishlistMembership` with unknown wishlist citation. Preserve existing unsafe-language/JSON/raw-factor/citation checks.

- [ ] **Step 2: Run red.** `GOWORK=off go test -race ./internal/recommendations -run 'TestWishlistEvidence|TestMembershipOnly|TestGeneratedWishlist|TestSafePresentation|TestDiscoveryExplanation' -count=1`. Expected: FAIL because zero-weight evidence disappears/model is called or legacy generated membership-only text survives.
- [ ] **Step 3: Implement evidence selection and shared fallback policy.** Use the same selection, substantive-citation validation, and deterministic watchlist fallback in rebuild, `Latest`, and `Get`. With no substantive affinity, skip the model for wishlist recommendations even if novelty exists. For discovery evidence the existing deterministic generic fallback remains available. Do not change score calculation or ask AI to supply missing evidence.
- [ ] **Step 4: Run green.** Repeat red command, then the recommendations package with `-race` and DB enabled. Update earlier presentation tests whose expected wishlist-only generated text contradicts this feature; keep explicit old-weight readability coverage from Task 1.

### Task 3: Build deterministic overview collections and trustworthy source status

**Files:** Create `internal/recommendations/overview.go`, `overview_test.go`, `overview_repository.go`, `overview_repository_integration_test.go`; modify `internal/recommendations/service.go`, `retrieval.go`, `db/queries/recommendations.sql`, `internal/sync/service.go`, `internal/sync/service_test.go`; consume `internal/catalog/discovery_status.go` contract above.

**Interfaces:** Add these exported DTOs in `overview.go` (JSON fields are required, nullable pointers have no `omitempty`):

```go
type DiscoveryCollection struct {
    Featured *Recommendation `json:"featured"`
    Items []Recommendation `json:"items"`
    Total int `json:"total"`
}
type WishlistSourceState struct {
    Availability string
    LastSuccessfulFetchAt *time.Time
}
type WishlistCollection struct {
    Items []Recommendation `json:"items"`
    Total int `json:"total"`
    Status string `json:"status"`
    Availability string `json:"availability"`
    LastSuccessfulFetchAt *time.Time `json:"lastSuccessfulFetchAt"`
    DegradedReason *string `json:"degradedReason"`
}
type Overview struct {
    Discoveries DiscoveryCollection `json:"discoveries"`
    Wishlist WishlistCollection `json:"wishlist"`
    Calendar Calendar `json:"calendar"`
    CatalogStatus catalog.PresentationStatus `json:"catalogStatus"`
}
type WishlistStateReader interface {
    WishlistSourceState(context.Context, uuid.UUID) (WishlistSourceState, error)
}
func BuildOverview(items []Recommendation, wishlist WishlistSourceState, status catalog.PresentationStatus) Overview
func (r *SQLRepository) WishlistSourceState(ctx context.Context, userID uuid.UUID) (WishlistSourceState, error)
func (s *Service) SetOverviewSources(wishlist WishlistStateReader, discovery catalog.DiscoveryStatusReader)
func (s *Service) Overview(ctx context.Context, userID uuid.UUID) (Overview, error)
```

Keep the existing ranking `Repository` interface unchanged; separate read dependencies avoid forcing ranking-only fakes to implement unrelated methods. Application wiring passes the existing recommendation SQL repository as `WishlistStateReader`. `Overview` calls `s.Latest` once, loads source metadata separately, and partitions that same immutable published item set. No per-section latest queries or reads of current `wishlist_entries` for classification.

Wishlist status mapping (read `user_source_states` with exact `source='wishlist' AND resource_key='' AND user_id=$1`):

| Availability | Successful timestamp | status | degradedReason |
| --- | --- | --- | --- |
| `available` or `not_modified` | non-null | `ready` | `null` |
| `unavailable` | non-null | `stale` | `private_or_unavailable` |
| `request_failed` | non-null | `stale` | `request_failed` |
| `unavailable` | null | `unavailable` | `private_or_unavailable` |
| `request_failed` | null | `unavailable` | `request_failed` |
| absent/unknown, or available/not_modified without successful timestamp | any/null | `unknown` | `null` |

Normalize unrecognized availability to `unknown`. Do not read/return persisted error text. A wishlist-state database error fails the overview (sanitized HTTP 500); a catalog-reader error degrades only coverage as described above. Classification stays frozen until a new ranking publishes, so current source freshness does not silently move a game across sections. A successful empty snapshot yields zero watchlist after its associated ranking publishes; while rebuilding, the prior published run remains visible.

- [ ] **Step 1: Write failing pure read-model tests.** `TestOverviewPartitionsAndCapsPublishedRanking` uses eight discoveries and eight wishlist games (wishlist scores deliberately higher), asserts featured discovery ID, six next discoveries, six wishlist cards, totals `8/8`, calendar count `16`, pairwise disjoint collection game IDs, all item ranking versions identical. `TestOverviewHandlesEmptyAndSingleCollection` covers none/discovery-only/wishlist-only and asserts `featured=nil`, `items=[]`, all calendar buckets `[]`, never null slices or wishlist fallback. `TestOverviewTieOrder` uses fixed UUIDs, case variants, equal names/scores in shuffled input and asserts score/lower-name/raw-name/game-ID order in each collection and calendar. `TestOverviewPreservesAllReleasePrecisions` covers day/month/quarter/year/coming_soon/unknown and day-without-date→TBA; no invented exact dates. `TestOverviewWishlistSourceStatus` covers every table row, empty ready snapshot, private stale nonempty snapshot, and malformed availability. `TestOverviewCatalogStatusMapping` covers all five provider statuses, nil provider, reader failure, retained stale aggregate, and omission of cursor/user fields.
- [ ] **Step 2: Run red.** `GOWORK=off go test -race ./internal/recommendations -run 'TestOverview' -count=1`. Expected: FAIL with missing overview types/functions.
- [ ] **Step 3: Implement pure partitioning and service composition.** Sort once with existing `Sort`, partition by `(Factors.Wishlist > 0)`, calculate complete totals before slicing, then build calendar from the entire sorted input. Initialize every collection/calendar slice to non-nil empty slices. Map catalog enum at the public boundary. Keep status separate from available recommendation content and call `safePresentation` through `s.Latest`.
- [ ] **Step 4: Write failing persistence tests.** `TestWishlistSourceStateScopesAndPreservesTimestamp` inserts two owners, available empty state, then unavailable/request_failed state; assert exact owner scoping and unchanged success timestamp, no dependence on wishlist row count. `TestOverviewReadsOnePublishedRunDuringRebuild` seeds v1, incomplete v2 and then publication; assert overview switches as a whole. `TestWishlistFailedPersistenceCannotAdvanceSuccessfulFetch` induces a wishlist transaction failure after a good snapshot and verifies the old membership/success timestamp remain, with failure availability. Reuse `internal/sync/service_test.go` isolated DB harness and a failing transaction condition; no real upstream/network.
- [ ] **Step 5: Run integration red.** `make wait-test-db`; then `TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' GOWORK=off go test -race ./internal/recommendations ./internal/sync -run 'TestWishlistSourceState|TestOverviewReadsOnePublishedRun|TestWishlistFailedPersistence' -count=1`. Expected: failing assertions for missing reader or premature success marker; no skipped tests.
- [ ] **Step 6: Implement source reader and minimal wishlist atomicity.** Put successful wishlist availability/timestamp update in the same `saveWishlist` transaction as wishlist replacement, using the same captured `now`; record unavailable/failure states separately while retaining old success timestamp. If persistence fails, mark a bounded failure without advancing last success and return the error. Preserve existing not-modified handling without replacing data. This corrects the current `recordState`-before-`saveWishlist` ordering only for wishlist; preserve all existing deletion/locking behavior. Add raw `g.name` between `lower(g.name)` and `g.id` in handwritten `Latest` SQL and `db/queries/recommendations.sql`; regenerate with `make sqlc`. No schema change.
- [ ] **Step 7: Run green.** Repeat pure/integration tests, then `GOWORK=off go test -race ./internal/recommendations ./internal/sync` with isolated DB. Review generated query diff only. Run `codegraph sync .` before using further graph output.

### Task 4: Expose authenticated overview while retaining calendar compatibility

**Files:** Modify `internal/httpapi/recommendation_handlers.go`, `router.go`, `ranking_handlers_test.go`, `internal/app/app.go`; create `internal/httpapi/overview_contract_test.go`.

**Interfaces:** Add `Overview(context.Context, uuid.UUID) (recommendations.Overview, error)` to `RecommendationReadService` and `getRecommendationOverview(RecommendationReadService) http.HandlerFunc`. Register authenticated `GET /api/recommendations/overview` explicitly before `/api/recommendations/{id}`. Keep `Latest`, `Get`, `/api/calendar`, and all feedback routes. Wire `recommendationService.SetOverviewSources(recommendationRepository, catalogReader)`; `catalogReader=nil` is valid seed-only operation until catalog status is enabled. No `/api/dashboard` route or profile embedding in overview.

- [ ] **Step 1: Write failing handler/contract tests.** Extend the existing fake reader with `Overview`, captured owner, call counts, and configurable errors. `TestOverviewRequiresAuthenticatedAllowlistedOwner` expects unauthenticated `401`, correct session owner, and no user ID query override; preserve auth middleware allowlist tests. `TestOverviewDoesNotResolveAsRecommendationID` expects overview `200`, not UUID `400`. `TestOverviewJSONContract` table tests mixed/discovery-only/wishlist-only/empty/stale source; decode JSON and assert exact four top-level keys, required nullable timestamps/reason/featured, six calendar arrays, `total` before truncation, precision unchanged, no `cursorAppID`, `steamId`, `userId`, internal error or raw body. `TestOverviewFailureIsSanitized` expects `500` and fixed `recommendation overview unavailable`. `TestCalendarCompatibilityMatchesOverviewPublishedRanking` compares complete calendar IDs/buckets to overview under a fixed published run.
- [ ] **Step 2: Run red.** `GOWORK=off go test -race ./internal/httpapi -run 'TestOverview|TestCalendarCompatibility' -count=1`. Expected: FAIL because route/interface are missing.
- [ ] **Step 3: Implement route and dependency wiring.** Derive owner only from authenticated session; serialize the typed DTO using existing `writeJSON`. Keep legacy calendar generated from one `Latest` read; do not truncate it to dashboard limits. Add `Overview` to every fake implementing the expanded HTTP interface identified by compilation/CodeGraph.
- [ ] **Step 4: Run green.** Repeat focused command, then `GOWORK=off go test -race ./internal/httpapi ./internal/app -count=1` with DB tests enabled. Expected: zero failures, old endpoints continue passing.

### Task 5: Align TypeScript DTOs, normalization, and parallel loading

**Files:** Modify `web/src/api/types.ts`, `client.ts`, `client.test.ts`, `hooks.ts`, `hooks.test.ts`; create `web/src/test/overviewFixture.ts` for shared typed fixture builders.

**Interfaces:** Define `DiscoveryCollection`, `WishlistCollection`, `CatalogStatus`, and `RecommendationOverview` with the exact JSON keys from Task 3. TS `WishlistCollection.status` is `'ready'|'stale'|'unavailable'|'unknown'`; `availability` is `'available'|'unavailable'|'request_failed'|'not_modified'|'unknown'`; `lastSuccessfulFetchAt: string|null`; `degradedReason: 'private_or_unavailable'|'request_failed'|null`. `CatalogStatus.status` is `'ready'|'backfilling'|'stale'|'unavailable'|'disabled'`, counts are `number`, timestamps `string|null`. `DiscoveryCollection.featured: Recommendation|null`, all items arrays required, totals `number`.

Set `DashboardData = { profile: PreferenceProfileData|null; overview: RecommendationOverview }`; do not keep a second independent dashboard calendar. Consumers use `data.overview.calendar`. Export `getRecommendationOverview(signal?: AbortSignal): Promise<RecommendationOverview>` and `normalizeOverview(value: Partial<RecommendationOverview>): RecommendationOverview`. Preserve `getCalendar`/`normalizeCalendar` for compatibility consumers; extract private `normalizeRecommendation(item: Recommendation): Recommendation` so featured and every array normalize factors/weights/evidence identically. `normalizeOverview` never synthesizes hero/items/totals from calendar or reclassifies games.

- [ ] **Step 1: Write failing client/hook tests.** `normalizeOverview` must normalize Go-null/absent slices to `[]`, missing featured to `null`, null metadata to honest unknown/disabled defaults, preserve supplied totals and stale/null timestamp/reason fields, and normalize nested recommendation defaults. `getRecommendationOverview` must request `/api/recommendations/overview` with same-origin credentials and passed abort signal. `useDashboardData` test uses two deferred responses: both `/api/profile` and overview requests start before either resolves, `/api/calendar` is not requested, and only the newest request can publish data. Add reload-failure test preserving previous complete overview and `status='stale'`; discovery-only, wishlist-only, and empty responses remain honest. Keep `deriveFreshness` based on profile plus **complete** `overview.calendar`, while catalog/wishlist source statuses remain independently visible (fresh recommendation timestamps must not hide stale sources).
- [ ] **Step 2: Run red.** `pnpm --dir web run test -- src/api/client.test.ts src/api/hooks.test.ts`. Expected: missing types/functions or old calendar-request assertions fail.
- [ ] **Step 3: Implement DTOs/loading/normalization.** `Promise.all([getProfile(signal), getRecommendationOverview(signal)])`; retain abort/request-number guards. Determine global empty state from absent profile and complete calendar count, not whether discoveries are empty. Supply an empty overview in `emptyData`. Update all existing typed initial-state fixtures to the single `overview` property; do not cast through `any` to conceal contract changes.
- [ ] **Step 4: Run green.** Repeat focused command; `pnpm --dir web run typecheck`. Expected: zero failures; nullable fields and catalog enum match the public contract, no residual dashboard `data.calendar` or `/api/calendar` fetch.

### Task 6: Render separate discovery/watchlist sections and coverage

**Files:** Modify `web/src/pages/DashboardPage.tsx`, `web/src/components/DashboardStates.test.tsx`, `web/src/components/RecommendationCard.test.tsx`, `web/src/styles.css`; create `web/src/pages/DashboardPage.test.tsx`, `web/src/components/CatalogCoverage.tsx`, `web/src/components/CatalogCoverage.test.tsx`.

**Interfaces:** Add `CatalogCoverage({status}: {status: CatalogStatus})`; dashboard uses server `overview.discoveries.featured`, `.items`, `.total`, `overview.wishlist`, and full `.calendar`. Keep `RecommendationCard` and `FeedbackForm` props unchanged. Remove dashboard `flatten()` and its locale-dependent global sort. Use accessible section headings/regions so tests scope cards and feedback to each collection.

- [ ] **Step 1: Write failing component tests.** Mixed fixture has eight discoveries/eight wishlist games and deliberately higher wishlist scores. Assert sections `Новая находка для вас`, `Ещё новые игры`, `Уже в вашем желаемом`; hero+six primary cards all discovery; six watchlist cards; complete totals `8/8` shown as total counts, without duplicate hero. Wishlist badges remain `♥ В желаемом`; discoveries never show `не в желаемом`. Discovery-only/wishlist-only/fully empty tests assert exact empty copy `Пока не нашли новых игр вне вашего списка желаемого`, no wishlist hero fallback, calendar/watchlist retained, and existing synchronization/preflight flow as the only data action.

Coverage tests cover all five public statuses, active discovered count and nullable localized last update, no cursor; backfilling says `Каталог обновляется` / `Каталог может быть неполным`; ready says a bounded scan completed without claiming complete Steam. Disabled says the reviewed local catalog is in use, not that automatic discovery completed; stale/unavailable say last saved recommendations remain visible. Wishlist status tests distinguish `ready` with zero items from `stale` preserved items and unavailable/unknown first-fetch state, show last successful time when present, and never label a private response as an empty successful list.
- [ ] **Step 2: Run red.** `pnpm --dir web run test -- src/pages/DashboardPage.test.tsx src/components/DashboardStates.test.tsx src/components/CatalogCoverage.test.tsx src/components/RecommendationCard.test.tsx`. Expected: old headings/global sorting or absent status section fails.
- [ ] **Step 3: Implement sections and status copy.** Render featured and next six exactly as supplied; no frontend re-sorting/re-slicing a combined calendar. Style wishlist as a secondary section. Put `CatalogCoverage` in the existing right rail beside profile/status. Pass the same existing `rankingUpdated` callback and CSRF prop to cards in **both** collections. Keep stale load-state banner plus source-specific freshness. Pass full overview calendar to existing calendar/right-rail bucket views.
- [ ] **Step 4: Run green.** Repeat focused tests, then frontend `test`, `typecheck`, `lint`, and `build`. Inspect desktop and narrow viewport in Task 9; no clipped source controls/status text or hidden watchlist actions.

### Task 7: Compose three-state calendar source filtering with date/score filters

**Files:** Modify `web/src/pages/CalendarPage.tsx`, `web/src/pages/CalendarPage.test.tsx`; retain `web/src/components/ReleaseCalendar.tsx`, `ReleaseBuckets.tsx`, and `web/src/calendar/dateBuckets.ts` behavior.

**Interfaces:** Export `type CalendarSourceFilter = 'all'|'discoveries'|'wishlist'` from `CalendarPage.tsx`; replace private `filterCalendar(calendar, wishlistOnly, minimum, precision)` with `filterCalendar(calendar: CalendarResponse, source: CalendarSourceFilter, minimum: number, precision: string): CalendarResponse`. The source predicate uses `(item.factors.wishlist ?? 0)>0`, identical to Task 3. Keep filter local; no API query parameters or ranking mutation.

- [ ] **Step 1: Write failing UI tests.** Replace checkbox assertions with labeled source select `Источник игр`, options `Все`, `Новые для меня`, `В желаемом`. Use both sources in every release precision bucket. Assert all/discoveries/wishlist produce complementary sets in month and list views; minimum score and precision apply by AND; bucket count equals currently filtered items and zero buckets disappear. Keep month navigation/date-only placement and no-fabricated-date tests. Include a calendar item beyond dashboard's seventh discovery/sixth wishlist and assert it survives calendar filters.
- [ ] **Step 2: Run red.** `pnpm --dir web run test -- src/pages/CalendarPage.test.tsx src/calendar/dateBuckets.test.ts`. Expected: missing source select/old boolean behavior.
- [ ] **Step 3: Implement the source select and predicate.** Default `all`; filter all six arrays before rendering. Counts must derive from filtered arrays, not overview totals. Preserve existing date precision selector, score range, month/list switch, chronological day display, and ranking-diff banner.
- [ ] **Step 4: Run green.** Repeat focused tests, then frontend `test`, `typecheck`, `lint`, and `build`; expected zero failures.

### Task 8: Prove feedback reranks both collections without membership drift

**Files:** Modify `internal/feedback/repository.go`, `internal/feedback/repository_integration_test.go`, `web/src/components/FeedbackForm.test.tsx`, `web/src/pages/DashboardPage.test.tsx`; extend `internal/recommendations/overview_repository_integration_test.go`.

**Interfaces:** Preserve `feedback.Command`, `Accepted`, `RankingDiff`, POST feedback/reminder routes, `getRankingDiff`, `FeedbackForm.onRankingUpdated`, and original feedback snapshots. Add raw `g.name` in SQL diff ordering between lower name and UUID to match recommendation ordering. Do not add collection fields to ranking diff; overview owns classification.

- [ ] **Step 1: Write failing regressions.** `TestFeedbackReranksBothCollectionsWithoutChangingMembership` builds at least two discovery/two wishlist games, records like/dislike with reason/free text for each source, processes existing profile/recommendation workflow, and compares pre/post overview by **game ID** (recommendation IDs legitimately change): games may reorder within their section or be excluded after repeated dislikes, but cannot cross sections when wishlist snapshot is unchanged. Assert both v2 factor/weight snapshots, complete totals/calendar, and original feedback snapshot retained. `TestWishlistMembershipChangeReclassifiesOnlyAfterPublishedRebuild` performs successful wishlist change, verifies old published collections until rebuild, then correct new membership. `TestRankingDiffCaseTieMatchesOverviewOrder` proves raw-name tie order. A failed explanation save must leave the previous overview visible. Keep remind-me non-reranking/idempotency tests.

Frontend: in each named section, submit like/dislike, reason and free text via existing form, poll diff, assert `reload` requests overview plus profile and displays server-updated order; remind-me remains available in both sections. Assert no hard-exclude control. Do not optimistically move a card between sources.
- [ ] **Step 2: Run red.** With isolated DB: `GOWORK=off go test -race ./internal/feedback ./internal/recommendations -run 'TestFeedbackReranksBothCollections|TestWishlistMembershipChange|TestRankingDiffCaseTie' -count=1`; `pnpm --dir web run test -- src/components/FeedbackForm.test.tsx src/pages/DashboardPage.test.tsx`. Expected: exact ordering assertion or stale overview reload failure before implementation; a regression already passing remains required evidence, do not force an artificial production change.
- [ ] **Step 3: Implement only uncovered integration gaps.** Align diff SQL order; retain versioned queue identities and finalization lookup from Task 1. Reuse the existing dashboard callback for both collections and refetch complete overview after the server diff completes. Do not modify classifier/ranking weights beyond this plan or create per-collection rankings.
- [ ] **Step 4: Run green.** `GOWORK=off go test -race ./internal/feedback ./internal/recommendations ./internal/httpapi ./internal/sync` with `TEST_DATABASE_URL`; repeat focused frontend tests. Run `codegraph sync .` and `codegraph affected internal/recommendations/service.go internal/recommendations/overview.go internal/feedback/repository.go web/src/pages/DashboardPage.tsx -p .` to identify additional focused regressions, without replacing required verification.

### Task 9: Verify browser state matrix and the real fixture product loop

**Files:** Modify `web/tests/e2e/smoke.spec.ts`, `cmd/e2e-fakes/main.go`, `Makefile`, `docs/local-demo.md`; create `web/tests/e2e/presentation.spec.ts`.

**Interfaces:** Keep real app fixture authentication and fake-upstream URLs behind `TEST_HARNESS_MODE=1`. Add only a fake-server wishlist mode control (`POST /__test/wishlist-mode?mode=public|empty|unavailable|failed`) in `cmd/e2e-fakes`; no application fixture route. Public mode returns at least eight eligible wishlist AppIDs and the seed supplies at least eight eligible discoveries. Unavailable returns the real client's recognized unavailable envelope; failed returns a transport/HTTP failure; empty returns explicit successful `items:[]`. Reset mode at each test start. Isolated presentation UI cases can intercept only browser overview/profile GETs with typed fixture bodies; identify them as rendering checks, not end-to-end persistence proof.

- [ ] **Step 1: Write failing browser assertions.** Expand real `smoke.spec.ts` to GET overview, assert full calendar parity with compatibility endpoint, eight-plus-per-source fixture totals, one+six discovery cards and six secondary wishlist cards, then submit feedback from **both** source sections and await a newer published overview without classification drift. After a successful wishlist sync, switch fake wishlist to unavailable then failed, synchronize again, and assert preserved wishlist membership, non-null unchanged last-success timestamp, stale safe status/reason, and visible stale copy. Then successful empty wishlist + completed rebuild yields ready/zero, not stale. Keep login/private-profile gate, repeat-sync, digest/signed-link, and deletion checks.

In `presentation.spec.ts`, add five table cases: mixed, discovery-only, wishlist-only, fully empty, stale sources. Assert headings/empty text, hero source, caps/totals, calendar source filters intersecting date/score, and coverage status at desktop and narrow width. Browser-intercepted source failure after initial success must retain rendered saved collections; these cases supplement the real fake-upstream loop.
- [ ] **Step 2: Run red.** `make prepare-e2e-db`; `make playwright-browser`; `pnpm --dir web exec playwright test tests/e2e/smoke.spec.ts tests/e2e/presentation.spec.ts`. Expected: new overview/UI/source-mode assertions fail on the pre-feature baseline; if run after Tasks 1–8, fixture mode/expanded-case assertions should fail until the fixture changes land.
- [ ] **Step 3: Implement bounded fixtures and verification wiring.** Extend fake wishlist modes only; no real upstream access or destructive application DB commands. Change `make verify` Playwright invocation to include both `tests/e2e/smoke.spec.ts` and `tests/e2e/presentation.spec.ts` so the new state matrix is not silently excluded. Update `docs/local-demo.md` demo instructions for discovery/watchlist headings, three-state filter, stale wishlist, coverage copy, and seed-only behavior; keep every referenced command/path valid.
- [ ] **Step 4: Run focused green.** Repeat browser command with the separate E2E DB; expect zero failed tests and both section feedback flows visible. If pinned Chromium download is unavailable, use only the README-documented explicit `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/absolute/path/to/chromium` fallback and report the executable/version used. Do not silently change Playwright dependencies.
- [ ] **Step 5: Run final verification.** `codegraph sync .`; `codegraph affected internal/recommendations/types.go internal/recommendations/scoring.go internal/recommendations/service.go internal/recommendations/overview.go internal/recommendations/overview_repository.go internal/sync/service.go internal/httpapi/recommendation_handlers.go internal/feedback/repository.go web/src/api/types.ts web/src/api/client.ts web/src/api/hooks.ts web/src/pages/DashboardPage.tsx web/src/pages/CalendarPage.tsx -p .`; `make verify`; `git diff --check`. Expected: zero failures, sqlc reproduction clean, Go race/API/integration tests executed against `TEST_DATABASE_URL`, frontend test/typecheck/lint/build pass, Docker checks pass, both browser suites pass. Inspect final diff and report actual fresh output; do not claim completion from an agent report or skipped integration suite.

## Acceptance coverage and handoff audit

| Spec acceptance | Owning tests/tasks |
| --- | --- |
| Seven discovery primary cards with seven-plus wishlist candidates | Tasks 3/6/9 mixed fixtures and section-scoped browser assertions |
| Separate labeled wishlist section and badge | Tasks 2/6/9 |
| Wishlist weight zero; factor/legacy snapshot retained; affinity discovery wins | Tasks 1/2/8 |
| Honest empty discovery, no wishlist fallback | Tasks 3/5/6/9 wishlist-only and empty cases |
| Three source filters with score/date precision intact | Tasks 7/9 |
| Deterministic full totals/calendar and all tie-break keys | Tasks 1/3/4/8 |
| Feedback updates both collections without reclassification | Tasks 8/9, including changed membership only after publication |
| Private/failed wishlist preserved and distinguished from successful empty | Tasks 3/4/6/9, including persistence failure marker ordering |
| Frontend, API contract, Go race, browser mixed/single/empty/stale cases | Tasks 3–9; `make verify` runs both browser suites |

Self-review: all spec sections map to tasks above; Go/TS agree on `Overview`/`RecommendationOverview` JSON keys, six calendar bucket names, nullable fields, and status enums. Go reuses `catalog.PresentationStatus`; the explicit enum adapter resolves the catalog diagnostics/presentation vocabulary mismatch. Read collections classify from persisted `Factors.Wishlist`, never current source availability or a new lookup. Limits count the full published candidate set before card slicing. No discovery scan, admission, queue priority, provenance migration, or ingestion implementation is included.

Current code differences that execution must address: `ranking-v1`/wishlist `30`; positive-contribution-only evidence; membership-only generated explanations; raw-name tie-break missing in SQL reads/diff; wishlist success state recorded before data save; no overview route/types; dashboard globally sorts/slices calendar; boolean calendar filter. The provider contract is an additive catalog dependency, with a disabled seed-only fallback. Existing seed-only operation can pass this plan before ingestion lands; do not remove wishlist-only shared rows until the catalog plan's union query is deployed.

Concurrent-work note: at planning time, another authorized task has edits in profile/AI evidence and dashboard styling/copy, including `profile.BuildVectors(input, asOf)` and the profile-summary prompt version. Preserve those changes and re-read overlapping dashboard/test files at execution time. This plan does not alter profile recency weighting, prompt versions, or the unrelated favicon work; its ranking-v2 weight change is limited to `recommendations.Weights.Wishlist`.
