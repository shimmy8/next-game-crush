# Steam Catalog Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Discover and qualify upcoming public Steam games independently of the wishlist while retaining reviewed seed, last successful data, precise release windows, and deterministic recommendations.

**Architecture:** Keep discovery in the catalog module and transport in Steam. PostgreSQL checkpoints, hourly reservations, revision observations, and existing leased jobs make each page replayable; shared provenance and the user's wishlist become separate candidate inputs. Introduce the candidate union before removing legacy wishlist projections, then wire bounded background discovery, enrichment, coalesced rebuilds, and aggregate diagnostics.

**Tech Stack:** Go 1.27.2 (`GOWORK=off`), PostgreSQL 16 with pgvector, pgx/v5, goose migrations, sqlc v1.31.1, React/TypeScript, pnpm 11, Vitest, Playwright.

**Spec:** `docs/superpowers/specs/2026-10-18-steam-catalog-discovery-design.md`; preserve the MVP rules in `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`.

## Global Constraints

- "recommendation horizon: 90 days"; "incremental discovery cadence: at most once per hour"; "metadata evaluation budget: 200 AppIDs per hourly run".
- "rolling backfill uses remaining budget after incremental candidates"; "unknown/Coming Soon discovery freshness window: 180 days".
- Incremental requests use `watermark_at - 5 minutes`; completion advances to captured `scan_started_at`, never completion time.
- Failed-item exponential backoff is capped at 24 hours; `retry_generation=0` identifies the first qualification.
- "Active catalog changes are coalesced for 10 minutes and trigger at most one rebuild per hour. No-op batches do not rebuild."
- Source: `IStoreService/GetAppList/v1` at `https://partner.steam-api.com`; filters/cursor/watermark/page size are in `input_json`, key is a separate server-side query parameter. No scraping, browser credentials, or third-party providers.
- Priorities: user workflow `100`, displayed/recommended refresh `80`, incremental and due retries `50`, backfill `10`; identity remains the idempotency key, and duplicate enqueue may only raise priority.
- PostgreSQL is the only persistent service; retain queue fencing, user deletion tombstones, authentication, CSRF, atomic ranking publication, and immutable published recommendation runs.
- Preserve last successful snapshots on unavailable/malformed/failed upstream responses. Released games lose active membership, retaining metadata and release history. Never synthesize an exact date from a coarse window.
- Add only `db/migrations/00009_catalog_discovery.sql` for this project. Migration `00010` is reserved for the feedback project. Never edit migrations 00001–00008 or generated sqlc files by hand.
- Discovery remains optional; disabled/unavailable discovery must leave seed/wishlist recommendations usable. All URL overrides require `TEST_HARNESS_MODE=1`.
- Logs/diagnostics contain fixed classifications, correlation IDs and aggregate counts only: no SteamID, user UUID, AppID, names, secrets, payloads, raw upstream bodies or model inputs/outputs.
- Do not modify `docs/steam-release-recommender-idea.md`, unrelated work, or existing specs. The commits below are implementation checkpoints; this planning task does not execute them.

## Review Focus

- Duplicate/overlapping pages or a stale worker must not regress a newer revision, duplicate game rows, or publish an uncommitted checkpoint (Tasks 3, 8, 9).
- A restart between page persistence, child enqueue and checkpoint commit must replay exactly that page without losing items or spending the hourly budget twice (Tasks 3, 9, 10).
- Coming Soon with a blank public date is valid unknown precision; wrong AppIDs and malformed envelopes must remain explicit failures (Tasks 5, 6).
- A game shared by seed, discovery and wishlist must retain independent provenance through removal, release and user deletion (Tasks 2, 4, 8).
- Two coordinators, a long-running scan, and exhausted child leases must preserve hourly limits, durable retries and foreground responsiveness (Tasks 7, 9, 10).

---

## Evidence, decisions and execution gates

Read `AGENTS.md`, `README.md`, the spec and this plan before implementation. Planning observed a clean worktree, an up-to-date CodeGraph index, and read source via `codegraph context`, `explore catalog`, and `impact RefreshKnown`. No runtime database counts or live Steam behavior were verified: the spec's 42 entries/42 embeddings are historical context, not acceptance fixtures.

Current mismatches to resolve: `Candidates` starts only from `catalog_entries`; `saveWishlist` writes/deletes `origin='wishlist'`; `RefreshKnown` loops all projected games; `DeleteUserData` deletes games outside that projection; app construction does not actually import the seed until a sync; `GameDetails` rejects blank release text; queues have no priority; the AI planner requires a user; rebuild currently calls explanations on every ranking and no explanation-input cache exists. Tasks below address these without turning discovery into a user-owned workflow. Other tasks began editing shared source files during planning; preserve them and recheck the relevant live diff before execution.

Implementation decisions where the spec leaves mechanics open:

- Default `CATALOG_DISCOVERY_ENABLED=0` is the fail-closed rollout flag; explicitly enable it in the discovery E2E configuration. First incremental watermark is `now - 24 hours`; rolling backfill guarantees older coverage.
- `DiscoveryConfig` bounds: horizon days `1..366` (default `90`), interval hours `1..24` (default `1`), hourly budget `1..1000` (default `200`), unknown freshness days `1..730` (default `180`), page size `1..200` (default `100`, additionally capped to remaining budget). Names are `CATALOG_HORIZON_DAYS`, `CATALOG_DISCOVERY_INTERVAL_HOURS`, `CATALOG_DISCOVERY_HOURLY_BUDGET`, `CATALOG_DISCOVERY_UNKNOWN_FRESHNESS_DAYS`, `CATALOG_DISCOVERY_PAGE_SIZE`.
- The budget counts reserved distinct `(source,appid,last_modified,retry_generation)` qualification attempts per UTC hour; ordinary HTTP/queue retries for that identity do not reserve again. Document that HTTP request count can exceed 200 because transport has bounded retries. Due failures precede new backfill; incremental receives the rest before backfill. If incremental fills each budget indefinitely, diagnostics expose lag rather than pretending backfill is complete.
- Retain historical source rows with `active boolean`; this additional field separates provenance from active membership. Keep `catalog_entries.origin` nullable and deprecated during rollout; it is never read as the new provenance authority. A later separately approved migration can drop it.
- Use `sync_jobs.result` for the durable bounded page manifest, not a second global catalog or an in-memory page. Add small hourly reservation, per-user rebuild debounce and explanation cache tables in migration 00009 for restart/concurrency/cache guarantees.
- A valid empty app list is a successful empty page, not an instruction to remove games. Missing list/envelope, nonprogressing pagination, invalid envelope fields and top-level failures fail the page. Individual invalid AppIDs, item timestamps or metadata are bounded item failures and do not poison valid neighbors; an item without a valid source revision is recorded only in the sanitized page manifest/counters.
- An upstream revision is the source timestamp, not a content hash. Skip already successful source revisions when safe; `unavailable` outcomes and policy-sensitive outcomes (`outside_horizon`, stale unknown) must still be reconsidered on rolling backfill as availability/time/horizon changes. A later reevaluation increments the same generation counter under lock, preserving the exact qualification key format instead of rearming a succeeded queue job. Per-source-revision observations are updated, not multiplied.

Run `codegraph status .` before discovery; if stale run `codegraph sync .`. Before each shared change use `codegraph impact <symbol> -p .`; after source edits run `codegraph sync .`, then `codegraph affected <changed-files...> -p .` to supplement the test commands below.

Start the disposable DB with `make wait-test-db`, then verify it without printing any environment file:

```bash
docker compose exec -T test-db pg_isready -U next_game_crush_test -d next_game_crush_test
export TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable'
```

All integration helpers create/drop only unique schemas on this URL, following `openTestQueue`/`openRankingStore`; fail if the URL's database is not `next_game_crush_test` (or an explicit `next_game_crush_e2e` harness). Never read `.env`, derive test URLs from `DATABASE_URL`, recreate the app database, or treat skipped DB tests as a pass. Every task's red/green command must show its named tests actually ran. Fixed test clock below is `2026-10-18T12:00:00Z`.

## Stable boundaries and file ownership

