# Steam Search Release-Window Catalog Design

**Date:** 2026-10-18
**Status:** Implemented; final release verification pending
**Related designs:**

- `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`
- `docs/superpowers/specs/2026-10-18-steam-catalog-discovery-design.md`
- `docs/superpowers/specs/2026-10-18-discovery-wishlist-presentation-design.md`

## 1. Decision and objective

Next Game Crush will discover a bounded release catalog from Steam Store search
surfaces, prioritize the useful part of that catalog, and admit games in an
inclusive release window from 30 UTC calendar days before today through 30 UTC
calendar days after today.

The product should quickly contain popular upcoming and newly released games
without spending Store metadata and OpenAI calls on the complete Steam App
List. Released games are useful recommendations because the user can play them
immediately, but a released game with a Steam review summary of `Mixed` or any
negative band is never an active candidate. A newly released game with no
reviews may remain eligible for seven days only when it is currently observed
in Steam's Popular New Releases feed.

Success means:

- a useful popular catalog appears before the long tail finishes scanning;
- exact releases are bounded to `today - 30 days ... today + 30 days`,
  inclusive;
- catalog ingestion is restart-safe, idempotent, rate-limited, and isolated
  from interactive user work;
- concurrent and within-TTL observations never cause duplicate Store work,
  and unchanged semantic content never causes duplicate OpenAI work;
- Steam review percentage and count are preserved and shown on recommendation
  cards;
- popularity and reviews affect catalog admission and ingestion order only;
  the deterministic recommendation score remains the ranking authority.

"Today" and release-window comparisons use UTC calendar dates. The configured
scheduler timezone controls when a scan starts, not which dates are eligible.

## 2. Scope and non-goals

This design changes catalog ingestion and candidate eligibility. It includes:

- Steam Store search as the primary candidate source;
- released as well as upcoming candidates;
- review-based released-game admission;
- durable scheduling, two worker lanes, budgets, deduplication, and refresh
  policy;
- review fields in persistence, Go and TypeScript API contracts, and the UI;
- low-priority `IStoreService/GetAppList` reconciliation as a gap detector.

It does not:

- introduce Steam-tag affinity or another recommendation signal;
- add review score, review count, or Steam popularity to the numeric ranking;
- let OpenAI assign scores, release dates, review bands, or final order;
- promise a complete or real-time Steam catalog;
- expose a multi-user scheduler or run a separate persistent service;
- require host-level cron configuration on the VPS;
- store raw Store search HTML, raw upstream bodies, cookies, or personalized
  storefront state;
- remove the reviewed seed until the replacement has demonstrated stable
  operation.

The existing source union remains: reviewed seed, active Steam discovery
membership, and the allowlisted user's wishlist. Owned, hidden, and directly
disliked/excluded games retain their existing exclusions. Source membership
does not bypass the release or review gates defined here.

## 3. Supersession of the previous discovery design

This document supersedes the following decisions in
`docs/superpowers/specs/2026-10-18-steam-catalog-discovery-design.md`:

| Previous decision | Replacement in this design |
| --- | --- |
| `IStoreService/GetAppList` is the primary discovery source. | Non-personal Steam Store search feeds are the primary candidate source. `GetAppList` becomes low-priority reconciliation. |
| Only unreleased games can enter the shared catalog. | Exact and suitably precise releases from the inclusive `-30/+30` window can enter; released games also pass the review gate. |
| Default horizon is 90 future days. | Default exact-date window is 30 days back and 30 days forward. |
| Incremental scan runs hourly and backfill uses 200 unique qualifications per hour. | A cron-configured daily search scan drives ingestion; Store metadata defaults to 1,000 calls/hour and catalog AI to 100 calls/hour. |
| Backfill walks the complete game-only App List. | Monotonic-validated `recent`/`comingsoon` feeds may stop at the release-window boundary; popular feeds require natural end for authority, and all feeds retain safety caps. Reconciliation may walk App List slowly but cannot delay priority feeds. |
| Unknown/Coming Soon candidates use a 180-day App List modification freshness rule. | Year, Coming Soon, and unknown candidates require current Popular Upcoming or wishlist provenance and use their own refresh policy. |
| One priority-ordered worker pool handles all jobs. | Interactive and catalog workers claim only their own durable queue lane. |

All other compatible guarantees from the previous design remain in force:
source provenance, precision-preserving dates, bounded retries, fencing,
last-known-good state, atomic ranking publication, sanitized diagnostics, and
seed fallback. Existing discovery data is migrated or retained as history; it
must not be discarded merely because its primary source changes.

