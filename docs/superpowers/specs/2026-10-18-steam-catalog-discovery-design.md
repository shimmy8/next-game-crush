# Steam Catalog Discovery Design

**Date:** 2026-10-18  
**Status:** Ready for user review  
**Related design:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`

## 1. Objective

Replace the static 20-game discovery ceiling with a continuously growing,
bounded catalog of upcoming Steam games. The service must discover public games
that are added or changed on Steam, enrich them with Store metadata, preserve
release-date precision, and make qualified games available to semantic
retrieval without depending on the user's wishlist.

Success means that a newly added or modified public Steam game is eventually
evaluated and, when it is an unreleased game inside the configured horizon,
appears in the recommendation candidate catalog. The existing reviewed seed
remains an immediate bootstrap and fallback; it is no longer the only source of
non-wishlist candidates.

## 2. Current Baseline

The current catalog is not discovered automatically:

- `catalog_seed.json` contains 20 reviewed AppIDs.
- Wishlist synchronization adds unreleased wishlist games.
- `catalog.RefreshKnown` refreshes metadata only for AppIDs already present in
  `catalog_entries`.
- The local database currently contains 20 seed entries and 22 wishlist-only
  entries.
- All 42 known entries have embeddings, so the limiting factor is catalog
  coverage rather than semantic processing.

This produces a valid ranking over a very small candidate universe, not broad
Steam release discovery.

## 3. Source Policy

Discovery uses Valve's documented `IStoreService/GetAppList/v1` from the Go
backend. The API supports game-only filtering, pagination by `last_appid`,
incremental reads with `if_modified_since`, and a `last_modified` timestamp.
The existing server-side Steam Web API key is reused and never exposed to the
browser.

The design deliberately excludes Steam Store HTML scraping, browser cookies,
personalized storefront sessions, Steam credentials, and third-party catalog
providers. Store metadata lookup remains a separate adapter because the app
list identifies changed AppIDs but does not provide the release precision,
genres, tags, description, artwork, or coming-soon state needed by the product.

Valve documents the app list, but it does not promise an official complete
"upcoming releases" Web API. Therefore catalog coverage is measurable and
eventual, not presented as instantly complete.

References:

- <https://partner.steamgames.com/doc/webapi/IStoreService>
- <https://partner.steamgames.com/doc/store/release_dates>

## 4. Discovery Model

Introduce a catalog discovery boundary independent of metadata enrichment:

```go
type DiscoveryPage struct {
    Apps       []DiscoveredApp
    NextAppID  uint32
    HasMore    bool
}

type DiscoveredApp struct {
    AppID        int64
    Name         string
    LastModified time.Time
}

type DiscoverySource interface {
    ListChangedGames(ctx context.Context, since time.Time, afterAppID uint32, limit int) (DiscoveryPage, error)
}
```

`steam.HTTPClient` implements this interface through
`IStoreService/GetAppList/v1`. This operation uses a dedicated Store Service
base URL, defaulting to `https://partner.steam-api.com`, rather than the current
player Web API base. Its filters, watermark, cursor, and page size are encoded
inside the documented `input_json` query parameter; the API key remains a
separate server-side query parameter. A Store Service URL override is accepted
only when `TEST_HARNESS_MODE=1`, matching the existing fail-closed upstream
override policy.

The catalog module owns checkpoints, admission, and removal policy; the Steam
module owns per-operation host selection, request encoding, HTTP, rate limiting,
response validation, and availability classification.

## 5. Bootstrap and Incremental Discovery

The catalog uses three inputs with explicit provenance:

1. **Reviewed seed:** imported immediately at startup as today.
2. **Wishlist:** unreleased wishlist entries remain mandatory known games.
3. **Steam discovery:** new or modified public games from `GetAppList` are
   evaluated independently of the user.

Two discovery modes share the same queue and checkpoint table:

- **Incremental scan:** captures `scan_started_at` before its first request and
  requests games modified since `watermark_at - 5 minutes`. Every page in the
  scan uses that same lower bound. After the last page is reconciled,
  `watermark_at` advances to the captured `scan_started_at`, never to completion
  time. The overlap is safe because source-revision observations are
  idempotent. This prevents a low AppID modified during a paginated scan from
  falling between two watermarks.
- **Rolling backfill:** walks the complete game-only app list by AppID in
  bounded pages. Its cursor persists across runs and wraps only after a full
  pass. This covers upcoming games that existed before discovery was enabled.