| Files | Responsibility |
| --- | --- |
| `internal/steam/discovery.go`, `discovery_test.go`, `discovery_types.go` | GetAppList transport, envelope/item validation, dedicated Store Service host |
| `internal/catalog/qualification.go`, `qualification_test.go` | Pure product/release/horizon/freshness policy |
| `internal/catalog/discovery_types.go`, `discovery_status.go`, `discovery_repository.go`, `discovery_repository_test.go` | Typed state, stable presentation contract, transactional persistence, hourly reservation and status projection |
| `internal/catalog/discovery.go`, `discovery_test.go`, `discovery_scan.go`, `discovery_scan_test.go` | Coordinator, page/qualification handlers, retry reconciliation |
| `internal/catalog/sources.go`, `sources_test.go`, existing `service.go` | Seed/shared sources, wishlist union refresh, projection cleanup |
| `internal/catalog/rebuild.go`, `rebuild_test.go` | User-owned rebuild debounce after shared catalog changes |
| `db/migrations/00009_catalog_discovery.sql`, `db/queries/catalog_discovery.sql`, `db/queries/catalog.sql`, `db/queries/jobs.sql` | Additive schema and source queries; `make sqlc` produces bindings |
| Existing jobs/config/app/sync/AI/recommendations/httpapi/frontend files named below | Integrate the new boundaries without changing existing public recommendation/calendar shapes |

The transport types live in `steam` to avoid a `steam -> catalog -> steam` import cycle. `catalog` declares aliases `type DiscoveryPage = steam.DiscoveryPage`, `type DiscoveredApp = steam.DiscoveredApp`, and the exact spec boundary:

```go
type DiscoverySource interface {
    ListChangedGames(context.Context, time.Time, uint32, int) (DiscoveryPage, error)
}
// steam.DiscoveryPage: Apps []DiscoveredApp; NextAppID uint32; HasMore bool
// steam.DiscoveredApp: AppID int64; Name string; LastModified time.Time
```

Contract for the dependent presentation project in `internal/catalog/discovery_status.go` (do not rename; reuse it if the presentation project has landed it first):

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

`(*catalog.DiscoveryService).DiscoveryStatus` implements it; `overview.catalogStatus` belongs to the presentation project. `ActiveDiscoveredGames` counts distinct active games with active `steam_discovery` provenance (including a seed overlap), not all games and not wishlist membership. Status precedence is `disabled`, `degraded` (latest operation failed), `stale` (last successful incremental absent or older than two configured intervals after initial work), `updating` (no completed backfill pass or active page), then `ready`. Initial enabled startup with active work is `updating`; absent timestamps serialize as `null`. A completed pass is coverage progress, never a claim of a complete realtime Steam mirror.

Presentation maps provider `updating → backfilling`, `degraded → unavailable`, other values identically; its public overview status enum is owned by the presentation spec. Catalog diagnostics retain `disabled|updating|ready|stale|degraded`.

Keep `recommendations.Repository.Candidates(context.Context, uuid.UUID, profile.Profile, int) ([]Candidate,error)`, `Service.Latest/Get`, `/api/recommendations`, `/api/calendar`, and their existing JSON fields stable. Add `SetHorizonDays(int) error` on `*recommendations.SQLRepository` instead of changing its constructor. Presentation consumes the existing recommendation models and this status reader; no dependency on checkpoint tables or queue payloads.

### Task 1: Make wishlist membership an independent candidate/refresh input

**Files:** Modify `internal/recommendations/retrieval.go`, `db/queries/catalog.sql`, `internal/catalog/service.go`; Test `internal/recommendations/repository_integration_test.go`, `internal/sync/service_test.go`; Generate `internal/store/sqlc/catalog.sql.go`.

**Interfaces:** Consumes current tables only. Produces `ListKnownCatalogGames(ctx context.Context) ([]dbgen.Game,error)` as a generated query over shared `catalog_entries UNION wishlist_entries` (single-user application), excluding wishlist-only games authoritatively known released in a valid normalized Store snapshot; retains `RefreshKnown(context.Context) error` and `Candidates` signatures.

- [ ] **Step 1: Write failing union tests.** Add `TestCandidatesIncludeWishlistWithoutCatalogProjection`: using `openRankingStore`, insert an unreleased wishlist game without `catalog_entries` or embedding, an equivalent other-user-only game and a shared game also wishlisted; assert `len(ownWishlistMatches)==1`, `len(otherUserMatches)==0`, `len(sharedMatches)==1`. Add `TestRefreshKnownRefreshesWishlistWithoutProjection` in sync tests; after deleting the legacy wishlist projection assert its fake `GameDetails` is called and release/fetch state updated. Repeat with a cached Store snapshot `ComingSoon=false` and future raw date: candidate must be excluded.

  Assertion in the named union test, after collecting matching candidates from `repo.Candidates(ctx,userID,p,100)`:

  ```go
  if len(ownWishlistMatches) != 1 || len(otherUserMatches) != 0 || len(sharedMatches) != 1 {
      t.Fatal("candidate union lost, leaked, or duplicated membership")
  }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/recommendations ./internal/sync -run 'TestCandidatesIncludeWishlistWithoutCatalogProjection|TestRefreshKnownRefreshesWishlistWithoutProjection' -count=1 -v`. Expect named assertion failures before SQL changes.
- [ ] **Step 3: Implement union reads.** Change the candidate base CTE to `SELECT game_id FROM catalog_entries UNION SELECT game_id FROM wishlist_entries WHERE user_id=$1`; preserve exclusions, mandatory wishlist override of semantic limit/horizon, stable ordering and deduplication. Exclude games whose valid normalized Store snapshot says released/non-game; absence of a valid snapshot preserves current fallback. Add generated `ListKnownCatalogGames` union query and call it from `RefreshKnown`. Do not remove any legacy rows/writers yet. Run `make sqlc`.
- [ ] **Step 4: Run green:** repeat Step 2, then `GOWORK=off go test -race ./internal/recommendations ./internal/sync -count=1`. Expected zero failures with DB tests executed. This is the verified union gate before Task 4 cleanup.
- [ ] **Step 5: Commit:** `git add db/queries/catalog.sql internal/store/sqlc/catalog.sql.go internal/catalog/service.go internal/recommendations/retrieval.go internal/recommendations/repository_integration_test.go internal/sync/service_test.go`; `git commit -m "refactor: read shared catalog and wishlist candidates independently"`.

### Task 2: Add additive discovery persistence and priority schema

**Files:** Create `db/migrations/00009_catalog_discovery.sql`; Modify/Test `internal/store/migration_upgrade_test.go`.

**Interfaces:** Produces the following schema, keeping existing rows and recommendation versions intact. No migration deletes wishlist-only projections.

- `catalog_entry_sources`: `(game_id uuid REFERENCES games ON DELETE CASCADE, source text CHECK IN ('seed','steam_discovery'), first_seen_at timestamptz, last_seen_at timestamptz, active boolean NOT NULL DEFAULT true)`, PK `(game_id,source)`; backfill only old `origin='seed'`. Make deprecated `catalog_entries.origin` nullable; new writes omit it.
- `catalog_discovery_state`: PK `(source,mode)`, source `steam`, mode `incremental|backfill`, cursor bigint `0..4294967295`, nullable watermark/start/completed timestamps, `availability`, bounded `last_error`, `updated_at`. Seed both rows, first watermark initialized by coordinator clock, not migration wall clock.
- `catalog_discovery_observations`: PK `(source,steam_app_id,last_modified)`, positive AppID bounded by uint32, statuses exactly `admitted|outside_horizon|released|not_game|unavailable|failed`; precision nullable or one of the parser values, optional date only for `day`, raw release text bounded to 512 chars; first/last-attempt timestamps, attempts, retry_generation, next_retry_at, safe error classification. Index due failed observations. Keep these rows independent of user FKs.
- `catalog_discovery_budget`: PK `hour_at timestamptz`, `reservations jsonb NOT NULL DEFAULT '{}'`, `used integer`, `counts jsonb` for latest aggregate counters; transactionally lock the UTC-hour row. Reservation keys are bounded hashes of canonical qualification identities, never credentials. Enforce nonnegative used/counts and prune rows older than seven days once no active page references their hour.
- `catalog_rebuild_state`: PK `user_id REFERENCES users ON DELETE CASCADE`, `dirty_revision bigint`, `enqueued_revision bigint`, nullable `dirty_since`, `last_enqueued_at`, `last_scheduled_at`, and `pending_job_id uuid REFERENCES sync_jobs ON DELETE SET NULL`; the same row lock and existing job-user trigger fence debounce scheduling against deletion.
- `catalog_explanation_cache`: `(game_id uuid REFERENCES games ON DELETE CASCADE, model text, input_hash text, explanation jsonb, created_at timestamptz DEFAULT now())`, PK `(game_id,model,input_hash)`; cache hash includes public grounded evidence, schema and prompt version. Never store private profile input/raw prompts in this table.
- `sync_jobs.priority smallint NOT NULL DEFAULT 50 CHECK (priority BETWEEN 1 AND 100)`; replace claim-supporting index with `(priority DESC,available_at,created_at,id)` for eligible statuses. No special unique constraint on priority.