The MVP design's static 90-day seed ceiling and unreleased-only retrieval are
also superseded. In the discovery/wishlist presentation design, this document
supersedes every statement that limits wishlist, discovery, retrieval, or the
calendar to unreleased games, treats released games as a hard exclusion, or
declares the recommendation horizon unchanged. Its collection separation,
zero wishlist ranking weight, deterministic ordering, stale-source behavior,
and presentation rules remain valid. Eligible released items can appear in its
discovery, wishlist, and calendar collections.

This is also a narrow, explicit replacement for the README's prior rule not to
use Store HTML for catalog discovery. Only the non-personal search-results
adapter described here may parse Store-rendered result markup. Product pages,
reviews pages, personalized sessions, and arbitrary crawling remain excluded.

## 4. Source policy and evidence limits

### 4.1 Primary search feeds

The catalog adapter reads four non-personal Steam Store search surfaces:

1. **Popular Upcoming** — highest-priority upcoming candidates;
2. **Popular New Releases** (`popularnew`) — highest-priority released
   candidates and the only source of the no-review grace exception;
3. **Recent releases** (`recent`), requested with newest-to-oldest release
   ordering;
4. **Coming soon** (`comingsoon`), requested with nearest-to-furthest release
   ordering.

Release-boundary termination is allowed only for `recent` and `comingsoon`,
and only after the adapter has validated monotonic release ordering across all
committed pages in that run. Popular Upcoming and Popular New are popularity-
ordered surfaces: their dates may be out of order, so they must reach their
natural end to become authoritative. A popular feed that hits a safety cap,
fails, or stops early is `partial` and cannot publish Popular-only membership.

The adapter requests a fixed locale and country (`l=english`, `cc=us`), ignores
account preferences where the Store surface supports it, and sends no browser
cookies. It requests game results, but product type is not trusted until Store
metadata confirms `type=game`.

Steam Store search is an unversioned storefront interface, not a documented
Steamworks Web API contract. Some search responses expose a JSON envelope whose
results are rendered HTML. Feed names, pagination fields, result markup, review
attributes, and ordering behavior are therefore observations to pin in adapter
fixtures, not promises made by Valve. A parse or envelope change is an upstream
failure; it must not be interpreted as an empty result set.

The adapter extracts only bounded candidate data needed before `appdetails`:

- Steam AppID and display name;
- displayed release text or date;
- feed membership and result position;
- normalized review summary, positive percentage, and total review count when
  present;
- a normalized search fingerprint excluding volatile review count.

Price, discount, capsule image, platform badges, and Store tags may be present
in the result but are not admission or ranking inputs in this design.

### 4.2 Canonical Store metadata

The existing Store `appdetails` adapter remains canonical for product type,
game identity, `coming_soon` phase, public release text, description, genres,
categories, developers, publishers, artwork, and platforms. Search data may
prioritize or reject obvious cases cheaply, but it cannot publish canonical
game metadata by itself.

Review summaries are the exception: normalized review data comes from the
search observation because this design does not add a separate reviews crawl.
Missing or malformed review markup is `unknown`, never zero reviews. Previous
successful review data is retained on a malformed refresh.

### 4.3 Reconciliation

The documented `IStoreService/GetAppList/v1` integration remains available as
a low-priority, bounded reconciliation source. It detects possible gaps and
changed AppIDs that the search feeds did not expose. It no longer defines the
normal initial backfill and does not bypass the same release, review, TTL, or
budget gates.

Reconciliation runs only after due retries and search-discovered candidates;
it yields when either the Store metadata budget or catalog worker capacity is
needed by higher-priority catalog work. It is best-effort coverage insurance,
not a completeness claim.

Relevant public references:

- Steam documents `IStoreService/GetAppList/v1` and its coarse filters at
  <https://partner.steamgames.com/doc/webapi/IStoreService>.
- Steam documents player-facing exact, month, quarter, year, and Coming Soon
  release precision at
  <https://partner.steamgames.com/doc/store/release_dates>.
- Steam explains that Popular Upcoming is influenced by wishlist volume and
  Popular New Releases by sales and player count in its visibility guidance at
  <https://partner.steamgames.com/doc/marketing/visibility> and
  <https://partner.steamgames.com/doc/store/freetoplay>.
- Steam's official overview of Store review behavior is
  <https://partner.steamgames.com/doc/store/reviews>.
- The observable Store search surface is
  <https://store.steampowered.com/search/>; its request and response shape is
  deliberately treated as unversioned.

The statements about individual search fields and feeds are implementation
observations and inferences from the public storefront. Only the linked
Steamworks documentation is treated as an official contract.

## 5. Release and review eligibility

### 5.1 Exact release phases

`appdetails.release_date.coming_soon` chooses the phase; a displayed date alone
does not. The exact-day rules are inclusive:

- released: `today - 30 days <= release_date <= today` and
  `coming_soon=false`;
