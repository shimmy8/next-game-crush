# Achievement Completion Signals Design

**Date:** 2026-10-18
**Status:** Approved
**Related MVP design:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`
**Related profile implementation:** `internal/profile/repository.go`,
`internal/profile/vectors.go`
**Related recommendation implementation:** `internal/recommendations/retrieval.go`,
`internal/recommendations/scoring.go`, `internal/recommendations/evidence.go`

## 1. Objective

Recognize that a short finite game can be a strong preference signal even when
its lifetime or recent playtime is much lower than an open-ended game such as
Valheim. Infer completion from Steam achievements without asking the user to
mark games complete, and make a confidently inferred completion at least as
strong as the largest current recent-playtime source in the preference
profile.

Success means:

- completion is inferred automatically from a successful Steam achievement
  snapshot;
- 100% achievement completion counts even when no ending achievement can be
  identified;
- every distinct unlocked non-DLC ending or base-game completion achievement
  counts as an ending marker;
- alternate endings can strengthen the signal, while a meta achievement such
  as "unlock all endings" does not count as another ending;
- the completion contribution is deterministic, persistent, auditable, and
  not recency-decayed;
- explicit user dislike or exclusion overrides the inferred positive
  contribution without rewriting the historical completion fact;
- OpenAI may classify achievement semantics, but cannot assign completion
  weights, calculate profile vectors, score candidates, or order
  recommendations;
- Steam or OpenAI failure preserves the last successful state and never turns
  unavailable data into a successful empty achievement set; and
- profile-summary evidence cannot omit Steam two-week activity merely because
  wishlist or high-lifetime-playtime rows consumed an early SQL limit.

## 2. Scope and Non-Goals

### In scope

- Parse achievement identity and localized metadata from the existing
  `ISteamUserStats/GetPlayerAchievements/v1` request.
- Use `ISteamUserStats/GetSchemaForGame/v2` only as a fallback when the current
  player-achievement response lacks usable names or descriptions.
- Persist normalized achievement definitions, per-user unlock observations,
  semantic classifications, historical completion evidence, and a current
  deterministic projection.
- Add completion to profile construction, candidate affinities, persisted
  recommendation factors, grounded explanations, and profile-summary
  evidence.
- Preserve existing source availability, deletion fencing, privacy, and
  single-user constraints.

### Out of scope

- Manual completion controls, completion correction buttons, or a new library
  management UI.
- A configurable recency-period selector or new period UI. This version uses
  Steam's existing `playtime_2weeks`, persisted as
  `recently_played_minutes`.
- Scraping guides, community wikis, Steam reviews, playthrough-length sites,
  or non-Steam achievement providers.
- Using playtime thresholds to guess that a game was completed.
- Treating every high achievement ratio as completion.
- Asking OpenAI to decide ranking weights, recommendation scores, candidate
  eligibility, or ordering.
- A full Steam catalog achievement crawl. Metadata is fetched only for games
  already selected by the existing bounded achievement synchronization flow.
- Global achievement rarity as proof of completion. A future feature may use
  rarity as separate context, but it is not part of this inference.
- Calling an ending "the canonical ending" or claiming story completion when
  the saved evidence only establishes that one ending was reached.

## 3. Current Behavior and Constraints

The current Steam client already calls `GetPlayerAchievements/v1` with
`l=en` in `internal/steam/http_client.go`, but its decoder keeps only
`achieved` and `unlocktime`. The fixture in
`internal/steam/testdata/achievements.json` also contains `apiname`, which is
currently discarded. `steam.AchievementSummary` retains only aggregate counts
and the latest unlock time.

Synchronization serializes that aggregate Go value into
`achievement_summaries.raw_snapshot`; it is not the original Steam response.
The database therefore cannot recover achievement names, descriptions, API
names, or individual unlock times from existing rows.

The current profile algorithm in `internal/profile/vectors.go` gives:

- recency-adjusted lifetime playtime a maximum source weight of `1.0`;
- recent playtime a maximum source weight of `1.5`; and
- achievement ratio a maximum source weight of `0.5`, also recency-adjusted.

Candidate affinity in `internal/recommendations/retrieval.go` similarly uses an
aggregate achievement ratio and recency decay. This undervalues short finite
games and cannot distinguish finishing a story from collecting optional
achievements.

`internal/ai/repository.go` currently applies `LIMIT 100` after sorting first
by wishlist membership and positive feedback, then last-played time and
lifetime playtime. `recently_played_minutes` is selected but not part of that
ordering. The later Go sort repeats the same omission. A profile prompt can
therefore receive no useful recent rows and correctly report that all supplied
recent values are zero even when current activity exists outside the clipped
set.

All changes must preserve these repository-wide rules:

- ranking remains deterministic;
- optional upstream failure preserves the last successful derived state;
- unavailable, private, malformed, failed, and not-modified responses remain
  explicit;
- PostgreSQL remains the only persistent service and queue;
- browser mutations remain authenticated and CSRF-protected;
- deletion fencing prevents an in-flight user job from recreating deleted
  data; and
- Go handlers, persisted JSON, frontend types and normalization, and contract
  tests change together.

## 4. Product Semantics

### 4.1 Completion evidence

A user-game has completion evidence when at least one of the following is
true:

1. a successful player-achievement snapshot has `total > 0` and
   `achieved == total`; or
2. the user has unlocked at least one distinct achievement whose current
   validated semantic classification is a non-DLC base-game ending or
   base-game completion marker.

The second rule accepts any ending, including alternate or non-ideal endings.
It does not require the classifier to identify a single main or canonical
ending.

The following do not count as ending markers:

- DLC ending or DLC completion achievements;
- chapter, mission, level, act, or partial-progress achievements;
- optional challenge, difficulty, speedrun, New Game Plus, collection, and
  mastery achievements;
- meta achievements for unlocking, viewing, or collecting all endings; and
- unknown or invalid classifications.

A meta achievement such as "unlock all endings" may coexist with the
individual ending achievements, but it contributes zero to `ending_count`.
Every qualifying individual achievement is counted once by stable Steam API
name, even if names or descriptions are duplicated.

No achievements, an unsupported achievement schema, a private response, or a
failed request means unknown evidence, not "not completed."

### 4.2 Persistent historical meaning

Completion is a historical statement: the application observed sufficient
evidence that the user completed or reached an ending of the game. Its profile
weight does not decay with `last_played_at` and does not disappear merely
because Steam later adds DLC or additional achievements, making the current
ratio lower than 100%.

Persistence does not make mistakes immutable. A newer successful source
snapshot, a corrected achievement definition, or a new semantic-classifier
version may supersede a previous current projection. The evidence history and
algorithm version remain auditable. A source failure alone never supersedes a
successful projection.

Profile summaries must distinguish this persistent historical preference from
current interest. An old completion may support "you completed" or "you have
historically shown interest in" wording, but only saved Steam two-week
activity or trustworthy launch timestamps may support
  "currently playing" or "recent interest" wording.

### 4.3 Explicit negative override

Automatic completion evidence never defeats explicit user intent. If the
latest direct action for the same user and game is `dislike` or legacy
`exclude`:

- the historical completion projection remains stored;
- its completion weight contributes zero to the positive profile vector;
- the game cannot be used as a positive completion anchor or positive
  completion evidence; and
- the existing negative feedback behavior remains in force.

A later authoritative `like` removes that current override according to the
existing latest-direct-action ordering. Historical dislikes remain available
to the negative-learning path as already designed. An active mode preference,
such as avoiding co-op, continues to penalize matching recommendation
candidates independently; it does not rewrite whether a source game was
completed.

There is no completion-specific mutation endpoint. The only override comes
from the existing explicit feedback contract.

## 5. Semantic Classification Contract

Achievement semantics are extracted into a strict, versioned structured
output. Each output item references an opaque input evidence key and selects
exactly one class:

- `base_game_ending`: reaching any ending of the base game;
- `base_game_completion`: explicitly finishing the base game, campaign, or
  main story without naming a particular ending;
- `dlc_ending_or_completion`: finishing or reaching an ending of downloadable
  or expansion content;
- `meta_ending_collection`: unlocking, seeing, or collecting multiple/all
  endings rather than reaching another ending itself;
- `partial_progress`: chapter, mission, act, level, or other intermediate
  progression;
- `optional_challenge`: difficulty, speedrun, collection, mastery, New Game
  Plus, side objective, or other optional accomplishment;
- `other`: meaningful semantics outside the preceding classes; or
- `unknown`: insufficient or ambiguous supplied evidence.

Only `base_game_ending` and `base_game_completion` increment `ending_count`.
The classifier receives only normalized game name, achievement API name,
English display name, English description, and opaque evidence keys. It does
not receive SteamID, user identity, unlock state, playtime, feedback, profile,
recommendations, ranking factors, or raw upstream bodies.

Output is accepted only when every requested key appears exactly once, every
class is allowlisted, no unknown key appears, and the response passes the
existing strict structured-output and safe-diagnostic validation patterns in
`internal/ai/openai.go`. The stored classification includes model, prompt
version, JSON-schema version, normalized input content hash, creation time,
and class.

OpenAI confidence numbers are not used. OpenAI supplies the semantic class
only. Go code decides whether the class qualifies, counts distinct unlocked
markers, applies explicit overrides, computes the fixed weight, builds profile
vectors, derives candidate affinities, and orders recommendations.

When OpenAI is disabled or unavailable, previously validated classifications
remain usable. New or changed definitions remain `unknown` until a valid
classification succeeds. The deterministic 100% rule remains available
without OpenAI. This version does not introduce a lexical completion heuristic
whose behavior could silently diverge from the versioned classifier.

## 6. Deterministic Completion Weight

Let `ending_count` be the number of distinct unlocked achievements with a
current qualifying semantic class. Let `ever_100_percent` record whether a
successful Steam snapshot has ever observed `total > 0` and
`achieved == total` for that user and game.

The ending-based weight is:

| `ending_count` | Weight |
|---:|---:|
| 0 | 0.00 |
| 1 | 1.50 |
| 2 | 1.85 |
| 3 | 2.05 |
| 4 or more | `min(2.25, 2.05 + 0.10 × (ending_count - 3))` |

The 100%-based weight is `1.50` when `ever_100_percent` is true and `0.00`
otherwise. The final inferred completion weight is:

```text
max(100_percent_weight, ending_weight)
```

The two bases are never added. Examples:

- 100% and no recognized ending: `1.50`;
- one ending and less than 100%: `1.50`;
- 100% and one ending: `1.50`, not `3.00`;
- 100% and two endings: `1.85`;
- three endings: `2.05`;
- four endings: `2.15`;
- five or more endings: at most `2.25`.

When a completion weight is active, the old aggregate achievement-ratio
component is not added for the same source game. Completion replaces that
component so identical achievement evidence is not counted twice. Independent
playtime, recent activity, and positive feedback components may still add
their own contributions because they represent separate observations.

After the explicit negative override is applied, the effective positive
completion weight is zero for that source game. The stored historical weight
and its basis remain unchanged.

The baseline completion weight of `1.50` equals the current maximum recent
source weight and exceeds the current maximum recency-adjusted lifetime
playtime source weight of `1.0`. Higher weights recognize multiple distinct
endings while the `2.25` cap prevents ending-heavy games from dominating the
profile without bound.

## 7. Data Model

Add a new numbered migration after the repository's current highest migration;
with the present tree this is expected to be `00015`. Do not rewrite an
existing migration. Add SQL in `db/queries/` and regenerate
`internal/store/sqlc/`; never edit generated bindings by hand.

### 7.1 Shared achievement definitions

`game_achievement_definitions` stores Steam metadata that is independent of a
user:

- `game_id`;
- `api_name`;
- nullable normalized English `display_name` and `description`;
- `metadata_source`: `player_achievements` or `game_schema`;
- `definition_content_hash` over normalized classifier input;
- `last_successful_fetch_at`; and
- timestamps needed for deterministic replacement and diagnostics.

The current definition is unique on `(game_id, api_name)`. Content-addressed
classification history prevents changed publisher wording from reusing a
classification for older text. Do not store provider response bodies merely
for convenience.

### 7.2 Per-user achievement observations

`user_achievement_unlocks` stores the latest successful authoritative player
snapshot together with the durable fact that an unlock was observed:

- `user_id`;
- `game_id`;
- `api_name`;
- `present_in_latest` and `achieved_in_latest`;
- `ever_achieved`;
- nullable first observed `unlocked_at`;
- integer `snapshot_generation` for the last successful snapshot in which the
  definition was present;
- `source`; and
- `last_successful_fetch_at`.

The primary key is `(user_id, game_id, api_name)`. A successful response is
applied transactionally for one user-game: validate the complete response,
mark prior rows absent from the new current generation, upsert current rows,
preserve `ever_achieved` monotonically, update the aggregate summary from only
the current generation, and update source state. Unavailable, failed,
malformed, or not-modified responses do not change the current generation.

Keeping absent and locked historical rows allows later semantic classification
to recognize a previously observed unlock and prevents an achievement reset or
publisher schema removal from erasing a historical completion signal. Only
`ever_achieved = true` rows may become ending evidence. Current aggregate
counts never include rows absent from the latest successful snapshot.

### 7.3 Semantic classification history

`achievement_semantic_classification_generations` stores the per-game
activation barrier: game, configured model/prompt/schema identity, expected
definition-set hash, status, and activation time. At most one generation for a
given configured identity is active for a game.

`achievement_semantic_classifications` stores validated immutable classifier
results:

- `game_id` and `api_name`;
- `definition_content_hash`;
- semantic `class`;
- classification generation ID;
- `model`;
- `prompt_version` and `schema_version`;
- `input_content_hash`; and
- `created_at`.

A uniqueness key over the definition hash and classifier identity makes jobs
idempotent. A per-game classification generation records the complete expected
set of current classifiable definition hashes. A generation becomes active
only after every expected key has one validated result; chunked provider calls
may stage results but cannot expose a partial generation. A deterministic
current-classification query selects only the active configured
prompt/schema/model generation for the current definition. A stale
classification is retained as history but cannot silently classify changed
text.

### 7.4 Historical evidence and current projection

`user_game_completion_evidence` records normalized immutable evidence rows:

- `id`, `user_id`, and `game_id`;
- `kind`: `hundred_percent` or `ending_marker`;
- nullable `api_name`, definition content hash, and semantic-classification
  identity for an ending marker;
- nullable unlock time;
- `observed_at`; and
- evidence/projection algorithm version.

A check constraint requires achievement/classification fields for
`ending_marker` and forbids them for `hundred_percent`. Uniqueness over the
user-game evidence identity makes replay idempotent. Historical rows are not
used blindly: the current projection accepts ending-marker evidence only when
its definition has a valid current configured classification. The history
therefore remains auditable when a later classifier version corrects a class.

`user_game_completion_projections` provides the current materialized result:

- `user_id` and `game_id` primary key;
- `ending_count`;
- `hundred_percent_basis`;
- `weight`;
- nullable `completion_at`;
- `basis`: `hundred_percent`, `ending`, or `both`;
- `algorithm_version`; and
- `updated_at`.

A projection row exists only when its deterministic weight is positive.
Absence means "completion not inferred from available saved evidence," not a
claim that the user did not complete the game. Source-state availability
continues to distinguish a successful non-qualifying snapshot from unknown or
unavailable Steam data.

`completion_at` is the earliest known qualifying ending unlock time. When the
only basis is 100%, use the last unlock time from the successful 100% snapshot
when known; otherwise leave it null rather than inventing a date. This date is
for historical wording and auditing, not decay.

The projection is derived entirely in Go from saved evidence and current
validated classifications. Database constraints enforce non-negative counts,
the weight range `0..2.25`, allowlisted bases/classes, and referential
integrity. The persisted historical projection does not embed the current
feedback override; profile loading computes `effective_completion_weight`
from projection plus the latest direct feedback so a later `like` can restore
the automatic positive signal without reconstructing Steam evidence.

## 8. Steam Acquisition and Synchronization Flow

### 8.1 Primary path: current player-achievement request

Extend `steam.Achievements` and its response types to retain, for every item:

- `apiname`;
- `achieved`;
- `unlocktime`;
- localized English `name`; and
- localized English `description`.

Continue sending `l=en`. Validate positive AppID, non-empty game name,
non-empty and unique API names, achieved values, non-negative timestamps, and
aggregate consistency before persistence. A malformed member makes the
response failed; it must not partially replace the prior snapshot.

The existing bounded target selection and maximum achievement-request
concurrency remain unchanged. This feature does not expand synchronization to
every owned game.

### 8.2 Schema fallback

If a successful player-achievement response contains usable unlock state but
one or more definitions lack a usable English name or description, request
`ISteamUserStats/GetSchemaForGame/v2` once for that AppID and `l=en`.

The fallback is:

- app-scoped rather than user-scoped;
- cached with a resource-scoped operation/cache key;
- validated as an explicit `Available`, `Unavailable`, `RequestFailed`, or
  `NotModified` source;
- used only to fill definition metadata, never to infer a user's unlocks; and
- observable only through aggregate, sanitized sync diagnostics.

If fallback succeeds, merge definitions by exact API name. It may supply
metadata for locked achievements as well. If it fails, retain any prior
successful definitions and persist the valid player unlock snapshot without
inventing semantics. Missing definitions remain unclassified and can still
participate in the deterministic 100% rule.

Player unlock persistence and shared schema metadata have independent success
boundaries. A schema failure must not discard a valid player snapshot, and a
player failure must not convert a valid shared schema into user completion.

### 8.3 Classification and projection jobs

After a committed definition changes its content hash, enqueue an idempotent
achievement-semantic classification generation for that game. The job may
split a bounded definition set across provider calls, but it activates the
generation only after the complete expected key set has validated output.

After either a successful player snapshot or a newly published relevant
classification, enqueue/rearm a user completion projection and the existing
profile/recommendation rebuild chain. Replays are safe: content hashes,
classifier identity, projection algorithm version, and existing queue
idempotency determine whether work is already current.

If publisher metadata changes and the new classification generation is not
active yet, keep the last successful completion projection while work is
pending or failed. Publish a lower replacement only after a complete active
generation makes the change deterministic: for example, a newly valid
classification explicitly supersedes a former ending classification. A
transient or partially staged classification gap is not negative evidence.

Do not hold a Steam synchronization transaction open across an OpenAI call.
Do not block preservation of valid Steam data on optional classification.

## 9. Profile and Recommendation Integration

### 9.1 Profile signal

Extend `profile.Signal` with the saved completion projection and extend
`profile.SignalWeights` with `Completion`. `LoadSignals` joins the current
completion projection and latest direct feedback for the same user-game.

`CalculateSignalWeights` applies the formula in section 6. It does not use
`HistoricalRecencyWeight` for completion. When completion is positive it
replaces, rather than adds to, the signal's achievement-ratio component. A
current dislike/exclude sets the effective positive completion contribution
to zero before the signal is added to the positive vector.

Completion becomes its own profile signal kind for confidence/coverage. Bump
the profile algorithm version and include the completion projection algorithm
version in the profile input/build key so a changed projection cannot reuse a
stale profile.

### 9.2 Candidate affinity and scoring

Add a deterministic `completionAffinity` factor and matching configured weight
to recommendation types and persisted factor JSON. It measures normalized
feature overlap with positively eligible completed source games, using the
same saved configured-model feature rows used by the other affinities.

Completed rows are excluded from the old achievement-ratio affinity
accumulator so the same achievement evidence cannot contribute through both
factors. Non-completed achievement progress continues to use the existing
achievement affinity. Recent and playtime affinities remain independent.

The factor is calculated in Go/SQL from saved local data. OpenAI has no role in
the factor or score. Ties retain the repository's existing deterministic order
by score, normalized name, raw name, and stable ID.

Add `completionAffinity` and its weight to:

- Go `Factors` and `Weights`;
- persisted `recommendation_factors` JSON;
- API response DTOs and safe presentation;
- `web/src/api/types.ts` and response normalization;
- technical-leak checks where internal factor names are forbidden in public
  prose; and
- backend/frontend contract fixtures and tests.

Bump the recommendation algorithm version. Existing stored rankings retain
their snapshot behavior and continue to decode absent completion fields as
zero.

### 9.3 Grounded evidence

Add `completion_anchor` to the private/public allowlisted evidence kinds. It
may name an owned source game only when:

- its current effective completion weight is positive;
- the candidate has material feature overlap or compatible configured-model
  embedding similarity with that source; and
- it is not subject to a current direct negative override.

Copy may state that the user completed or reached an ending in the source
game. It must not expose exact weights, API names, raw achievement text,
source UUIDs, classifier classes, or claim that an old completion is recent.
The existing grounded-evidence selection and deterministic fallback rules
continue to apply.

## 10. Profile-Summary Evidence Selection

Fix `LoadProfileEvidence` as part of this feature because otherwise valid
recent and completion signals can be absent from the summary that explains
the profile.

Remove the semantic `LIMIT 100` from the initial SQL ordering. Load the bounded
single-user relevant set, normalize it, and apply one deterministic selector
in Go. The selector protects evidence in this order:

1. latest explicit positive or negative feedback needed to explain an
   override or avoidance;
2. games with positive Steam two-week `recently_played_minutes`;
3. games with a positive inferred completion projection;
4. wishlist context; and
5. remaining historical playtime and achievement-progress rows.

Within a tier sort by the tier's material value descending, then
`last_played_at` descending with nulls last, normalized game name, raw game
name, and game UUID. Deduplicate by game before applying the evidence cap.
Recent activity is the existing Steam `playtime_2weeks` value persisted as
`recently_played_minutes`; it is not a configurable period and is not inferred
by OpenAI.

Evidence items add allowlisted fields for completion basis, ending count,
completion date when known, and whether the completion signal is currently
overridden. The AI does not receive internal numeric weights. The prompt and
schema version are bumped and explicitly state:

- completion is historical unless separate recent evidence says otherwise;
- short completed games may be strong preferences despite low hours;
- an overridden completion cannot become a positive preference claim; and
- missing dates or activity do not support recency claims.

The selector must retain at least one positive two-week activity row whenever
one exists and the cap is non-zero. Completion rows must be represented before
unprotected wishlist/history rows. Tests with more than 100 relevant games pin
these guarantees.

## 11. Failure Handling and Last-Known-Good State

### Steam player achievements

- `Available`: atomically advance the current user-game achievement snapshot,
  update aggregate summary and source state, preserve historical unlock facts,
  then project from saved evidence.
- `NotModified`: keep all rows and treat the last successful snapshot as
  current for orchestration.
- `Unavailable`, private, malformed, or `RequestFailed`: update only sanitized
  attempt/source diagnostics; preserve unlocks, summary, evidence, and
  projection.

### Steam schema fallback

- Success updates only validated shared definitions.
- Not-modified reuses the last successful definitions.
- Unavailable or failed fallback preserves definitions and leaves new missing
  metadata unclassified.
- A schema result never creates or deletes a user's unlock evidence.

### OpenAI classification

- Valid output appends an immutable classification and may trigger projection
  rebuilds.
- Disabled, timeout, rate limit, malformed output, unknown keys, missing keys,
  or invalid enums publish nothing and preserve the last valid classification
  for the same current definition hash.
- Provider errors are reduced to the existing allowlisted safe diagnostic
  categories. Raw prompts, bodies, API keys, and provider error text are not
  logged or stored.

### Projection and rebuild

- Projection runs only from committed local evidence and classifications.
- A failed projection or downstream profile/recommendation rebuild leaves the
  last published projection/profile/ranking available and retryable.
- Projection publication and its enqueue/rearm action follow existing
  transaction and idempotency patterns so crashes can replay safely.
- Unknown evidence never clears `ever_100_percent` or a last-known-good
  historical fact.

## 12. Migration and Backfill

The migration creates the new tables, indexes, foreign keys, enums/checks, and
cascade behavior without changing already applied migrations.

Backfill is local and deterministic:

1. For every existing `achievement_summaries` row with `total > 0` and
   `achieved == total`, create historical 100% completion evidence and a
   projection with weight `1.50`.
2. Use `last_successful_fetch_at` as the observation timestamp. Leave
   `completion_at` null unless a trustworthy saved timestamp is already
   present; do not manufacture an unlock date.
3. Do not create per-achievement unlock rows or semantic classifications from
   aggregate data.
4. Rows below 100% remain unchanged and unknown with respect to ending
   semantics until the next successful detailed synchronization.
5. Do not make Steam or OpenAI calls from the migration.

After deployment, normal synchronization populates detailed observations and
queues classification. A versioned profile/recommendation rebuild incorporates
available projections. Backfill and repeated synchronization must be
idempotent.

Generated SQL bindings are regenerated with `make sqlc`. Upgrade tests cover a
database containing legacy aggregate summaries as well as an empty database.

## 13. Versioning and Compatibility

Introduce and persist explicit versions for:

- achievement metadata normalization;
- semantic prompt and JSON schema;
- completion projection algorithm;
- profile algorithm/build key;
- recommendation ranking algorithm;
- grounded evidence schema; and
- profile-summary prompt/schema and input content hash.

Changing semantic class definitions, qualifying classes, the weight formula,
or negative-override rule requires the corresponding version bump and a
deterministic rebuild. Model-name changes alone do not reinterpret an existing
definition unless the configured classifier identity changes and a new valid
classification is published.

Old recommendation JSON without completion fields decodes those fields as
zero. New readers continue to render stored old rankings from their saved
factors/evidence rather than recomputing them at read time. New APIs do not add
a completion mutation contract.

## 14. Security and Privacy

- Steam and OpenAI keys stay server-side; real upstream traffic uses HTTPS.
- Achievement classifier inputs contain shared game metadata only, never
  SteamID, user UUID, unlock state, playtime, feedback text, or profile data.
- Per-user unlocks, completion evidence, and projections are user-derived data
  and cascade on user deletion.
- Shared achievement definitions and their semantic classifications contain no
  user identity and may remain with the shared game catalog after deletion.
- User-specific projection and rebuild jobs carry the existing tombstone/fence
  identity and cannot recreate data after deletion. A shared definition job
  may finish after deletion but cannot write a user row.
- Diagnostics aggregate repeated per-game Steam stages and never expose AppID,
  API achievement names, display names, descriptions, raw bodies, model
  prompts, job payloads, or personal identifiers.
- Public recommendation evidence exposes only sanitized game names and
  allowlisted completion wording. It does not expose achievement text or
  internal weights.
- Test-only Steam/OpenAI overrides remain fail-closed behind
  `TEST_HARNESS_MODE=1`.

## 15. Likely Implementation Surface

The implementation plan should verify the exact current tree, but the expected
surface is:

- Steam boundary: `internal/steam/client.go`, `types.go`, `http_client.go`,
  fixtures, and contract tests;
- synchronization: `internal/sync/achievements.go`, `service.go`, tests, source
  state/diagnostics, and job registration;
- persistence: a new numbered migration, `db/queries/`, generated
  `internal/store/sqlc/`, migration upgrade tests, and deletion tests;
- AI classification: `internal/ai/client.go`, `openai.go`, `types.go`, prompts,
  service/repository code, and strict-output tests;
- profile: `internal/profile/repository.go`, `vectors.go`, service/build keys,
  and tests;
- recommendations: retrieval, types, scoring, evidence, persistence,
  presentation, integration tests, and algorithm version;
- API/frontend contract: handlers and contract tests,
  `web/src/api/types.ts`, `web/src/api/client.ts`, evidence labels/cards, and
  frontend tests; and
- end-to-end fake Steam/OpenAI responses and smoke coverage.

This is a cross-cutting persistence and ranking change. The implementation
must follow the repository's test-first workflow, use `codegraph affected` to
supplement focused test selection, and finish with `make verify`.

## 16. Testing Strategy

### Steam contracts

- Parse API name, achieved state, unlock time, English name, and description
  from the current player endpoint.
- Reject empty/duplicate API names, invalid achieved values, negative unlock
  times, malformed success shapes, and aggregate inconsistencies.
- Trigger schema fallback only for missing/invalid metadata.
- Merge schema metadata by exact API name and never derive unlock state from
  schema.
- Preserve the last successful snapshot on private, unavailable, malformed,
  rate-limited, failed, and not-modified responses.

### Persistence and synchronization

- A successful response transactionally advances one user-game snapshot,
  marks rows absent from that generation as not current, and preserves their
  monotonic `ever_achieved` history.
- A partial persistence failure rolls the transaction back.
- Schema failure does not roll back a valid player snapshot.
- Repeated responses, jobs, and classifications are idempotent.
- Aggregate diagnostics contain counts and safe reasons only.

### Classification

- Every semantic class is accepted only in the versioned strict schema.
- Missing, duplicate, or unknown evidence keys; unknown enums; invalid JSON;
  and unsafe output publish nothing.
- DLC endings, meta all-endings achievements, partial progress, optional
  challenges, other, and unknown do not increment `ending_count`.
- Base-game endings and explicit base-game completion markers do increment it.
- Changed normalized text changes the content hash and cannot reuse a stale
  classification.
- OpenAI-disabled and failure cases retain prior valid classifications and
  leave unseen definitions unknown.

### Projection formula

Table tests cover:

- no evidence: no completion projection/zero weight;
- 100% only: `1.50`;
- one ending: `1.50`;
- two endings: `1.85`;
- three endings: `2.05`;
- four endings: `2.15`;
- five and many endings: capped at `2.25`;
- 100% plus one ending: `1.50`, not a sum;
- 100% plus two endings: `1.85`;
- duplicate API names: counted once;
- locked, DLC, meta, partial, optional, and unknown achievements: not counted;
- additional Steam achievements after a prior 100% snapshot: historical
  completion retained;
- missing completion date: remains null; and
- repeated projection: identical output and version.

### Profile and ranking

- A baseline completed game contributes exactly `1.50`, equal to the maximum
  recent source and greater than maximum lifetime playtime.
- Completion does not decay with age.
- Completion replaces the old achievement-ratio component for the same game;
  it is not double counted.
- Independent playtime, recent, and positive-feedback components remain
  additive.
- Latest direct dislike/exclude makes effective positive completion zero;
  historical projection and negative evidence remain.
- A later direct like removes the current override deterministically.
- Completion affinity excludes overridden/negative anchors and completed rows
  do not also enter achievement affinity.
- Ranking and evidence ties are deterministic across input order.
- Persisted factor/weight JSON and legacy zero-value decoding round-trip.

### Summary evidence and presentation

- With more than 100 relevant games, at least one game with positive Steam
  two-week activity survives whenever one exists.
- Completion rows survive before unprotected wishlist/history rows.
- Explicit negative overrides are supplied as such and cannot become positive
  completion prose.
- An old completion is described as historical unless independent recent data
  supports a current-interest claim.
- Missing unlock/last-played dates do not produce fabricated recency.
- Completion evidence is specific, sanitized, and contains no internal
  factors, weights, IDs, API names, or raw achievement text.
- Go response structs, TypeScript types, normalization, UI labels, and contract
  fixtures remain aligned.

### Lifecycle and end-to-end

- Legacy 100% rows backfill to `1.50` without external calls.
- User deletion cascades unlocks/evidence/projections while shared definitions
  remain.
- An in-flight fenced projection/rebuild cannot recreate deleted user data.
- Fake Steam data covers ordinary, missing-metadata, schema-fallback, private,
  and failed responses.
- Fake OpenAI data covers all qualifying/non-qualifying classes and invalid
  output.
- A short completed game becomes a strong profile anchor and affects a
  published deterministic ranking even with low playtime.
- The same flow publishes successfully with OpenAI disabled when the 100%
  deterministic rule is available.

## 17. Acceptance Criteria

The design is implemented when all of the following are true:

1. No manual completion field, endpoint, action, or UI exists.
2. The current localized player-achievement call is the primary metadata
   source and game schema is fallback-only.
3. Successful detailed snapshots preserve per-achievement identity, metadata,
   achieved state, and unlock time; failed/unavailable snapshots preserve the
   last successful state.
4. Only distinct unlocked base-game ending/completion classifications count;
   DLC and meta all-endings achievements do not.
5. 100% completion works without OpenAI and persists historically.
6. The exact weight formula is `1.50`, `1.85`, `2.05`, then `+0.10` per further
   ending capped at `2.25`; 100% and ending bases use `max`, never addition.
7. Completion replaces achievement ratio for that source, remains independent
   of playtime/recent/feedback, and does not recency-decay.
8. Latest explicit dislike/exclude suppresses the positive completion signal
   without deleting the historical fact.
9. OpenAI supplies only versioned semantic classes; all projection, weighting,
   affinities, scoring, evidence selection, and ordering remain deterministic
   Go behavior.
10. Profile-summary evidence protects Steam two-week activity and inferred
    completion before unprotected wishlist/history rows and distinguishes
    historical completion from current interest.
11. New factor/evidence contracts are aligned across persistence, Go API,
    TypeScript normalization, UI, and tests; legacy rankings remain readable.
12. Migration/backfill, user deletion, job replay, source failure, and
    classifier failure preserve local-first operation and last-known-good
    state.
13. Focused race-enabled backend tests, frontend test/typecheck/lint/build, and
    the repository-wide verification command pass with zero failures.

## 18. Follow-Up Boundary

After this written specification is reviewed, the next step is a separate
implementation plan. Implementation must not begin from this design document
alone. Any later proposal to add manual completion correction, achievement
rarity, non-Steam evidence, or a full-catalog schema crawl requires a separate
design decision.