- [ ] **Step 1: Write `TestCatalogDiscoveryMigrationPreservesVersion8Data`.** Follow the existing goose `UpTo(ctx,8)` isolated-schema test; seed one seed row, one wishlist row, a release observation and published recommendation run; upgrade. Assert `seedSources==1`, `wishlistSources==0`, `legacyWishlistProjection==1`, `publishedRuns==1`, old jobs `priority==50`; duplicate `(source,appid,last_modified)` is rejected; multiple source rows for one game are accepted; day-only date and mode/status constraints reject invalid inserts.

  ```go
  if seedSources != 1 || wishlistSources != 0 || legacyWishlistProjection != 1 || publishedRuns != 1 || oldPriority != 50 {
      t.Fatal("additive migration changed existing data or defaults")
  }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/store -run '^TestCatalogDiscoveryMigrationPreservesVersion8Data$' -count=1 -v`. Expect missing relations/columns.
- [ ] **Step 3: Implement migration 00009 with goose Up/Down.** Add the schema above and indexes. Down raises a fixed safe error if discovery source/history or NULL-origin projections exist, because the old binary cannot represent them; otherwise it removes new tables/priority and restores the old origin NOT NULL and queue index. Document disable-only rollback for a populated installation: retain new schema and last catalog, turn the flag off, and do not pretend goose Down is lossless.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/store -count=1`; `make sqlc`. Expected fresh and v8-upgrade tests pass and generation succeeds.
- [ ] **Step 5: Commit:** stage only migration, migration tests and generated model changes; `git commit -m "feat: persist bounded Steam discovery state and provenance"`.

### Task 3: Implement transactional discovery repository and status types

**Files:** Create `db/queries/catalog_discovery.sql`, `internal/steam/discovery_types.go`, `internal/catalog/discovery_types.go`, `internal/catalog/discovery_status.go`, `internal/catalog/discovery_repository.go`, `internal/catalog/discovery_repository_test.go`; generate `internal/store/sqlc/` through `make sqlc`. Reuse a presentation-owned `discovery_status.go` unchanged if it already exists.

**Interfaces:** Define `DiscoveryMode string` (`incremental`, `backfill`); `Revision {AppID int64; LastModified time.Time}`; `Observation {Revision; Status string; Window ReleaseWindow; Attempts int; RetryGeneration int; NextRetryAt *time.Time; SafeError string}`; `ScanState {Source string; Mode DiscoveryMode; CursorAppID uint32; WatermarkAt, ScanStartedAt, ScanCompletedAt *time.Time; Availability, LastError string}`. `DiscoveryConfig {Enabled bool; HorizonDays, IntervalHours, HourlyBudget, UnknownFreshnessDays, PageSize int}` with `Validate() error`. Define the stable `PresentationStatus`/reader above.

`NewDiscoveryRepository(*store.Store) *DiscoveryRepository`; methods `State(ctx, mode) (ScanState,error)`, `BeginScan(ctx, mode, now time.Time) (ScanState,error)`, `SaveObservation(ctx, Observation) error`, `Reserve(ctx, hour time.Time, identity string, limit int) (bool,error)`, `PresentationStatus(ctx, enabled bool, now time.Time, interval time.Duration) (PresentationStatus,error)`. `BeginScan` retains an unfinished scan's start/lower bound/cursor under row lock; only a completed new scan resets them. Query names: `GetCatalogDiscoveryState`, `UpsertDiscoveryObservation`, `ListDueDiscoveryFailures`, `GetDiscoveryObservation`, `CountDiscoveryProvenance`, `ListPendingDiscoveryQualifications`. Transactional page/retry methods are added in Tasks 9–10; no loosely composed public setters for cursor/watermark.

- [ ] **Step 1: Write `TestDiscoveryRepositoryUpsertsRevisionAndBudget`.** Define `openCatalogTestStore(t *testing.T) *store.Store` here using only guarded `TEST_DATABASE_URL` and isolated schemas. Save failed generation 0, then admitted generation 1 at the same timestamp; SQL asserts `count==1`, `attempts==2`, `next_retry_at IS NULL`, safe error cleared, first timestamp unchanged. Concurrently reserve the same identity twice then 201 distinct identities at limit 200: assert `used==200`, duplicate reservation succeeds without extra spend. `TestDiscoveryRepositoryKeepsDistinctRevisions` saves source timestamps in reverse completion order and asserts exactly one row for each timestamp; canonical-game stale-writer protection is tested in Task 8. `TestDiscoveryPresentationStatus` table-tests disabled/updating/ready/stale/degraded and distinct provenance overlap counts. Include a goose Down test for the populated-schema safe refusal from Task 2.

  ```go
  if count != 1 || attempts != 2 || nextRetry != nil || safeError != "" || used != 200 {
      t.Fatal("revision upsert or hourly reservation is not idempotent")
  }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog -run 'TestDiscoveryRepository|TestDiscoveryPresentationStatus' -count=1 -v`. Expect missing types/methods.
- [ ] **Step 3: Implement the types, repository and named source queries.** Use row locks and `WithTx`, UTC-second source timestamps, bounded enum diagnostics and counters retained on success. `SaveObservation` takes the monotone cumulative `Observation.Attempts` supplied by the fenced caller, using `GREATEST` on conflict; it does not increment blindly on duplicate persistence. Implement status precedence exactly as the stable contract. Run `make sqlc`.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/catalog ./internal/store -count=1`. Expected DB tests execute and all pass.
- [ ] **Step 5: Commit:** stage new catalog repository/types/tests, query and generated files only; `git commit -m "feat: add transactional discovery repository"`.

### Task 4: Switch provenance writers, retire wishlist projections and preserve shared history

**Files:** Create `internal/catalog/sources.go`, `internal/catalog/sources_test.go`; Modify `internal/catalog/service.go`, `internal/sync/service.go`, `internal/sync/service_test.go`, `internal/httpapi/data_handlers.go`, `internal/httpapi/data_service_integration_test.go`, `db/queries/catalog.sql`.

**Interfaces:** Add `(*catalog.Service).Bootstrap(ctx context.Context) error`, `CleanupLegacyWishlistProjection(ctx context.Context) error`, and private transactional `setSharedSource(ctx context.Context, tx pgx.Tx, gameID uuid.UUID, source string, active bool, seenAt time.Time) (changed bool,error)`. Bootstrap imports seed provenance without HTTP, preserving valid Store snapshots and known released status. Active projection exists iff an active shared source exists; wishlist writes touch only wishlist tables/games.

- [ ] **Step 1: Write `TestSharedSourcesSurviveWishlistRemoval`, `TestBootstrapKeepsKnownReleasedSeedInactive`, `TestLegacyWishlistCleanupPreservesUnion`, and `TestDeleteRetainsInactiveDiscoveryHistory`.** Assert seed+discovery source count remains `2` after wishlist removal; legacy wishlist-only `catalog_entries` is removed but game, release history and own wishlist survive; candidate IDs before/after cleanup are identical; deleting the user removes personal-only imports and user-owned rebuild jobs, but preserves both active and inactive shared-source games, release history, discovery observations and global discovery jobs. Reuse the existing in-flight deletion test to prove late user workers still cannot recreate personal data.

  ```go
  if sourceCount != 2 || legacyProjectionCount != 0 || wishlistCount != 1 || releaseHistoryCount == 0 {
      t.Fatal("wishlist projection cleanup damaged independent sources")
  }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog ./internal/sync ./internal/httpapi -run 'TestSharedSources|TestBootstrap|TestLegacyWishlist|TestDeleteRetainsInactiveDiscoveryHistory|TestDeleteFences' -count=1 -v`. Expect old origin-dependent behavior/missing methods to fail.