- upcoming: `today <= release_date <= today + 30 days` and
  `coming_soon=true`.

An exact date equal to today belongs to the phase reported by `appdetails`, so
it cannot appear in both collections. Exact dates outside the window are
inactive even when the game is wishlisted or appears in a popularity feed.

### 5.2 Imprecise release windows

Release precision is preserved; no synthetic exact day is stored.

- A month or quarter is eligible when its real calendar interval intersects
  the applicable `-30/+30` window and the Store phase agrees.
- A year, Coming Soon, or unknown date is eligible only while the game has
  current Popular Upcoming or current wishlist provenance.
- An imprecise released game still has to pass the released review gate.
- The seven-day unrated grace requires an exact release day. A released
  month/quarter/year entry without reviews is not eligible because its age
  cannot be proven.
- Malformed date text is `unknown`; it never becomes an exact date.

"Current" Popular Upcoming membership means observed in the most recent
successfully completed feed scan. An incomplete or failed scan cannot revoke
membership. Wishlist currency continues to use the existing explicit Steam
source-state semantics and last successful snapshot.

### 5.3 Released review gate

Normalize Steam's displayed review summary into three internal bands:

- `positive`: Overwhelmingly Positive, Very Positive, Positive, or Mostly
  Positive;
- `mixed_or_below`: Mixed or any negative summary;
- `unrated`: an explicitly observed zero-review result.

An unrecognized non-empty summary or internally inconsistent percentage/count
is `unknown`, not `unrated`.

Released-game rules are source-independent:

- `positive` is eligible inside the release window;
- `mixed_or_below` is a hard rejection;
- `unrated` is eligible only when all three conditions hold: exact release is
  no more than seven UTC calendar days old, the game is in the most recently
  completed Popular New Releases scan, and the membership has not since been
  authoritatively removed;
- `unknown` does not create a new candidate and does not erase a prior
  successful review snapshot.

"Hard rejection" means removal from active catalog membership,
recommendations, calendar results, and recommendation API responses. A minimal
ingestion tombstone remains so the next scan does not repeat Store and OpenAI
work. Existing canonical metadata and history are retained for last-known-good
and audit behavior. If a later successful observation moves a game from Mixed
to a positive band, it may reactivate without repeating AI when its semantic
content hash is unchanged.

Upcoming games do not require reviews. Review bands, counts, popularity feed
membership, and no-review grace do not add or subtract recommendation score.

## 6. Prioritization and bounded work

Each search run maintains one durable deduplicated AppID worklist as pages are
committed. The same AppID found in several feeds receives the highest
applicable priority and keeps all provenance:

1. current wishlist games and games in the last published recommendation;
2. Popular Upcoming;
3. Popular New Releases;
4. remaining positive reviewed releases in the 30-day lookback;
5. remaining upcoming games in the 30-day horizon;
6. low-priority App List reconciliation.

Search feeds are cheap discovery inputs. Before spending an `appdetails` call,
the coordinator rejects candidates demonstrably outside the window, released
Mixed-or-below candidates, released unrated candidates without a valid
Popular-New grace, unchanged candidates within TTL, and unchanged exclusion
tombstones.

The ordinary `recent` and `comingsoon` scans may stop after their validated
monotonic release ordering crosses the configured boundary. A regression,
unparseable ordering key, or out-of-order item disables boundary termination
for that feed run; it must continue to natural end or become `partial` at a
safety cap. Every run also has a validated maximum page count and maximum
new-AppID count; reaching a safety cap marks the run `partial` rather than
`complete`. Popular feeds are processed first even when a safety cap prevents
reaching the long tail. These caps are operational guards, not a claim that
Steam honors an exact page-size contract.

## 7. Scheduling and process architecture

### 7.1 Embedded scheduler

The Go application owns the schedule. The production VPS needs only the normal
container and environment configuration; no host crontab, CI runner, or
external scheduler is required.

Initial configuration contract:

```text
CATALOG_DISCOVERY_ENABLED=1
CATALOG_REFRESH_CRON="0 3 * * *"
CATALOG_REFRESH_TIMEZONE="Europe/Moscow"
CATALOG_LOOKBACK_DAYS=30
CATALOG_HORIZON_DAYS=30
CATALOG_APPDETAILS_HOURLY_BUDGET=1000
CATALOG_AI_HOURLY_BUDGET=100
CATALOG_WORKER_CONCURRENCY=4
CATALOG_SEARCH_PAGE_SIZE=100
CATALOG_SEARCH_MAX_PAGES_PER_FEED=100
CATALOG_SEARCH_MAX_NEW_APPS_PER_RUN=20000
CATALOG_RECONCILIATION_CRON="0 4 * * 0"
```