Advancing a cursor and recording a completed watermark happen only after every
AppID in the page has either been evaluated successfully or recorded as a
bounded per-item failure. A crashed page is safe to replay. Failed observations
carry `retry_generation` and `next_retry_at`; due failures are processed before
new backfill items with exponential backoff capped at 24 hours. A later attempt
for the same upstream revision updates that observation transactionally instead
of creating a conflicting second row.

The reviewed seed must not be removed when discovery is unavailable. No failed
or empty response may be interpreted as an empty global catalog.

## 6. Qualification Pipeline

Each discovered AppID is processed independently:

1. Fetch normalized Store metadata with the existing `GameDetails` client.
2. Reject non-game product types, malformed AppIDs, and unavailable store
   pages without mutating previously valid state.
3. Parse the public release string with the existing precision-preserving
   parser.
4. Admit games that are unreleased and either:
   - have a day/month/quarter/year window intersecting the configured horizon;
   - or are explicitly `coming_soon`/unknown and have been modified within the
     configured freshness window.
5. Upsert the game, source snapshot, fetch state, discovery provenance, and
   release observation.
6. Schedule the existing feature extraction and embedding jobs only when the
   canonical game content hash changed or the model version changed.

Default policy:

- recommendation horizon: 90 days;
- discovery coordinator starts with the application and is also nudged by a
  completed user synchronization;
- incremental discovery cadence: at most once per hour;
- metadata evaluation budget: 200 AppIDs per hourly run;
- rolling backfill uses remaining budget after incremental candidates;
- unknown/Coming Soon discovery freshness window: 180 days;
- released games leave the active candidate catalog but retain game metadata
  and release history.

These limits are configuration values with bounded validation, not per-user
settings in the first implementation.

## 7. Persistence

Add a new migration; do not rewrite existing migrations.

### `catalog_discovery_state`

One row per source and scan mode:

- `source` and `mode` (`incremental` or `backfill`), unique together;
- `cursor_app_id`;
- `watermark_at` for the last fully completed incremental scan;
- `scan_started_at` and `scan_completed_at`;
- `availability`, `last_error`, and `updated_at`.

### `catalog_discovery_observations`

One idempotent row per evaluated source revision:

- `source`, `steam_app_id`, and upstream `last_modified`;
- evaluation status (`admitted`, `outside_horizon`, `released`, `not_game`,
  `unavailable`, or `failed`);
- normalized release precision and optional release date;
- `first_observed_at`, `last_attempt_at`, `attempts`, `retry_generation`,
  `next_retry_at`, and bounded safe error classification;
- uniqueness on `(source, steam_app_id, last_modified)`.

Every qualification attempt upserts this single source-revision observation. A
success replaces its `failed` status, clears retry fields and the safe error,
and retains attempt history counters.

Replace the single-valued shared origin with
`catalog_entry_sources(game_id, source, first_seen_at, last_seen_at)`, where
`source` is `seed` or `steam_discovery`. `catalog_entries` remains the active
shared-catalog projection and exists while at least one shared source is active.
The migration backfills existing seed origins into the new source table.

Wishlist membership remains solely in `wishlist_entries`; it is not shared
catalog provenance. Candidate retrieval and catalog refresh use the union of
active shared catalog entries and the current user's unreleased wishlist games.
Existing wishlist-only `catalog_entries` rows are removed only after the union
query is deployed and verified; their `games`, release history, and
`wishlist_entries` rows remain. Adding or removing a wishlist entry therefore
cannot overwrite or delete seed/discovery provenance.

Raw upstream bodies and API keys are never stored in discovery diagnostics.
Existing sanitized game snapshots remain subject to the current storage policy.

## 8. Jobs and Failure Handling

Add versioned jobs:

- `catalog.discovery.tick:v1`;
- `catalog.discover.incremental:v1`;
- `catalog.discover.backfill:v1`;
- `catalog.qualify:v1:<appid>:<last-modified>:retry:<generation>`.

The coordinator enqueues one incremental scan and, when budget remains, one
backfill page. It schedules its next tick with `available_at` rather than
sleeping in a worker. Restarting the application idempotently ensures that one
future tick exists, so backfill progresses without repeated manual syncs.