- [ ] **Step 3: Implement source writes and cleanup after Task 1's green gate.** Move seed provenance into source rows; preserve original timestamps and content. Stop wishlist projection writes/removals in `saveWishlist` and origin-dependent released removal in refresh; deactivate shared sources on authoritative release, never on unavailable/failed metadata. Cleanup transaction deletes only legacy `origin='wishlist'` projections without any active shared source; never delete the underlying game. Startup invokes cleanup only after the new union code is installed; migration itself never deletes those rows. In deletion cleanup, protect `games` by **any** `catalog_entry_sources` history and by discovery observations' AppIDs, even when inactive. Generate updated queries.
- [ ] **Step 4: Run green:** repeat Step 2, then the full catalog/sync/httpapi/recommendations packages with `GOWORK=off go test -race ./internal/catalog ./internal/sync ./internal/httpapi ./internal/recommendations -count=1`. Update old tests that asserted wishlist projection membership to assert wishlist union behavior; keep their release/privacy/fallback expectations.
- [ ] **Step 5: Commit:** stage explicit files plus generated query changes; `git commit -m "refactor: separate shared catalog provenance from wishlist membership"`.

### Task 5: Add GetAppList transport and bounded configuration

**Files:** Create `internal/steam/discovery.go`, `internal/steam/discovery_test.go`; Modify `internal/steam/http_client.go`, `internal/steam/types.go`, `internal/steam/http_client_test.go`, `internal/config/config.go`, `internal/config/config_test.go`, `.env.example`, `docker-compose.yml`; consume discovery transport types from Task 3.

**Interfaces:** `(*steam.HTTPClient).ListChangedGames(ctx context.Context, since time.Time, afterAppID uint32, limit int) (DiscoveryPage,error)`; `HTTPConfig.StoreServiceBaseURL string`; `HTTPClient.storeServiceBase *url.URL`; new `AppListOperation Operation="app_list"`. Add `config.Config.CatalogDiscovery catalog.DiscoveryConfig` and `SteamStoreServiceBaseURL string`, parsed from `STEAM_STORE_SERVICE_BASE_URL`; use spec aliases from Task 3. Keep `steam.Client` unchanged so player fakes need no discovery method.

- [ ] **Step 1: Write `TestGetAppListContracts` with three distinct httptest hosts.** Assert only Store Service receives `/IStoreService/GetAppList/v1/`, key separate from `input_json`; decode JSON and assert `include_games=true`, all four other product flags false, `if_modified_since==1792324500` for `2026-10-18T11:55:00Z`, `last_appid==100`, `max_results==2`; backfill zero `since` omits `if_modified_since`. Fixture ordered apps `101,102`, `last_modified`, `last_appid=102`, `have_more_results=true` yields cursor 102; explicit empty apps/false more succeeds. Missing response/apps, bad JSON, more-with-empty/nonprogressing cursor fail. Invalid individual IDs are retained as bounded invalid items for the catalog layer to record by manifest ordinal; they cannot become games. Test HTTP 429 `Retry-After`, cancellation, response size, host isolation, and absence of secret/URL/body in errors. `TestGameDetailsBlankComingSoonDate` accepts blank date only with `coming_soon=true`. `TestDiscoveryConfigBoundsAndOverrideGuard` pins defaults, bounds, rejects non-boolean enable values and Store Service override outside harness.

  ```go
  page, err := client.ListChangedGames(ctx, since, 100, 2)
  if err != nil || len(page.Apps) != 2 || page.NextAppID != 102 || !page.HasMore {
      t.Fatalf("discovery page did not preserve the public fixture contract")
  }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/steam ./internal/config -run 'TestGetAppList|TestGameDetailsBlankComingSoonDate|TestDiscoveryConfig' -count=1 -v`. Expect missing transport/config and existing blank-date rejection.
- [ ] **Step 3: Implement transport/config.** Use `getWithoutCache` and the same limiter/semaphore/timeouts/retry machinery; no second unrestricted HTTP client. Validate supplied limits and uint32 timestamp range before request, disable redirects as current client does, and support the dedicated default host. Missing/invalid envelope fields are page failures; a malformed item ID/timestamp is represented by `AppID=0` without retaining raw text (manifest ordinal distinguishes multiple invalid items; no observation row with invalid key). Keep API key server-side. Update Compose's explicit environment pass-through and `.env.example` with defaults/bounds; never read `.env`. Ensure computed fixture timestamp above is verified against `time.Date(...).Unix()` in the test rather than trusting prose.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/steam ./internal/config -count=1`; `docker compose config --quiet`. Expected zero failures and no unexpected secret-bearing output.
- [ ] **Step 5: Commit:** stage only named files; `git commit -m "feat: add bounded Steam Store discovery adapter"`.

### Task 6: Implement pure precision-preserving qualification policy

**Files:** Create `internal/catalog/qualification.go`, `internal/catalog/qualification_test.go`; Modify `internal/recommendations/retrieval.go`, `internal/recommendations/repository_integration_test.go`.

**Interfaces:** `Qualification {Status string; Window ReleaseWindow}`; `Qualify(app DiscoveredApp, details steam.GameDetails, now time.Time, cfg DiscoveryConfig) Qualification`; `IntersectsHorizon(window ReleaseWindow, now time.Time, days int) bool`; `(*recommendations.SQLRepository).SetHorizonDays(days int) error`. Reuse `ParseReleaseDate`; compute coarse window boundaries transiently without writing those boundaries as release dates.

- [ ] **Step 1: Write `TestQualificationPolicy`.** At fixed clock assert upcoming `2027-01-16` admitted (90-day boundary), `2027-01-17` outside; intersecting `January 2027`, `Q1 2027`, `2027` admitted; `September 2026` outside/released; Coming Soon or unparseable/blank text modified exactly `now-180d` admitted, one second older outside; `ComingSoon=false` => released even if raw date future; DLC/software/demo => not_game; mismatched/zero AppID => failed. For all coarse/unknown windows assert `Date==nil`; precise date keeps `PrecisionDay`. `TestCandidatesConfiguredHorizon` verifies shared non-wishlist day 91 is excluded at 90 and included at 120, while future wishlist remains mandatory.

  In table subtests, `tc.wantStatus`/`tc.wantPrecision` are literal values above:

  ```go
  got := Qualify(app, details, now, cfg)
  if got.Status != tc.wantStatus || got.Window.Precision != tc.wantPrecision { t.Fatal("qualification mismatch") }
  if got.Window.Precision != PrecisionDay && got.Window.Date != nil { t.Fatal("invented exact date") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog ./internal/recommendations -run 'TestQualificationPolicy|TestCandidatesConfiguredHorizon' -count=1 -v`. Expect missing policy/configurable horizon.