The same values are wired through `.env.example` and `docker-compose.yml` with
bounded validation. Invalid cron syntax, timezone, window, budget, or
concurrency fails startup without printing the configuration. The old hourly
interval and combined discovery-budget settings are retired after a compatible
transition.

For each cron occurrence the scheduler enqueues a durable root job with a key
derived from the UTC occurrence instant. Competing application instances
therefore produce one run. On startup, if the most recent expected occurrence
has no successful or active run, the scheduler enqueues one catch-up occurrence;
it never replays a series of missed days.

Occurrence identity and queue-attempt identity are separate. The run row is
unique by schedule and occurrence instant. Its first queue key ends in
`retry:0`; after a terminal job and its durable retry time, a coordinator takes
a row lock, increments the generation, and enqueues `retry:<generation>`. It
never rearms or mutates the historical dead queue row. Thus a globally unique
queue idempotency key cannot strand a failed occurrence, while competing app
instances cannot create two next generations.

A fresh installation also enqueues one idempotent initial-backfill run without
waiting for the next cron occurrence. "Fresh" means no successful or active
Steam Search discovery run exists and no active search-derived discovery
membership exists; seed and wishlist rows do not make this condition false.

The scheduler only enqueues. It never performs search, Store, database, or AI
work inline.

The weekly reconciliation occurrence uses the same scheduler, database slot
identity, lane, and catch-up rules as the daily scan. It does not require a
second host cron entry.

### 7.2 Queue lanes

Keep one PostgreSQL-backed queue and add an explicit, validated job lane:

- `interactive`: user synchronization, profiles, feedback, ranking,
  notifications, and other user-visible work;
- `catalog`: search scans, candidate qualification, Store refresh,
  reconciliation, and catalog-triggered AI enrichment.

Queue claims include the worker's allowed lane in the transactional
`FOR UPDATE SKIP LOCKED` selection. A worker cannot claim a job for which it has
no registered role. Child jobs declare or safely inherit a lane; a catalog
workflow cannot silently enqueue expensive work into the interactive lane.
The debounced per-user recommendation rebuild is the deliberate exception: the
catalog coordinator marks users dirty and enqueues the existing interactive
rebuild workflow so publication uses the normal path.

The application runs two `jobs.Worker` instances in the same binary and
container. The interactive worker retains its normal concurrency. The catalog
worker defaults to concurrency 4. Either fatal worker error causes coordinated
application shutdown so the supervisor can restart a coherent process.

Priorities continue to order work within each lane. Lane isolation, rather
than priority alone, prevents a large catalog import from occupying every
worker slot.

### 7.3 Manual command

The same binary exposes:

```text
next-game-crush catalog refresh
```

The command validates configuration and enqueues the same durable root job
with a manual-run idempotency key. It does not scan, bypass budgets, write
catalog rows directly, or require a second long-running container.

## 8. Durable workflow

A catalog run is divided into short restart-safe jobs:

```text
cron/manual occurrence
  -> popular feeds
  -> bounded recent and coming-soon pages
  -> deduplicated candidate reservations
  -> appdetails qualification
  -> catalog persistence
  -> content-hash-gated AI enrichment
  -> debounced interactive recommendation rebuild
```

Each search feed has a durable cursor and run status. A page job atomically
persists its normalized observations, next cursor, counts, and worklist changes
before it completes. A crash can replay the page.

Popular feeds run first. Their page observations remain provisional until that
individual feed reaches its natural end without exhausting a safety cap. Each
committed popular page may immediately enqueue candidates
that qualify without popularity: exact/month/quarter upcoming entries and
positive reviewed releases. Feed completion atomically makes membership
authoritative and then enqueues candidates that depend on the Popular Upcoming
or Popular New exceptions. Year/TBA and unrated-grace candidates are therefore
never admitted from a half-read or safety-capped feed, while independently
eligible popular results are not stranded. The ordinary `recent` and
`comingsoon` feeds do not grant popularity exceptions; each committed page may
also enqueue explicitly dated/reviewed candidates immediately. This allows
popular work to qualify before the ordinary long tail finishes without
treating an incomplete popular feed as complete.

A run is `complete` only after both popular feeds reached their natural ends,
each ordinary feed reached its natural end or a validated monotonic release
boundary, and all page manifests were persisted. Safety-cap exhaustion is
`partial`; a parse, transport, or persistence error is
`failed` or `degraded` under the retry policy below.

### 8.1 Monotonic revision fencing

Runs may overlap: a daily run can coexist with a manual run, startup catch-up,
or an old retry. The design does not serialize them. PostgreSQL allocates a
strictly increasing `run_revision` when the logical run row is created. Every
retry generation for that logical run retains the original revision; it does
not become newer merely because it finished later.