The scan job pages AppIDs and enqueues independent qualification children with
`required=false`. A qualification job persists normal non-admission outcomes
(`outside_horizon`, `released`, `not_game`, `unavailable`) and succeeds.
Retryable transport/source failures use normal bounded retries. If a child
exhausts retries, the page reconciler writes a safe `failed` observation with
`next_retry_at`; only then may the page cursor advance. Checkpoint persistence
and malformed page-envelope failures fail the scan itself and prevent cursor or
watermark advancement.

The first qualification uses retry generation `0`. When a failed observation
becomes due, the coordinator atomically increments its generation and enqueues
the corresponding new idempotency key. It never rearms or mutates the historical
dead queue row. Concurrent coordinators use a row lock so only one next
generation is created.

Add `priority smallint NOT NULL DEFAULT 50` to `sync_jobs` and `Priority int` to
`jobs.Spec`. Claim ordering becomes `priority DESC, available_at, created_at,
id`. Bounded priorities are:

- user-triggered Steam/profile/recommendation workflow: `100`;
- refresh of already displayed/recommended catalog entries: `80`;
- incremental discovery and due failed-item retry: `50`;
- rolling backfill: `10`.

The idempotency key, not priority, still defines job identity. Re-enqueueing an
existing job may raise its priority but never lower it.

The worker respects the existing Steam limiter. Discovery must not starve
profile synchronization: user-visible library/wishlist calls and refreshes of
already recommended games have higher priority than backfill.

If Valve discovery is unavailable, recommendations continue from the last
successful catalog, seed, and wishlist. The UI reports stale discovery coverage
without replacing the catalog with an empty set.

## 9. Observability and Product Status

Diagnostics expose aggregate, non-personal discovery state:

- last successful incremental scan;
- current backfill cursor and completed-pass timestamp;
- AppIDs discovered, evaluated, admitted, skipped, and failed in the latest
  run;
- active catalog counts by provenance;
- pending qualification count;
- freshness or degraded reason.

The user-facing copy says "Каталог обновляется" or "Каталог может быть
неполным". It must not claim complete Steam coverage while a backfill is in
progress or the source is stale.

Logs include workflow/job correlation and aggregate counts, but no SteamID,
user UUID, API key, raw response, job payload, or full model input/output.

## 10. Compatibility and Rollout

- Existing seed and wishlist catalog rows remain valid.
- Existing recommendation runs remain readable and immutable.
- Discovery can be disabled with a fail-closed server configuration flag; seed
  and wishlist behavior then matches the current implementation.
- The first enabled run imports the seed, performs an incremental scan from a
  conservative recent watermark, then spends its remaining budget on backfill.
- Recommendation rebuilding may run after each discovery batch, but publication
  remains atomic through the existing ranking-version mechanism. Active catalog
  changes are coalesced for 10 minutes and trigger at most one rebuild per hour.
  No-op batches do not rebuild. Existing explanation input hashes are reused;
  OpenAI is called only when the grounded explanation input changed.

## 11. Acceptance Criteria

- A fake `GetAppList` page containing a new upcoming game creates a
  `steam_discovery` catalog entry after qualification.
- A modified existing AppID is reevaluated without creating duplicate games or
  duplicate source-revision observations.
- Pagination resumes from the persisted cursor after restart.
- A failed page does not advance the cursor or erase existing catalog entries.
- A game modified during a multi-page scan is picked up by that scan or the
  overlapped next scan; it cannot be skipped by watermark advancement.
- A single malformed/unavailable AppID does not block other items in its page.
- Released, DLC, software, and out-of-horizon items do not enter active
  recommendation candidates.
- Exact/month/quarter/year/Coming Soon precision survives discovery unchanged.
- Feature and embedding work is content-hash/idempotency guarded.
- Steam rate limits and `Retry-After` are respected.
- User-triggered synchronization jobs claim ahead of queued backfill jobs.
- A failed AppID is retried from its durable `next_retry_at` without waiting for
  a new Steam modification or a complete backfill wrap.
- A source revision that goes `dead → due → retried → admitted` updates one
  observation and uses a distinct queue identity for the retry generation.
- Diagnostics report coverage without exposing identifiers or raw upstream
  data.
- Existing seed-only operation remains functional when discovery is disabled
  or unavailable.

## 12. Explicit Non-Goals

- Claiming a complete real-time mirror of Steam.
- Scraping Store HTML or personalized search pages.
- Crawling reviews, community pages, prices, discounts, or regional packages.
- Supporting multiple stores or third-party game databases.
- Personalizing discovery ingestion per user.
- Removing the reviewed seed before rolling backfill has demonstrated stable
  coverage.