- [ ] **Step 3: Implement policy and horizon setter.** Product type comparison is normalized case-insensitive exact `game`; lower/upper day interval is inclusive UTC calendar dates. Unknown/Coming Soon use last-modified freshness, never scan time substituted for a missing upstream timestamp. `ComingSoon=true` with blank date produces `coming_soon`, otherwise preserve parser precision/raw text. Replace fixed retrieval 90 with defaulted validated field; keep wishlist exception and deterministic ranking.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/catalog ./internal/recommendations -count=1`. Expected parser and candidate regression suites pass.
- [ ] **Step 5: Commit:** stage policy/test/retrieval files; `git commit -m "feat: qualify discovery games using precise release windows"`.

### Task 7: Add queue priorities, transaction enqueue and safe deferred polling

**Files:** Modify `internal/jobs/queue.go`, `internal/jobs/queue_test.go`, `db/queries/jobs.sql`; generate `internal/store/sqlc/jobs.sql.go` and models.

**Interfaces:** `jobs.Spec.Priority int` (zero normalizes to 50; otherwise 1..100); `jobs.EnqueueInTx(ctx context.Context, queries *dbgen.Queries, spec Spec) (uuid.UUID,error)` for callers already holding a pgx transaction, sharing all enqueue validation and trigger protections with `Queue.Enqueue`/`AwaitChildren`. `(*Queue).DeferUntil(ctx context.Context, lease Lease, availableAt time.Time) error` releases the owned lease to queued status, refunds only the polling attempt, finishes its job_attempt as waiting, and leaves `available_at` in the future. Handler returns `jobs.ErrDeferred` after successful defer.

- [ ] **Step 1: Write `TestClaimOrdersPriorityThenStableTime`, `TestReenqueueRaisesButNeverLowersPriority`, `TestAwaitChildrenPreservesPriorityAndAvailableAt`, `TestDeferUntilIsFenced`.** Enqueue backfill 10 before user 100 at equal times and assert 100 claims first; priority ties order by available_at/created_at/id. Duplicate keys retain ID/status/attempts/payload, priority rises 10→80 and stays 80 after enqueue 50. Tx rollback leaves no job. Stale fencing token cannot defer or refund attempts; valid defer cannot be immediately reclaimed or produce terminal success; existing optional-child dead/lease-expiry promotion remains working.

  ```go
  lease, err := queue.Claim(ctx, "priority-test", time.Minute)
  if err != nil || lease == nil || lease.ID != foregroundID { t.Fatal("backfill claimed ahead of foreground work") }
  if firstID != duplicateID || storedPriority != 80 { t.Fatal("duplicate enqueue changed identity or lowered priority") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/jobs -run 'TestClaimOrders|TestReenqueueRaises|TestAwaitChildrenPreservesPriority|TestDeferUntil' -count=1 -v`. Expect absent priority/defer behavior.
- [ ] **Step 3: Implement shared validation and SQL.** Claim order `priority DESC,available_at,created_at,id`; conflict update `priority=GREATEST(sync_jobs.priority,EXCLUDED.priority)` only. Propagate priority and `AvailableAt` through both enqueue paths. `DeferUntil` requires unexpired owned lease and updates job plus attempt in one transaction; no sleeps and no reset/rearm of historical dead jobs. `make sqlc`.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/jobs ./internal/sync ./internal/feedback ./internal/notifications -count=1`. Expected existing queues, optional children, notifications and lifecycle tests pass.
- [ ] **Step 5: Commit:** stage queue/query/tests/generated files; `git commit -m "feat: prioritize foreground jobs and defer discovery polling safely"`.

### Task 8: Persist independent qualification and content-guarded enrichment

**Files:** Create `internal/catalog/discovery.go`, `internal/catalog/discovery_test.go`; Modify `internal/catalog/discovery_repository.go`, `internal/ai/service.go`, `internal/ai/service_test.go`.

**Interfaces:** `NewDiscoveryService(repo *DiscoveryRepository, source DiscoverySource, metadata steam.Client, queue *jobs.Queue, cfg DiscoveryConfig, logger *observability.Logger) (*DiscoveryService,error)`; `RegisterHandlers(*jobs.Worker) error` initially registers qualification, expanded in Tasks 9–10; `SetEnrichmentPlanner(DiscoveryEnrichmentPlanner)` where `PlanGameFeatureJob(context.Context, uuid.UUID, string, int) (jobs.Spec,error)` takes game ID, canonical content hash and priority. AI implements this method; existing `PlanFeatureJobs` remains. `(*DiscoveryRepository).MarkCandidatesChanged(ctx context.Context, tx pgx.Tx, now time.Time) error` marks existing profiled users transactionally. `QualificationPayload {Version int; App DiscoveredApp; RetryGeneration int; BudgetHour time.Time; Priority int}` uses strict v1 decoding. `QualifyJobKind="catalog.qualify"`; key exactly `catalog.qualify:v1:<appid>:<unix-last-modified>:retry:<generation>`.

- [ ] **Step 1: Write `TestDiscoveryQualificationCreatesSharedCandidate`, `TestQualificationRevisionReplayAndFailurePreservation`, and `TestDiscoveryFeaturePlanningUsesContentAndModel`.** A non-wishlist game becomes one games row + active discovery source/projection + fetch state + release observation + admitted revision observation; rerun produces no duplicate histories/jobs. New metadata content schedules one feature job; unchanged price-only revision schedules zero; model change schedules new work; embedding key carries embedding-model version as well as feature content. Malformed/unavailable details preserve prior canonical snapshot/source active state; unavailable is a successful nonadmission observation. Retryable transport failure returns error, stores no false successful empty state; invalid product/released/outside transitions deactivate discovery source, preserve seed membership unless an authoritative released/non-game response invalidates all active shared eligibility. A slow old revision cannot undo a newer success. All per-item commits are fenced by the qualification lease.

  ```go
  if gameCount != 1 || observationCount != 1 || activeDiscoveryCount != 1 || wishlistCount != 0 {
      t.Fatal("discovered game was not independently admitted exactly once")
  }
  if unchangedFeatureJobs != 0 || changedFeatureJobs != 1 || newModelJobs != 1 { t.Fatal("enrichment identity guard failed") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog ./internal/ai -run 'TestDiscoveryQualification|TestQualificationRevision|TestDiscoveryFeaturePlanning' -count=1 -v`. Expect missing service/planner.
- [ ] **Step 3: Implement qualification transaction.** Fetch outside transaction, then verify current lease token/expiry and lock revision/game before atomic observation + game/snapshot + fetch state + release history + source/projection updates + AI enqueue + dirty-state change. Use normalized metadata content hashing, excluding observed timestamps/revision/price-only changes. Same revision retry success clears retry fields, retains attempts/generation/first-observed. For valid nonadmission metadata, retain game/release history for previously known games; do not create a shared active projection. Skip AI for nonadmission and disabled capability; never attach a global feature job to a user's workflow. AI key includes game/content/schema/prompt/text+embedding model versions; reuse existing content-hash guards inside handlers.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/catalog ./internal/ai -count=1`. Expected deterministic fallback works without OpenAI and previous successful features/vectors survive failure.
- [ ] **Step 5: Commit:** stage explicit service/repository/AI files; `git commit -m "feat: qualify discovered revisions and plan guarded enrichment"`.

### Task 9: Make pages replayable and reconcile optional failures before checkpoints

**Files:** Create `internal/catalog/discovery_scan.go`, `internal/catalog/discovery_scan_test.go`; Modify `internal/catalog/discovery_repository.go`, `db/queries/catalog_discovery.sql`; generate bindings.

**Interfaces:** `ScanPayload {Version int; Mode DiscoveryMode; ScanStartedAt time.Time; AfterAppID uint32; BudgetHour time.Time; PageGeneration int}`; `PageManifest {Version int; Mode DiscoveryMode; ScanStartedAt time.Time; AfterAppID, NextAppID uint32; HasMore bool; Items []PageItem}`; `PageItem {App DiscoveredApp; Ordinal int; JobID uuid.UUID; RetryGeneration int; Invalid bool}`. Manifest version 1 is stored in scan job `result` before children become claimable. `(*DiscoveryRepository).SchedulePage(ctx context.Context, lease jobs.Lease, payload ScanPayload, page DiscoveryPage, cfg DiscoveryConfig) (PageManifest,error)` and `ReconcilePage(ctx context.Context, lease jobs.Lease, manifest PageManifest, now time.Time) (ready bool, changed bool, err error)` perform fenced transactions. Kinds `catalog.discover.incremental` and `catalog.discover.backfill`; page keys `catalog.discover.<mode>:v1:<scan-start-unix>:after:<cursor>:retry:<page-generation>`. Coordinator may create a higher generation only after that page job is terminal dead; it reuses its committed manifest where present instead of abandoning scheduled items.

- [ ] **Step 1: Write `TestDiscoveryPageReplayAfterRestart`, `TestDiscoveryPageWaitsForTerminalItems`, `TestDiscoveryMalformedEnvelopeKeepsCheckpoint`, `TestDiscoveryOverlapCannotSkipLowAppID`, `TestOptionalQualificationLeaseExhaustion`.** Stop after manifest/children commit and reconstruct service; assert exact cursor/items/scan-start retained and no duplicate budget/job. Replay after child success but before checkpoint yields one observation. A valid, malformed and unavailable item together admit the valid one and advance only after invalid/failed outcomes recorded. Exhaust a child by crash/expired max-attempt lease, then assert reconciler writes `failed,next_retry_at` before advancing. Fault-inject checkpoint commit: cursor and watermark unchanged and original shared rows intact. For page1 app100/page2 app200, modify app50 after scan start; all requests retain old lower bound and final watermark equals start, next scan overlap picks app50. Empty explicit final page completes scan; `has_more=true` without progress fails. Add `TestDiscoveryBackfillRechecksUnchangedPolicyRevision`: an outside-horizon revision becomes eligible after clock advance, uses a new generation while its old succeeded job stays unchanged, and updates its one observation.

  ```go
  if !state.WatermarkAt.Equal(scanStart) || requestLowerBounds[0] != requestLowerBounds[1] {
      t.Fatal("paginated scan changed its lower bound or advanced to completion time")
  }
  if nextScanAdmittedLowAppID != 1 || replayObservationCount != 1 { t.Fatal("restart/overlap lost or duplicated revision") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog -run 'TestDiscoveryPage|TestDiscoveryMalformed|TestDiscoveryOverlap|TestDiscoveryBackfill|TestOptionalQualification' -count=1 -v`. Expect missing page methods.
- [ ] **Step 3: Implement phased scan handler.** On first execution load/continue persisted state and fetch one bounded page; atomically reserve identities, save manifest and enqueue qualification jobs `ParentJobID=lease.ID,Required=false,Optional=true`, priority from mode. On replay read manifest rather than refetching moving upstream data. A revision job already owned by another page retains its identity/parent; reconciler checks **all manifest JobIDs**, not merely `GetJobChildren`, so duplicate pages cannot advance early. Pending referenced jobs call `DeferUntil(now+5s)` and return `ErrDeferred`. Reconcile terminal succeeded observations and dead jobs in one transaction; invalid AppIDs count as skipped/failed manifest items without violating valid-key observation constraints. Backfill revisits policy-sensitive successful observations using a new locked retry generation when the cached release window can now qualify; it must not reuse a succeeded generation-0 job. Advance only persisted page's cursor with expected mode/start/cursor and valid lease. Last incremental page sets watermark to start; backfill wraps cursor to 0 only at full-pass completion, retaining completed timestamp while next pass starts. Incremental may enqueue continuation within remaining capacity; backfill schedules at most one new page per hourly tick. Otherwise leave the unfinished scan for the next tick; never mark a budget pause as a completed scan. `make sqlc`.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/catalog ./internal/jobs -count=1`. Expected restart, stale lease and optional failure scenarios pass with real isolated PostgreSQL.
- [ ] **Step 5: Commit:** stage page/repository/query/test/generated files; `git commit -m "feat: reconcile replayable discovery pages before advancing checkpoints"`.

### Task 10: Coordinate durable retry generations, hourly budgets and future ticks

**Files:** Modify `internal/catalog/discovery.go`, `internal/catalog/discovery_test.go`, `internal/catalog/discovery_repository.go`, `db/queries/catalog_discovery.sql`.

**Interfaces:** `(*DiscoveryService).EnsureScheduled(ctx context.Context) error`; `Nudge(ctx context.Context) error`; `(*DiscoveryRepository).ScheduleDueRetries(ctx context.Context, now time.Time, cfg DiscoveryConfig) (int,error)`; `TickJobKind="catalog.discovery.tick"`, key `catalog.discovery.tick:v1:<scheduled-hour-unix>`. Tick payload `{version:1,scheduled_at:<UTC timestamp>}`. Store source retry delay `min(1h*2^retry_generation,24h)` with saturating arithmetic; first dead generation 0 becomes due in one hour.

- [ ] **Step 1: Write `TestDiscoveryTickSurvivesRestartAndNudge`, `TestConcurrentCoordinatorsReserveOnce`, `TestDeadDueRetriedAdmittedUsesOneObservation`, `TestIncrementalBudgetPrecedesBackfill`, `TestDisabledDiscoverySchedulesNothing`.** Two simultaneous coordinators reserve/enqueue one next generation under observation/hour locks, leaving original dead job byte-for-byte terminal. Assert keys end `retry:0` then `retry:1`, same observation count 1, next retry cleared after admission; 201 candidates produce at most 200 reserved jobs/hour; retry/incremental exhaust budget before backfill; 20 repeated nudges cannot start a second incremental within an hour; restarting reuses one pending future tick. A scan paused at cursor100 resumes the same start/lower bound next hour. After a coordinator job exhausts attempts, a new startup/next hourly watchdog key can restore progress without mutating dead history.

  ```go
  if oldJobStatus != "dead" || newJobID == oldJobID || retryGeneration != 1 || observationCount != 1 {
      t.Fatal("durable retry rearmed history or duplicated the source revision")
  }
  if hourlyUsed > 200 || pendingFutureTicks != 1 || secondIncrementalStarts != 0 { t.Fatal("coordinator bound failed") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog -run 'TestDiscoveryTick|TestConcurrentCoordinators|TestDeadDue|TestIncrementalBudget|TestDisabledDiscovery' -count=1 -v`. Expect coordinator methods absent.
- [ ] **Step 3: Implement tick/retry transactions.** Tick first ensures its successor at `available_at` before starting scan work; take source-mode/hour row locks, process due failures, then incremental, then backfill only when incremental page reconciliation establishes remaining capacity. Increment retry generation and atomically enqueue distinct key using `jobs.EnqueueInTx`; never clear/rearm dead queue rows. Reserve per qualification identity once. Skip request/enqueue when no capacity; resume unfinished scans next tick. `Nudge` coalesces into the next allowed scan slot and cannot move cadence earlier. Disabled handlers safely no-op without fetching/mutating prior catalog. Update bounded latest-run aggregate counters transactionally and prune only unreferenced old budget rows. `make sqlc`.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/catalog ./internal/jobs -count=1`. Expected no sleeps, no duplicate active coordinator slots, and no budget overrun under concurrent execution.
- [ ] **Step 5: Commit:** stage coordinator/repository/query/tests/generated files; `git commit -m "feat: coordinate bounded discovery and durable item retries"`.

### Task 11: Wire startup/sync and bound known-game refresh priority

**Files:** Modify `internal/app/app.go`, `internal/sync/service.go`, `internal/sync/service_test.go`, `internal/catalog/service.go`, `internal/catalog/sources.go`, `internal/ai/service.go`, `internal/profile/service.go`, `internal/recommendations/service.go`, `internal/feedback/service.go`; Create `internal/app/discovery_test.go`.

**Interfaces:** `(*sync.Service).SetDiscoveryNudger(interface{Nudge(context.Context) error})`; `(*catalog.Service).RefreshGame(ctx context.Context, appID int64) error`; retain `RefreshKnown` for synchronous callers. Add `catalog.refresh_game` job, version 1 payload `{version,app_id}`, key `<refresh-parent-key>:game:v1:<appid>`. Async existing `catalog.refresh` enumerates the deduplicated known union and schedules per-game children; a displayed/latest-published recommendation match has priority 80, others 50; explicit user import/profile/ranking/AI stages use 100. Global refresh/discovery jobs have `UserID=uuid.Nil`.

- [ ] **Step 1: Write `TestAppBootstrapsSeedWithoutSteamDiscovery`, `TestSyncNudgesDiscoveryOnlyAfterCompletion`, `TestForegroundSyncClaimsBeforeBackfill`, `TestKnownRefreshFansOutWithoutLosingWishlist`.** Assert seed available before manual sync even when discovery source fails; boot twice creates one future tick; final successful user workflow nudges once without awaiting global discovery; failed sync never nudges. Queue contains many backfill10 jobs but user100 and displayed-refresh80 claim first. Slow unrelated metadata item cannot serialize the whole known-catalog loop inside one worker lease. Snapshot failure retains user-visible last success and does not block recommendations from the existing catalog. Cross-user deletion still fences user-owned jobs and never removes global discovery.

  ```go
  if seedCount != 20 || successfulSyncNudges != 1 || failedSyncNudges != 0 { t.Fatal("bootstrap or sync nudge contract failed") }
  if first.Kind != "steam.sync" || second.Kind != "catalog.refresh_game" { t.Fatal("foreground priority was lost during wiring") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/app ./internal/sync ./internal/catalog -run 'TestAppBootstraps|TestSyncNudges|TestForegroundSync|TestKnownRefresh' -count=1 -v`. Expect absent startup/nudge/per-game fan-out.
- [ ] **Step 3: Implement wiring.** Construct transport with Store Service URL, bootstrap/cleanup shared projections, configure recommendation horizon, create discovery service, attach AI planner and logger, register all handlers, and ensure future tick before worker Run. Registration/startup persistence errors fail startup safely; discovery transport failures are deferred worker outcomes, not a requirement for server readiness. Set priority explicitly at each foreground enqueue path, including optional AI descendants. Split async refresh into independently leased game jobs; retain optional unavailable metadata behavior and current sync progress contract. Preserve user deletion ownership on foreground jobs and nil ownership on shared discovery/AI jobs.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/app ./internal/sync ./internal/catalog ./internal/jobs ./internal/profile ./internal/ai ./internal/recommendations ./internal/feedback ./internal/httpapi -count=1`. Expected all existing workflow tests pass; update progress test fixtures only when new child kind requires it, not to weaken success criteria.
- [ ] **Step 5: Commit:** stage named integration files/tests; `git commit -m "feat: start background catalog discovery without blocking user sync"`.

### Task 12: Coalesce catalog-triggered ranking rebuilds and reuse explanation inputs

**Files:** Create `internal/catalog/rebuild.go`, `internal/catalog/rebuild_test.go`; Modify `internal/catalog/discovery_repository.go`, `internal/ai/service.go`, `internal/ai/service_test.go`, `internal/recommendations/service.go`, `internal/recommendations/service_test.go`, `internal/recommendations/retrieval.go`, `internal/recommendations/repository_integration_test.go`, `internal/app/app.go`, `db/queries/catalog_discovery.sql`. Consume migration 00009's cache; do not edit an applied migration.

**Interfaces:** Consume Task 8's `MarkCandidatesChanged` (no user supplied by discovery payload). `(*DiscoveryService).ScheduleRebuilds(ctx context.Context) error` locks per-user state and enqueues existing `recommendations.rebuild` with `UserID` and key `catalog:recommendations.rebuild:v1:<user-id>:<dirty-revision>` at `max(dirty_since+10m,last_scheduled_at+1h)`; priority 50. While `pending_job_id` is nonterminal, coalesce additional changes into that pending job rather than enqueuing a second one; a revision arriving after it has started remains dirty for a later allowed slot. `last_enqueued_at` records enqueue time, `last_scheduled_at` the reserved execution slot. Add separate `ExplanationCache` interface with methods `GetExplanationByInput(ctx context.Context, gameID uuid.UUID, model, inputHash string) (ai.Explanation,bool,error)` and `PutExplanationByInput(ctx context.Context, gameID uuid.UUID, model, inputHash string, value ai.Explanation) error`, implemented by `*recommendations.SQLRepository`; attach through `(*recommendations.Service).SetExplanationCache(cache ExplanationCache, model string)` without changing the ranking `Repository` interface/fakes. Use `catalog_explanation_cache` from Task 2, map source/prompt/schema versions into the input hash, never private profile text.

- [ ] **Step 1: Write `TestCatalogRebuildDebounceAndNoop`, `TestCatalogRebuildCannotResurrectDeletedUser`, `TestRankingReusesGroundedExplanationInput`.** Changes at 12:00 and 12:04 yield one delayed rebuild at 12:10, another change after it cannot enqueue before 13:10, no-op page produces none; dirty change during a running rebuild remains pending for next allowed slot; deleting user atomically removes debounce/user jobs and a racing scheduler cannot recreate them. Two rankings with identical grounded explanation evidence call OpenAI once; changed evidence/model/prompt calls again; publication failure leaves previous published version intact; cached explanations still validate citations and safe presentation.

  ```go
  if rebuildJobs != 1 || !firstAvailableAt.Equal(now.Add(10*time.Minute)) { t.Fatal("ten-minute coalescing failed") }
  if secondAvailableAt.Before(firstAvailableAt.Add(time.Hour)) || noopJobs != 0 { t.Fatal("hourly/noop rebuild guard failed") }
  if identicalInputModelCalls != 1 || changedInputModelCalls != 2 { t.Fatal("grounded explanation cache identity failed") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog ./internal/recommendations -run 'TestCatalogRebuild|TestRankingReusesGroundedExplanationInput' -count=1 -v`. Expect missing scheduler/cache and repeated explanation calls.
- [ ] **Step 3: Implement debounce and cache.** Dirty revision increments only when active candidate membership/canonical ranking evidence changes, including completed feature/embedding work that changes retrieval; recheck shared source activity before marking. Lock existing users `FOR KEY SHARE` and check tombstones before inserting debounce state so deletion races cannot make global qualification recreate personal state or abort after a missing user. Store cache by hash of existing public grounded evidence plus schema/prompt/model, exclude recommendation IDs/timestamps/ranking versions unless they alter evidence. Do not mutate published runs; reuse existing atomic `SaveRanking`/`PublishRanking`. Schedule via global tick/page finalization, preserving hourly throttle through row lock and transactional enqueue. AI completion marks dirty through a narrow callback `SetCatalogChangedHook(func(context.Context, uuid.UUID) error)` on AI Service, game ID only; no import cycle. Wire this hook, cache and scheduler from `internal/app/app.go` in the same task.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/catalog ./internal/ai ./internal/recommendations ./internal/httpapi -count=1`. Expected immutability/deletion/caching tests pass and deterministic explanations remain available when OpenAI is disabled or fails.
- [ ] **Step 5: Commit:** stage explicit debounce/AI/cache files and generated queries; `git commit -m "feat: debounce discovery rankings and reuse grounded explanations"`.

### Task 13: Expose aggregate diagnostics and the stable presentation reader

**Files:** Modify `internal/catalog/discovery_repository.go`, `internal/catalog/discovery.go`, `internal/catalog/discovery_test.go`, `internal/httpapi/data_handlers.go`, `internal/httpapi/health_test.go`, `internal/httpapi/data_service_integration_test.go`, `internal/observability/events.go`, `internal/observability/events_test.go`, `internal/app/app.go`, `web/src/api/types.ts`, `web/src/api/client.ts`, `web/src/api/client.test.ts`, `web/src/pages/DiagnosticsPage.tsx`, `web/src/pages/DiagnosticsPage.test.tsx`.

**Interfaces:** `(*DiscoveryService).DiscoveryStatus(ctx context.Context) (PresentationStatus,error)` for dependent overview; add aggregate `DiscoveryDiagnostics {PresentationStatus; BackfillCursor uint32; LatestRun DiscoveryRunCounts; ActiveBySource map[string]int64; PendingQualifications int64; Reason string}`, where counts fields/JSON are `Discovered/discovered`, `Evaluated/evaluated`, `Admitted/admitted`, `Skipped/skipped`, `Failed/failed`, all int64. Add `Diagnostics(ctx context.Context) (DiscoveryDiagnostics,error)` and `SQLDataService.SetDiscoveryDiagnosticsReader(interface{Diagnostics(context.Context)(catalog.DiscoveryDiagnostics,error)})`; `HealthReport.CatalogDiscovery *catalog.DiscoveryDiagnostics` JSON `catalogDiscovery,omitempty`. Keep `DataService` methods unchanged. Diagnostics frontend normalizes absent aggregate to disabled/unknown display rather than crashing older health fixtures.

- [ ] **Step 1: Write `TestDiscoveryDiagnosticsAreAggregateAndSafe`, `TestDiscoveryLogsOmitSensitiveInputs`, and frontend `DiagnosticsPage` tests.** Feed malicious raw upstream text containing fixture key, SteamID, user UUID, payload/model strings; marshaled API/log output contains none. Assert latest-run arithmetic and distinct source overlap count, persisted cursor/pass timestamp, pending queued/running/waiting/retryable qualifications, and fixed degraded reason. Assert reader timestamps are nullable and `ActiveDiscoveredGames` excludes wishlist-only/inactive discovery games. UI says exactly `Каталог обновляется` for updating and `Каталог может быть неполным` for stale/degraded or unfinished coverage, without claiming complete Steam coverage. Missing discovery fields preserve existing diagnostics.

  ```go
  for _, secret := range []string{fixtureKey, fixtureSteamID, fixtureUserID, fixtureRawBody} {
      if strings.Contains(apiJSON+logOutput, secret) { t.Fatal("sensitive discovery diagnostic escaped") }
  }
  if status.ActiveDiscoveredGames != 1 || pending != 2 { t.Fatal("diagnostic aggregate is not distinct/scoped") }
  ```
- [ ] **Step 2: Run red:** `GOWORK=off go test -race ./internal/catalog ./internal/httpapi ./internal/observability -run 'TestDiscoveryDiagnostics|TestDiscoveryLogs' -count=1 -v`; `pnpm --dir web run test -- src/pages/DiagnosticsPage.test.tsx src/api/client.test.ts`. Expect missing report/event/normalization contracts.
- [ ] **Step 3: Implement reader/report/events and minimal UI.** Aggregate from committed state only, classify failures with allowlisted reasons and derive counts from manifests/observations without returning identifiers or raw errors. Add `Logger.CatalogDiscovery(ctx context.Context,event CatalogDiscoveryEvent)` with correlation/source/mode/availability/counts/duration/safe reason only; expand existing kind/source/stage allowlists for discovery so values are visible but arbitrary strings remain redacted. No payload-derived metric labels. Presentation project separately injects the same `DiscoveryStatusReader` into overview; do not invent a second overview endpoint here.
- [ ] **Step 4: Run green:** repeat Step 2; `GOWORK=off go test -race ./internal/catalog ./internal/httpapi ./internal/observability -count=1`; `pnpm --dir web run test`; `pnpm --dir web run typecheck`; `pnpm --dir web run lint`; `pnpm --dir web run build`. Expected zero failures, additive wire compatibility and safe logs.
- [ ] **Step 5: Commit:** stage explicit backend/frontend diagnostics files; `git commit -m "feat: report catalog discovery coverage and safe aggregate diagnostics"`.

### Task 14: Verify end-to-end fallback, discovery coverage and rollout runbook

**Files:** Modify `cmd/e2e-fakes/main.go`, `web/playwright.config.ts`, `web/tests/e2e/smoke.spec.ts`, `README.md`, `docs/local-demo.md`; add focused scenario file `web/tests/e2e/catalog-discovery.spec.ts` and include it in `Makefile` verify's Playwright command.

**Interfaces:** Test-only fake endpoint `/IStoreService/GetAppList/v1/` returns bounded deterministic pages including AppID 424242 absent from seed/wishlist; optional `/__test/catalog-mode` accepts `healthy|unavailable|malformed` only in the fake executable. Real application accepts no new unauthenticated mutation/test control route. Playwright explicitly sets `STEAM_STORE_SERVICE_BASE_URL` and discovery enable flag under harness; tests remain a single-worker separate E2E database.

- [ ] **Step 1: Write failing `catalog-discovery.spec.ts`.** Authenticate fixture user, complete profile sync, poll aggregate counts/status until discovery qualifies 424242; assert it can be recommended without a wishlist row, calendar buckets preserve precision, and a failed later page leaves recommendations/catalog visible with incomplete copy. The browser fixture's first incremental page admits 424242 and its next page consistently returns unavailable, so the scenario finishes inside the normal browser timeout without accelerating the hourly cadence. Restart service in backend integration coverage (Tasks 9–11), not by shelling out from the browser. Add backend regression `TestDiscoveryDisabledAndUnavailableRetainSeedCandidates` if not already covered, asserting both modes return nonempty seeded/wishlist fallback with no OpenAI. Add smoke fixtures for pagination, changed source revision, DLC/released/software/outside horizon, 429, and dead→retry generation; backend tests own timing-sensitive cases.

  Define local `type HealthFixture={catalogDiscovery:{activeDiscoveredGames:number;status:string}}` and authenticated request helpers `loadHealth(): Promise<HealthFixture>` and `loadRecommendations(): Promise<Recommendation[]>` using the Playwright page's request context and existing endpoints (import `Recommendation` from `../../src/api/types`). Capture published IDs, then poll the deterministic failed-page diagnostic before asserting persistence:

  ```ts
  await expect.poll(async () => (await loadHealth()).catalogDiscovery.activeDiscoveredGames).toBeGreaterThan(0)
  expect((await loadRecommendations()).some((item) => item.steamAppId === 424242)).toBe(true)
  await expect.poll(async () => (await loadHealth()).catalogDiscovery.status).toBe('degraded')
  expect((await loadRecommendations()).map((item) => item.id)).toEqual(beforeFailureIds)
  ```
- [ ] **Step 2: Run red:** `make prepare-e2e-db`; `make playwright-browser`; `pnpm --dir web exec playwright test tests/e2e/catalog-discovery.spec.ts`. Expect missing fake discovery route/scenario outcome. Never run E2E against `DATABASE_URL` from user configuration.
- [ ] **Step 3: Implement fakes/config and runbook.** Make e2e fake contract use proper input_json and deterministic app timestamps; never real network/keys. Update `make verify` to run both `tests/e2e/smoke.spec.ts` and `tests/e2e/catalog-discovery.spec.ts`. Document opt-in flag/defaults/bounds, first recent-watermark plus slow rolling coverage, 200 unique qualifications versus transport retries, safe diagnostics/retry behavior, no completeness promise, seed fallback, disable/re-enable behavior and union-before-cleanup rollout. Retain real-account verification limitations. Update README's claim that reviewed seed is the sole bounded source.
- [ ] **Step 4: Run green and required cross-cutting verification:** `codegraph sync .`; `codegraph affected internal/catalog/discovery.go internal/catalog/discovery_scan.go internal/jobs/queue.go internal/sync/service.go internal/recommendations/retrieval.go internal/httpapi/data_handlers.go -p .`; `make verify`; `git diff --check`. Expected sqlc comparison unchanged after regeneration, Go race/frontend/E2E/build checks zero failures with DB tests executed. If pinned Chromium download fails, confirm an installed executable exists and run `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' make verify`; report that explicit fallback, never silently skip browser verification. Inspect `git diff --stat` and all changed paths; exclude `.env`, build artifacts and `.codegraph` state.
- [ ] **Step 5: Commit:** stage only fake/E2E/Makefile/README/runbook changes; `git commit -m "test: verify discovery fallback and document staged rollout"`.

## Acceptance coverage self-review

| Spec acceptance criterion | Owning tests/tasks |
| --- | --- |
| New upcoming fake AppID becomes discovery catalog entry | Task 8 `TestDiscoveryQualificationCreatesSharedCandidate`; Task 14 browser scenario |
| Modified AppID reevaluated without duplicate game/observation | Tasks 3, 8 revision replay/upsert tests |
| Pagination survives restart | Task 9 `TestDiscoveryPageReplayAfterRestart`; Task 10 paused-scan continuation |
| Failed page preserves cursor/catalog | Task 9 malformed/commit fault cases; Task 14 source outage |
| Mid-scan low AppID modification not lost | Task 9 `TestDiscoveryOverlapCannotSkipLowAppID` |
| One malformed/unavailable item does not block page | Tasks 5, 8, 9 mixed-page item cases |
| Released/DLC/software/outside horizon excluded | Tasks 1, 6, 8 policy/transition tests |
| Exact/month/quarter/year/Coming Soon preserved | Tasks 5–6 blank-date/precision boundary assertions; Task 14 calendar |
| Features/embeddings content/model guarded | Task 8 planner/replay tests; Task 12 enrichment dirty hook |
| Limiter and Retry-After honored | Task 5 shared transport tests, existing HTTP timeout/retry suite |
| User sync claims before backfill | Tasks 7 and 11 priority claims |
| Durable next_retry_at independent of new modification/wrap | Task 10 due retry test with backfill cursor unchanged |
| Dead→due→retried→admitted uses one observation/new queue identity | Tasks 3, 9, 10 generation/upsert/expired-lease cases |
| Aggregate diagnostics avoid identifiers/raw upstream | Task 13 API/log privacy tests |
| Disabled/unavailable discovery preserves seed operation | Tasks 10–11 startup/disabled tests; Task 14 fallback regression |

Additional spec sections: dedicated host/input_json/test override (Task 5); reviewed startup seed and wishlist provenance migration (Tasks 1–4, 11); bounded budgets and coordinator cadence (Tasks 2–3, 9–10); removed released candidates with retained history/deletion fencing (Tasks 4, 8, 12); priority and enqueue identity (Task 7); coalesced atomic ranking/cache behavior (Task 12); Russian coverage copy and dependent overview interface (Task 13). No uncovered acceptance criterion remains in this plan. This is a coverage review, not a claim that implementation/tests already pass.

## Handoff and dependency order

Execute Tasks 1→2→3→4 before any legacy projection cleanup is enabled. Tasks 5–7 establish transport, policy and queue contracts; Task 8 consumes them, Tasks 9–10 establish durable scan coordination, Task 11 starts the coordinator, Task 12 connects ranking changes, and Tasks 13–14 expose/verify the resulting behavior. Keep the flag disabled until migration, union/cleanup tests, replay/retry tests and `make verify` pass.

The presentation implementation may depend on Task 1's stable candidate contract and Task 13's `DiscoveryStatusReader`; it must not infer status by counting the old `origin` column. Its own migrations must be numbered after other reserved projects. Feedback owns 00010; discovery owns only 00009. Coordinate edits to shared recommendation/httpapi/frontend files by completing/merging the relevant task first, preserving the interfaces above.

Plan prepared only; no source changes, runtime DB mutations, commits, pushes or deployments are part of this document-writing task.