Each committed page also receives a transactionally allocated observation
ordinal. The tuple `(run_revision, observation_ordinal)` is the monotonic
revision of every per-AppID search observation. Current review, search
fingerprint, eligibility, and candidate projections update only when that tuple
is greater than the tuple already published for the AppID. Historical rows may
still record a stale completion, but it cannot change current state or enqueue
work from superseded inputs.

Each feed's authoritative membership projection stores
`published_run_revision`. Feed completion publishes membership, absence, and
omission counters only with a compare-and-set requiring
`run_revision > published_run_revision`. The publication and its per-AppID
membership/omission changes are one transaction. Therefore a retry from an
older run finishing after a newer successful run cannot restore old
membership, replace reviews, or increment omission counts. Downstream
`appdetails` persistence rechecks the candidate revision under the same fence
before changing current catalog eligibility; optional stale results may remain
as history only.

At least two consecutive complete scans that both omit a prior ordinary feed
membership are required before absence deactivates that membership. Popular
membership used for admission is stricter: only the most recent complete
popular-feed scan grants the Popular Upcoming or Popular New exception. Failed,
partial, and missing scans never masquerade as authoritative empty scans.

## 9. Persistence and idempotency

Use the next available numbered migration; do not rewrite migration 00009 or
any other applied migration. The implementation may evolve the existing
discovery tables, but the resulting model must represent these concepts:

### Search runs and feed progress

- unique scheduled/manual run identity and configuration snapshot;
- strictly increasing logical `run_revision`, retained by all retry
  generations;
- retry generation and next retry time for a terminal occurrence;
- per-feed status, cursor, first/last page, and aggregate counts;
- per-feed `published_run_revision` fencing authoritative membership and
  omission counters;
- `complete`, `partial`, `failed`, and successful timestamps;
- monotonic-order validation, release boundary, and safety-cap reason without
  raw response content.

### Per-AppID ingestion state

- Steam AppID primary identity;
- feed provenance, first/last seen time, latest complete scan, and priority;
- normalized search release text/precision and search fingerprint;
- normalized review band, displayed label, nullable positive percentage,
  total count, and successful observation time;
- latest `(run_revision, observation_ordinal)` used by current-state
  compare-and-set writes;
- last successful `appdetails` time and `next_refresh_at`;
- last canonical semantic content hash;
- active state, exclusion tombstone reason, and retry state.

### Catalog and review read model

- active shared source membership remains separate from wishlist membership;
- normalized review data is addressable by Steam AppID even when the candidate
  is currently rejected, so a tombstone does not require an active game row;
- accepted games expose the latest successful review summary through the
  recommendation read model;
- existing release observations and canonical game snapshots remain append-safe
  or last-known-good as they are today.

Required exclusion reasons include at least `outside_window`,
`mixed_or_below`, `unrated_grace_expired`, `unrated_not_popular_new`,
`review_unknown`, `not_game`, `unavailable`, and `malformed`. Reasons exposed in
diagnostics are allowlisted and aggregate.

### Idempotency gates

- Search pagination is unique by run, feed, and page/cursor generation.
- An AppID appears once in a run worklist even when several feeds return it.
- One database reservation owns `appdetails` qualification for an AppID at a
  time across scheduled scans, manual refresh, reconciliation, and user sync.
- An unchanged candidate before `next_refresh_at` performs no Store request.
- Review-count-only changes update review state but do not request
  `appdetails` or AI.
- A canonical semantic content hash excludes price, review data, release date,
  availability, observed timestamps, and other non-semantic presentation
  fields.
- AI job identity includes semantic hash plus feature, prompt, and model
  versions. The same identity is reusable after reactivation.
- Model or prompt version changes may legitimately enqueue new AI work without
  pretending the Steam content changed.

## 10. Refresh policy and budgets

Default catalog limits are independent:

- `appdetails`: 1,000 unique request reservations per UTC hour;
- catalog-originated AI enrichments: 100 unique identities per UTC hour;
- catalog worker concurrency: 4;
- search page size: 100 requested results;
- search pages: at most 100 per feed per run;
- newly observed AppIDs: at most 20,000 per run;
- interactive Steam/OpenAI work: not charged to the catalog budget.

Budgets are persisted in PostgreSQL so restarts and multiple app instances do
not reset them. Transport retries for the same reservation do not spend a new
unique unit. Budget exhaustion defers the job to the next eligible window; it
does not dead-letter or drop the candidate.

Default Store metadata TTL:

- exact release within three days before or after today: 24 hours;
- other exact/month/quarter candidates inside the window: 72 hours;
- year, Coming Soon, or unknown Popular Upcoming/wishlist candidates: seven
  days;
- `mixed_or_below`, unrated, outside-window, and `not_game` unchanged
  tombstones: 30 days for `appdetails`, while each search scan may still update
  review/eligibility state without a metadata request;
- new unavailable, malformed, or review-unknown states: retry generations as
  below, then 24 hours after the first exhausted generation and seven days
  after a second unchanged exhausted generation;
- every tombstone wakes immediately on a material search
  fingerprint/eligibility change even when its TTL has not expired.

Search scans may refresh review count and percentage daily without a separate
per-game request. A changed phase, displayed release value, product identity,
or expired TTL can request `appdetails`; a changed review count alone cannot.

The HTTP adapter continues to honor `Retry-After`. Repeated `429`, timeout, or
`5xx` responses reduce catalog concurrency and request rate for the affected
upstream host. Recovery increases rate gradually up to the configured caps.
Adaptive backoff is shared by catalog workers so four lanes cannot each ignore
the others' throttling signal.

Every search, Store qualification, and catalog AI job has at most three normal
queue retries after its initial attempt. `Retry-After` controls the next retry
when valid and is bounded to 24 hours; otherwise retry delays are one minute,
five minutes, then
30 minutes with bounded jitter. Exhausted search page/envelope or page-
persistence work records a safe terminal observation and cannot advance that
feed cursor. A page cursor already committed before downstream work is not
rolled back: exhausted per-AppID Store qualification records an independent
retry state, and optional AI exhaustion leaves the admitted candidate active.
Occurrence and per-AppID watchdogs schedule a new retry generation after 1, 2,
4, 8, 16, and 24 hours, capped at 24 hours thereafter. Optional AI resumes
under its content identity and budget instead of blocking the source scan. A
successful generation clears its retry time. All retry identities, generation
changes, cursors, and observations are transactionally fenced.

Deck compatibility is not part of bulk qualification. It is requested later
only for candidates selected for published recommendations, through the
existing last-known-good and content-gated path.

## 11. AI and deterministic ranking boundary

OpenAI remains optional. After a game passes qualification, the catalog may
schedule the existing structured feature and embedding work, subject to the
catalog AI budget. OpenAI failure or budget exhaustion cannot deactivate a
valid catalog candidate or erase existing features and embeddings.

This design adds no recommendation mechanism. In particular:

- search position and popularity-feed membership do not become score factors;
- review label, percentage, and count do not become score factors;
- recommendation weights and factor semantics remain unchanged;
- the numeric score and published order remain deterministic and versioned;
- existing AI extraction, embeddings, summaries, and grounded explanations
  retain their current advisory roles.

Retrieval and eligibility must change to allow qualified released games and
the 30-day lookback. That change does not authorize new weights or an LLM
ranking step.

## 12. Rebuild coalescing

Catalog membership, semantic content, or review-gate transitions mark the
allowlisted user's candidate set dirty. Review count, percentage, label, or
observation-time changes that leave the eligibility band unchanged update the
review read model but never rebuild the numeric ranking. A ten-minute durable
debounce coalesces a batch, with at most one catalog-triggered rebuild per user
per hour. A later dirty revision always schedules a successor after the cap.

Dirty changes are coalesced into one recommendation rebuild after the current
batch. The existing durable revision/fencing pattern ensures:

- no-op batches enqueue nothing;
- concurrent batches cannot lose a dirty revision;
- at most one pending wakeup exists for the user;
- wakeup identity contains the user, dirty revision, and retry generation;
- ranking publication remains atomic;
- a failed rebuild leaves the last published ranking visible and schedules a
  bounded retry.

The overview may read a fresher review summary for an already published game
without changing that ranking version because reviews are presentation data,
not factor snapshots.

A single server-side current-eligibility predicate applies every clock- or
review-driven hard rule using the current UTC date: release-window exit,
day-eight unrated-grace expiry, authoritative product/phase invalidation, and
`mixed_or_below`. Every recommendation delivery path must use that predicate:

- `GET /api/recommendations/overview`;
- `GET /api/calendar`;
- `GET /api/recommendations/{id}`, which returns not found for an ineligible
  published row;
- digest input selection used by `POST /api/digests/test` and scheduled digest
  composition.

The gate suppresses an expired or rejected previously published row
immediately, even while discovery is disabled or unavailable. No endpoint or
digest repository may bypass it by reading published recommendation rows
directly. The debounced rebuild removes the row from the next immutable
recommendation run; stored historical ranking rows are not rewritten. The
scheduler also marks the candidate set dirty when a persisted
`next_eligibility_change_at` becomes due, so read-time filtering does not
become a permanent substitute for a new published ranking.

## 13. API and UI contract

Extend the Go `recommendations.Recommendation` contract and
`web/src/api/types.ts` together with an optional review summary:

```json
{
  "reviews": {
    "band": "positive",
    "label": "Very Positive",
    "positivePercent": 91,
    "total": 1284,
    "observedAt": "2026-10-18T03:14:15Z"
  }
}
```

Contract rules:

- `band` is `positive`, `mixed_or_below`, or `unrated` for a successfully
  normalized observation;
- `positivePercent` is nullable for unrated results and otherwise an integer
  from 0 through 100;
- `total` is a non-negative integer;
- the whole `reviews` object is absent when no successful normalized review
  observation exists;
- raw HTML and localized tooltip fragments are never returned;
- mixed-or-below released games never appear merely to show their rating.

Recommendation cards and calendar detail show localized deterministic copy,
for example `91% положительных · 1 284 отзыва`. A valid unrated Popular-New
grace candidate shows `Пока нет отзывов`. Upcoming or unknown review data may
omit the line. The UI does not imply that review score contributed to the
personal recommendation score.

Update Go handlers/read models, TypeScript types, normalization, test fixtures,
card components, calendar detail, and API contract tests in the same change.
Existing clients remain compatible because `reviews` is additive and optional.

## 14. Failure and last-known-good behavior

- A failed or partial search feed never deactivates games by absence.
- A malformed search review fragment never becomes zero reviews or Mixed.
- A failed `appdetails` call retains the previous canonical snapshot,
  eligibility, release history, and next bounded retry.
- A successful authoritative phase/date change outside the window deactivates
  the source immediately.
- A successful Mixed-or-below observation deactivates a released game
  immediately and records a tombstone.
- A positive recovery may reactivate a still-in-window game without duplicate
  AI work when semantic content is unchanged.
- Two complete omissions are required for ordinary feed cleanup; source
  failure alone deletes nothing.
- Disabling discovery stops new scans and catalog-originated work but preserves
  the last valid catalog, seed, review data, and published recommendations;
  current read-time eligibility still hides entries whose saved dates or
  review grace have expired.
- Search adapter breakage degrades diagnostics and leaves low-priority App List,
  seed, and wishlist paths available; it never triggers an unbounded emergency
  crawl.

## 15. Security, privacy, and upstream hygiene

- Steam Web API keys and OpenAI keys remain server-side.
- Search requests use no user cookie, Steam session, or personalized browser
  state.
- Test Store/search URL overrides remain fail-closed behind
  `TEST_HARNESS_MODE=1`.
- Request URLs containing keys, raw upstream bodies, raw search HTML, AppIDs,
  SteamIDs, job payloads, prompts, and personal feedback do not appear in logs
  or diagnostics.
- Stored review data is aggregate public Store data. Raw HTML is discarded
  after bounded parsing.
- HTTP response sizes, redirect hosts, content types, pagination progress, and
  parsed field lengths are validated.
- Existing single-user allowlist, authenticated API, CSRF, signed-action,
  deletion-fencing, and SSRF-safe URL rules remain unchanged.
- User deletion removes personal wishlist/profile/recommendation data while
  shared catalog/review/tombstone state remains non-personal.

## 16. Observability

Diagnostics expose aggregate state for each source and worker lane:

- last scheduled occurrence and last complete/partial/failed run;
- per-feed pages, discovered AppIDs, deduplicated candidates, boundary stop,
  and safety-cap state;
- admitted upcoming, admitted released, review-rejected, unrated-grace,
  outside-window, unavailable, and failed counts;
- Store and AI budget used/remaining, deferred count, current adaptive
  concurrency, and next eligible time;
- active candidates by source and release phase;
- current metadata/AI cache entry counts and per-feed deduplicated candidate
  counts (the system does not persist historical cache-hit events);
- dirty rebuild revision and pending wakeup state;
- last successful App List reconciliation and catalog freshness.

Public status remains honest: `ready`, `backfilling`, `stale`, `unavailable`,
or `disabled`. `ready` means the configured bounded feeds completed, not that
the complete Steam catalog was mirrored.

Logs carry workflow/job correlation, lane, safe feed name, stage, duration,
aggregate counts, and allowlisted failure reasons. They contain no candidate
identity or raw upstream content.

## 17. Rollout and compatibility

1. Add new schema and additive API review fields while current discovery stays
   readable.
2. Add the search adapter behind the existing discovery enable flag and
   fixture-test every feed/parser contract.
3. Deploy lane-aware queue claims before enabling bulk search so catalog work
   cannot occupy interactive capacity.
4. Enable embedded daily scheduling with popular feeds first. Preserve the
   seed and prior active discovery rows.
5. Fill the `-30/+30` catalog in priority order, using current semantic hashes
   and feature/model versions to reuse enrichment.
6. Enable ordinary recent/coming-soon tails, then low-priority reconciliation.
7. Remove the obsolete hourly coordinator path and old configuration only
   after the new scheduler has produced complete runs and restart recovery has
   been verified.

Rollback is disable-only once new observations exist: set discovery disabled
and retain the migrated schema and last-known-good catalog. Applied migrations
and public review history are not destructively rolled back.

## 18. Testing and acceptance criteria

### Source and parsing

- Fixture tests cover all four feeds, fixed locale/non-personal requests,
  pagination, boundary stop, duplicate AppIDs, review bands, explicit zero
  reviews, malformed markup, response-size limits, redirects, and
  non-progressing cursors.
- A layout/envelope change fails safely and cannot produce a successful empty
  scan.
- A safety-capped popular feed admits independently eligible dated/reviewed
  games but grants no Popular-only TBA or unrated exception.
- An out-of-order Popular fixture places an in-window result after an
  out-of-window result and proves the adapter does not stop at that date;
  Popular authority requires the natural end.
- `recent` and `comingsoon` fixtures prove boundary termination only after
  monotonic ordering is validated across page boundaries; an ordering
  regression disables the early stop and produces a natural-end or partial
  outcome.
- Store `appdetails` remains authoritative for `type`, phase, and release text.

### Eligibility

- Exact days at `today-30`, `today`, and `today+30` are included with the
  correct Store phase; one day beyond either boundary is excluded.
- Month and quarter intervals intersect correctly without an invented day.
- Year, Coming Soon, and unknown entries require current Popular Upcoming or
  wishlist provenance.
- Every Mixed-or-below released game is absent from active catalog,
  recommendations, calendar, and API results regardless of popularity or
  wishlist membership.
- Overview, calendar, recommendation detail, test digest, and scheduled digest
  contract tests all apply the same hard review eligibility gate; detail
  returns not found and digests omit the rejected row.
- A zero-review exact release in current Popular New is admitted through day
  seven and removed on day eight unless it gains a positive review band.
- Published rows are suppressed on clock-driven window/grace expiry even when
  discovery is disabled or the upstream is unavailable.
- An unrated released game outside Popular New, or an imprecise unrated
  released game, is rejected.
- Review and popularity data never change deterministic recommendation score.

### Scheduling, lanes, and budgets

- Two app instances enqueue one job for the same cron occurrence.
- Startup enqueues at most one missed occurrence and one idempotent empty-catalog
  initial run.
- A dead occurrence creates one fenced next retry generation when due; a
  globally unique dead queue row cannot block catch-up forever.
- Catalog workers cannot claim interactive jobs and interactive workers cannot
  claim catalog jobs.
- A catalog backlog does not prevent an interactive sync, feedback rebuild, or
  notification from acquiring worker capacity.
- Four catalog lanes never exceed the shared 1,000/hour Store or 100/hour AI
  unique-identity defaults.
- `Retry-After`, `429`, timeout, and `5xx` tests prove shared adaptive slowdown
  and gradual recovery.
- The manual command only enqueues and respects all ordinary gates.

### Deduplication and recovery

- One AppID present in every feed receives one Store reservation and retains
  all feed provenance.
- Replay after a crash resumes the page without duplicate game, observation,
  Store, AI, or rebuild work.
- An older run retry finishing after a newer successful run cannot replace
  current feed membership, reviews, eligibility, or omission counters, and
  cannot enqueue work from its superseded observation.
- Review-count-only changes update the card data without `appdetails`, AI, or a
  ranking-version change.
- Unchanged semantic content, reactivation, and model/prompt-version changes
  exercise the intended AI idempotency behavior.
- An expired TTL or material release/phase change refreshes metadata exactly
  once under competing workers.
- Tombstone tests cover 30-day stable rejection TTL, escalating unavailable/
  malformed TTL, and immediate material-fingerprint wakeup.
- Terminal per-AppID Store and optional AI failures retain independent retries
  and do not hold an already committed search-page cursor.
- Failed/partial scans and failed Store/OpenAI calls preserve last-known-good
  state.
- Two complete omissions deactivate ordinary membership; one omission or any
  incomplete run does not.

### Persistence, API, and UI

- A migration upgrade preserves seed, wishlist, discovery history, published
  rankings, queue jobs, and existing game metadata.
- Go/TypeScript contract tests cover rated, unrated, missing, and stale review
  summaries.
- Dashboard and calendar render localized percentage/count copy without
  suggesting it is a ranking factor.
- Diagnostics expose aggregate run, lane, budget, cache, and rejection data
  without identifiers, secrets, payloads, or raw bodies.

### Verification gate

- Focused Go tests run with `GOWORK=off` and `-race`.
- Frontend tests, typecheck, lint, and build pass.
- Cross-cutting queue, persistence, workflow, Docker, and browser behavior pass
  one fresh final `make verify`, using the documented system-Chrome fallback
  only when the pinned Playwright Chromium download is unavailable.
