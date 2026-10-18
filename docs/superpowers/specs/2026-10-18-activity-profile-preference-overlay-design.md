# Activity Profile and Preference Overlay Design

**Date:** 2026-10-18

**Status:** Ready for user review

**Related MVP design:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`

**Related completion design:** `docs/superpowers/specs/2026-10-18-achievement-completion-signals-design.md`

**Related feedback design:** `docs/superpowers/specs/2026-10-18-feedback-preference-corrections-calendar-design.md`

## 1. Objective

Separate two facts that the current implementation treats as one model:

1. the user's **Activity Profile**, which describes what the user has actually
   played; and
2. the **Preference Overlay**, which represents explicit likes, dislikes,
   exclusions, and active corrections used to adapt recommendations.

The Activity Profile must remain an account of observed Steam activity. A new
reaction to a recommendation must change the next ranking immediately, but it
must not rewrite the narrative of what the user has played or trigger another
AI profile summary.

Success means:

- only played-game evidence contributes to Activity Profile vectors, confidence,
  evidence counts, and summary text;
- wishlist entries, feedback, preference corrections, and unplayed owned games
  never affect that profile directly or indirectly;
- the latest effective direct reaction per game and effective active corrections
  form a separate, versioned Preference Overlay;
- the overlay can retrieve and softly adjust similar games without becoming the
  final ranking authority;
- feedback and correction changes rebuild only the overlay and recommendations;
- successful Steam synchronization rebuilds the Activity Profile and
  recommendations;
- the API keeps returning the last fully published profile while a newer summary
  is being prepared, and reports that an update is in progress;
- Steam and OpenAI failures preserve last-known-good user-visible state; and
- entering the authenticated application starts a Steam synchronization only
  when the last successful Steam workflow is more than 24 hours old.

## 2. Scope and Non-Goals

### In scope

- Activity-only signal selection and vector construction.
- Activity-only evidence for the Russian profile summary.
- Explicit publication state for profile versions and stale-while-revalidate
  reads.
- A versioned Preference Overlay with positive and negative semantic vectors.
- Latest-reaction semantics for direct feedback and correction effectiveness.
- Overlay-aware semantic retrieval and deterministic ranking factors.
- Trigger changes for Steam synchronization, feedback, and correction disable.
- A backend-enforced 24-hour application-entry freshness check.
- API, TypeScript, and profile-card state changes.
- Migration, legacy backfill, deletion, diagnostics, and test coverage.

### Out of scope

- Steam endpoint, achievement-import, catalog, discovery, or release-date
  pipeline redesign.
- Multiple users, public registration, or cross-account learning.
- A full Steam catalog or a new datastore.
- Letting OpenAI choose scores, weights, eligibility, or ordering.
- Inferring that an owned game with no play evidence was played.
- Using wishlist membership as taste evidence or as a score boost.
- Reworking feedback dialogs, calendar controls, or digest behavior.
- Fixing evidence keys rendered inside AI summary prose. That is an adjacent,
  bounded output-sanitization change described in section 19, not part of this
  architecture migration.

## 3. Current-State Problem

`internal/profile/repository.go` currently loads a union of `user_games`,
`wishlist_entries`, feedback source games, and completion projections. The
vector builder in `internal/profile/vectors.go` gives positive weight to
wishlist and likes and negative weight to every historical dislike. The AI
evidence loader in `internal/ai/repository.go` repeats the same mixture in the
user-facing summary.

This produces four incorrect behaviors:

1. a wishlist concept or one reaction can be narrated as part of the user's
   played taste history;
2. `dislike -> like` can leave both historical events contributing to different
   sides of a vector even though only the newest reaction is effective;
3. feedback and correction-disable workflows enqueue `profile.rebuild`, so a
   recommendation reaction needlessly regenerates the Activity Profile and its
   AI summary; and
4. `LatestProfile` orders every `preference_profiles` row by version, including
   a newly inserted row whose summary is still empty. The ready profile therefore
   disappears until `ai.profile_summary` finishes.

The existing recommendation pipeline already gives wishlist a zero numeric
weight, but wishlist still influences the profile vector that supplies semantic
retrieval. Its indirect ranking weight is therefore not zero.

## 4. Terminology

### Activity evidence

Evidence that the user actually played a game. A game is eligible when at least
one of these saved facts is positive:

- `user_games.playtime_minutes > 0`;
- `user_games.recently_played_minutes > 0`;
- `achievement_summaries.achieved > 0`; or
- `user_game_completion_projections.weight > 0`.

Ownership alone, a zero-achievement snapshot, wishlist membership, catalog
membership, and feedback are not activity evidence.

### Activity Profile

A versioned deterministic activity snapshot with a separately published summary
presentation. The existing `preference_profiles` name remains in storage for a
minimal migration, but new code and UI call this concept the Activity Profile.

### Preference Overlay

A versioned deterministic projection of effective explicit preferences. It
contains semantic centroids from latest direct reactions and a digest of
effective structured corrections. It does not contain Steam activity or
wishlist evidence.

### Draft profile

An Activity Profile version whose deterministic vectors are complete and may be
used internally for ranking, but whose complete user-visible document has not
yet been published.

### Published profile

A profile version with a nonblank validated summary, complete metadata, and one
atomic publication timestamp. Only a published version is returned as the
visible profile.

### Effective direct reaction

The newest `like`, `dislike`, or legacy `exclude` for one `(user, game)`, ordered
by `created_at DESC, id DESC`. `remind_me` is not a direct reaction and does not
supersede one.

### Effective correction

A `preference_adjustment` that is enabled and whose `source_feedback_id` is the
effective direct negative reaction for its source game. A later like makes the
older negative correction ineffective without deleting its audit history.

## 5. Source-of-Truth Rules

### Activity Profile inputs

The Activity Profile uses only eligible activity evidence and the latest saved
game embedding for each eligible game. Its positive weights remain deterministic:

- normalized lifetime playtime, with the existing historical recency decay;
- two-week activity;
- unlocked-achievement ratio; and
- durable completion strength.

Completion remains a historical fact even after a dislike. Feedback must not
set `completion_overridden` in Activity Profile calculation or summary evidence.
The overlay may counteract completion in ranking, but it cannot rewrite the
activity record.

New Activity Profile versions have one positive activity vector. A negative
Activity Profile vector has no product meaning. New versions do not write one,
and readers ignore legacy negative profile embeddings.

Confidence and `evidenceCount` are calculated only from eligible activity
signals that have usable embeddings. Missing semantic enrichment lowers semantic
coverage but never turns wishlist or feedback into substitute evidence.

### Activity Profile exclusions

The following have zero contribution to vector construction, confidence,
evidence count, summary evidence selection, and activity anchors:

- wishlist membership or priority;
- like, dislike, exclude, reminder, reason, and free text;
- feedback classification and preference adjustments;
- owned library rows with no positive activity evidence; and
- catalog-only games.

### Preference Overlay inputs

The overlay is rebuilt from a single repeatable-read snapshot containing:

- the effective direct reaction for each game;
- the latest saved game embedding for those reaction games; and
- effective corrections.

An effective `like` contributes its game's embedding once to the positive
centroid. An effective `dislike` or `exclude` contributes it once to the negative
centroid. Historical opposite reactions remain in `feedback_events` for audit
but contribute nothing. Reaction counts do not stack.

An effective correction stays structured. For example, `mode=co-op -> avoid`
contributes the existing deterministic mode penalty to matching games; it does
not add every property of the source game's embedding to the negative centroid.
Repeated effective corrections for the same target retain the existing bounded
`max(confidence * strength)` aggregation.

If a reaction game lacks a saved embedding, its direct exact-game effect still
applies, but it contributes no semantic vector until enrichment succeeds. The
overlay never calls OpenAI and never blocks direct deterministic feedback.

### Wishlist

Wishlist remains:

- a mandatory candidate source even when a game is outside the normal discovery
  horizon or lacks an embedding;
- a presentation lane and release-tracking marker; and
- allowed context in a recommendation explanation when clearly labeled as
  wishlist context.

Wishlist has zero direct score weight and zero indirect profile, overlay,
retrieval-affinity, tag-affinity, explanation-anchor, and confidence weight.
Being wishlisted is not evidence that the user played or will enjoy a game.

## 6. Data Model and Migration

Add the next numbered migration, expected to be
`db/migrations/00017_activity_profile_preference_overlay.sql`. Do not modify an
applied migration. Regenerate `internal/store/sqlc/` only through `make sqlc`.

### Activity Profile publication columns

Keep `preference_profiles` as the physical table and add:

- `publication_state text NOT NULL`, constrained to `building`, `published`,
  `failed`, or `insufficient`;
- `published_at timestamptz`, non-null exactly when state is `published`; and
- `summary_source text`, constrained to `openai`, `deterministic`, or `legacy`,
  non-null exactly when state is `published`.

The migration first adds nullable columns, backfills rows with a nonblank
summary as `published` with `published_at=created_at` and
`summary_source='legacy'`, marks other existing rows `failed`, then installs the
checks and defaults. New rows default to `building`, with no published timestamp
or summary source. `insufficient` means a successful source snapshot contained
no qualifying activity; like `building` and `failed`, it has no published
timestamp or summary source.

`preference_profiles.version`, `build_key`, `algorithm_metadata`, and
`profile_embeddings` remain. The Activity Profile algorithm version is bumped.
Its metadata records the activity signal counts by kind and the source workflow
identity, but never raw game or account identifiers.

### Preference Overlay tables

Add `preference_overlays`:

- `id uuid primary key`;
- `user_id uuid not null references users(id) on delete cascade`;
- `version bigint not null` with unique `(user_id, version)`;
- `algorithm_version text not null`;
- `build_key text not null` with unique `(user_id, build_key)`;
- `input_content_hash text not null`;
- `algorithm_metadata jsonb not null default '{}'`;
- `created_at timestamptz not null default now()`.

Add `preference_overlay_embeddings`:

- `overlay_id uuid not null references preference_overlays(id) on delete cascade`;
- `polarity text not null` constrained to `positive` or `negative`;
- `model text not null`;
- `input_content_hash text not null`;
- `embedding vector(1536) not null`;
- `source text not null default 'deterministic'`;
- `last_successful_fetch_at timestamptz not null default now()`; and
- primary key `(overlay_id, polarity, model)`.

An overlay version exists even when it has no usable vector. In that case it has
metadata and a stable empty-input hash but no embedding row for the absent
polarity. This avoids fabricating zero-vector similarity while preserving
version identity and idempotency.

### Recommendation provenance

Add nullable `preference_overlay_id` foreign keys to `recommendation_runs` and
`recommendations`. They remain nullable only for legacy rows. New ranking runs
must store both the Activity Profile ID and Preference Overlay ID, including an
empty overlay version. The ranking build key includes both versions and both
algorithm versions.

No migration rewrites historical recommendation factors or feedback events.

## 7. Activity Profile Build and Publication

Keep the existing `profile.rebuild` job kind for queued-job compatibility, but
change its semantics to Activity Profile only.

### Build phase

Within one transaction and user-scoped advisory lock:

1. load eligible activity signals only;
2. compute the deterministic positive activity vector and metadata;
3. deduplicate by activity build key and algorithm version;
4. insert one `building` profile row and its positive embedding; and
5. commit the draft before scheduling summary generation.

When the qualifying signal set is empty, insert an `insufficient` internal
snapshot with no profile embedding and do not schedule `ai.profile_summary`.
This row gives a deterministic recommendation run exact source provenance
without fabricating a semantic profile.

The build result exposes the draft profile ID/version to the ranking workflow.
It is not selected by the public profile read.

### Summary phase

`ai.profile_summary` loads evidence from the draft's eligible played games only.
Its prompt and schema may describe current activity, historical play,
achievements, and completion, but receive no wishlist or feedback fields.

On valid model output, one fenced transaction updates the draft's summary,
model/hash metadata, `summary_source='openai'`,
`publication_state='published'`, and `published_at`. A reader sees either the
entire previous published version or the entire new version; it never sees the
old summary paired with new confidence, evidence count, or version metadata.

If OpenAI is explicitly disabled, the summary job creates a deterministic
Russian fallback from aggregate activity facts and publishes the draft with
`summary_source='deterministic'`. The fallback may state which activity classes
are present and their counts, but cannot infer genres, mechanics, or themes.

On a transient, refused, malformed, or invalid OpenAI result:

- normal retries keep the draft `building`;
- the terminal lease-fenced attempt marks it `failed`;
- when an older published profile exists, that version remains visible; and
- when no published profile has ever existed, the terminal attempt writes the
  deterministic fallback and publishes the first profile instead of leaving the
  profile card permanently empty.

This first-run exception is deterministic and does not weaken model-output
validation.

### Read semantics

The public profile repository first inspects the newest Activity Profile row. A
newest `published` row is returned. A newest `building` or `failed` row falls
back to the newest older published row; only `building` reports
`updating=true`. A newest `insufficient` row is authoritative successful empty
activity and returns no visible profile instead of resurrecting an older one.

Internal ranking code uses the exact draft Activity Profile ID produced by the
current Steam workflow. A manual recommendation rebuild outside that workflow
uses the newest vector-complete Activity Profile, whether published or building.
It never combines vectors from one profile version with metadata from another.

It is therefore valid for a newly published recommendation run to reference a
newer Activity Profile vector while the profile card still displays the
previous fully published narrative with `updating=true`. This is intentional:
ranking freshness and document publication are separate boundaries.

## 8. Preference Overlay Build

Add job kind `preferences.rebuild`. One rebuild:

1. reads effective direct reactions and effective corrections in a
   repeatable-read transaction;
2. orders inputs deterministically by game ID and feedback ID;
3. computes positive and negative centroids from available saved game
   embeddings;
4. computes a canonical content hash over effective reaction identities,
   actions, embedding hashes, and effective correction values;
5. reuses an existing overlay with the same build key/content hash or inserts
   the next version under a user-scoped advisory lock; and
6. stores only aggregate safe metadata, such as positive/negative vector source
   counts and effective correction count.

The overlay is deterministic. It does not call OpenAI and does not depend on a
profile summary. A missing game embedding is recorded only as an aggregate
count; it does not fail the rebuild or remove the direct feedback effect.

The effective-reaction query uses `DISTINCT ON (game_id)` with
`created_at DESC, id DESC` and filters to direct actions. Therefore:

- `dislike -> like` yields one positive contribution and no negative
  contribution;
- `like -> dislike` yields one negative contribution and no positive
  contribution;
- a reminder changes neither result; and
- retrying the same idempotent feedback event cannot create another
  contribution.

Effective correction queries must join their source feedback to this same
latest-direct projection. A correction sourced from a superseded dislike stays
in immutable history but is absent from overlay metadata, API defaults, and
ranking factors.

## 9. Workflow Triggers and State Transitions

### Successful Steam synchronization

The required flow becomes:

```text
Steam import and achievement projection
  -> optional game feature/embedding refresh
  -> profile.rebuild (activity-only draft vectors)
  -> recommendations.rebuild (exact activity profile + latest overlay)
  -> root Steam workflow succeeds

profile.rebuild
  -> ai.profile_summary (independent publication work)
       -> publish new profile atomically
       -> or preserve the prior published profile
```

The recommendation stage does not wait for prose. A Steam source failure keeps
the corresponding last successful source state and therefore cannot replace a
valid profile with a successful empty one.

### Feedback

For `like`, `dislike`, or legacy `exclude`:

```text
save immutable feedback event
  -> classify and persist any correction
  -> preferences.rebuild
  -> recommendations.rebuild
  -> publish ranking and return diff
```

For `remind_me`, retain the current reminder workflow and do not rebuild either
profile or overlay unless another existing reminder requirement needs ranking.

Feedback never enqueues `profile.rebuild` or `ai.profile_summary`.

### Correction disable

Disabling an adjustment rebuilds `preferences.rebuild` and then
`recommendations.rebuild`. It does not rebuild the Activity Profile. Repeated
disable requests retain their existing idempotent job/version response.

### Manual synchronization

The existing authenticated, CSRF-protected `POST /api/sync` remains available
at all times. It requests a synchronization regardless of freshness, while the
existing user-scoped lock returns an already-active workflow instead of
creating a competing one.

## 10. Application-Entry Freshness

Add owner-only, authenticated, CSRF-protected
`POST /api/sync/refresh-if-stale`. The authenticated dashboard calls it once on
application entry after `/api/me` supplies the CSRF token.

The backend, not the browser clock, decides freshness:

- the watermark is `updated_at` of the newest succeeded root `steam.sync` job
  for the user;
- no succeeded workflow is stale;
- a watermark at or before `now() - 24 hours` is stale;
- a newer watermark is fresh; and
- an existing queued, running, waiting, or retryable `steam.sync` workflow is
  returned instead of creating another job.

The response is safe and explicit:

```json
{
  "enqueued": true,
  "reason": "stale",
  "jobId": "opaque-job-id",
  "lastSuccessfulAt": "2026-10-17T10:00:00Z"
}
```

`reason` is one of `missing`, `stale`, `fresh`, or `active`. `jobId` is present
for a created or active workflow; `lastSuccessfulAt` is omitted when absent.
A fresh response uses HTTP 200; created or already-active work uses HTTP 202.

The endpoint is fail-closed for demo sessions and non-owner sessions. A failure
to perform the freshness check does not block rendering saved profile and
recommendation data. Multiple tabs and process restarts are safe because the
existing database advisory lock and active-job query remain authoritative.
When synchronization is needed, the endpoint applies the same Steam privacy
preflight as manual sync; a blocked preflight returns the existing sanitized
blocked response and does not enqueue work.

## 11. Ranking and Retrieval Integration

Candidate eligibility and final ordering remain deterministic.

### Retrieval union

Retrieve a bounded union of:

1. nearest neighbors of the current Activity Profile positive vector;
2. nearest neighbors of the current Preference Overlay positive vector; and
3. mandatory wishlist/catalog candidates under the existing horizon rules.

Deduplicate by game ID before scoring. The positive overlay path lets an
explicit like generalize to similar future games even when those games are not
nearest to the broader activity history. The negative overlay vector does not
retrieve candidates; it only supplies a soft penalty to candidates already in
the union.

Each nearest-neighbor leg has its own fixed limit in the ranking algorithm
version. Increasing the union size or weights requires an algorithm-version
bump and regression tests.

### Factors

Replace the ambiguous shared semantic factors with separately persisted
factors:

- `activitySimilarity`;
- `positivePreferenceSimilarity`;
- `negativePreferenceSimilarity`;
- `positiveDirectFeedback` for the exact candidate's latest like;
- `negativeDirectFeedback` for the exact candidate's latest negative action;
  and
- the existing `negativeModePreference` for effective corrections.

Tag, playtime, achievement, recent-activity, and completion affinity use only
played Activity Profile anchors. Wishlist and reaction-only games cannot become
those anchors.

### Weight principles

- Activity similarity remains the primary semantic taste factor.
- Positive overlay similarity is a bounded soft boost smaller than the maximum
  activity semantic contribution.
- Negative overlay similarity is a bounded soft penalty; similarity alone never
  hard-excludes another game.
- Latest direct feedback retains the exact-game effect. A current dislike or
  exclude may keep that game ineligible; a later like reverses it after the
  existing authoritative workflow reconciliation.
- Effective structured corrections retain bounded deterministic penalties and
  never hard-exclude a category.
- Wishlist weight remains exactly zero.
- The sum is clamped and normalized as today, with stable name/game-ID
  tie-breakers.

Exact numeric overlay weights are fixed in the implementation plan after tests
capture the invariants above. They must be saved in every factor snapshot and
require a ranking algorithm version bump. OpenAI may provide game features and
embeddings, but cannot choose or modify these values.

## 12. API and TypeScript Contracts

### `GET /api/profile`

Return only the Activity Profile document:

```json
{
  "id": "opaque-profile-id",
  "version": 8,
  "summary": "...",
  "summaryStatus": "ready",
  "summarySource": "openai",
  "confidence": 0.72,
  "evidenceCount": 14,
  "createdAt": "2026-10-18T09:00:00Z",
  "publishedAt": "2026-10-18T09:00:08Z",
  "updating": true
}
```

`summarySource` is `openai`, `deterministic`, or `legacy`. A returned published
profile always has `summaryStatus='ready'` and a nonblank summary. If no profile
has ever been published, return 404; the dashboard combines that with current
sync state to render first-run progress rather than the normal empty-profile
copy.

Remove `adjustments` from this response. The existing authenticated
`GET /api/preference-adjustments` remains the source for effective explicit
corrections and returns only effective enabled adjustments unless diagnostics
explicitly requests history.

Update `web/src/api/types.ts`, `web/src/api/client.ts`, normalization, API
handler tests, and dashboard tests together. `PreferenceProfileData` gains
`summarySource`, `publishedAt`, and `updating` and no longer owns an
`adjustments` array. The dashboard loads preference adjustments as a separate
resource.

### Recommendation contracts

Rename the semantic/direct feedback factor fields consistently in Go JSON,
`web/src/api/types.ts`, safe normalization, evidence mapping, contract tests,
and diagnostics. Legacy stored rows remain readable by a compatibility decoder
until the next ranking is published; new writes use only the new factor names.

The overlay ID/version are internal provenance and are not exposed as raw IDs
in the normal recommendation UI.

## 13. User Interface Behavior

The profile card is titled **Профиль игры** or retains **Профиль вкуса** only if
its explanatory copy clearly says it is based on played activity. Its source
details list games played, recent activity, achievements, and completions only.
They must not mention wishlist or reactions.

When `updating=true`:

- keep rendering the complete previous profile;
- show a nonblocking label such as `Профиль обновляется`;
- do not replace its summary, confidence, evidence count, version, or source
  details until the new version publishes; and
- do not disable recommendation browsing or feedback.

On the first run before any published profile, show synchronization/profile
build progress. If OpenAI is disabled or terminally unavailable, the first
deterministic fallback is published and replaces that state.

Move active correction presentation to a visually separate section named, for
example, **Явные настройки рекомендаций**. Likes/dislikes remain visible on
recommendation cards. The UI must not imply that those controls change the
played Activity Profile.

Failure copy says that the previous profile remains in use. It must not clear
the card or silently show the generic `Профиль собран...` text in place of a
previously ready summary.

## 14. Concurrency and Idempotency

- Activity Profile builds serialize on the existing user-scoped profile
  advisory lock and deduplicate by source workflow plus algorithm version.
- Summary publication updates only a `building` row and uses the active job
  fencing token. A late result cannot overwrite a `failed` or already published
  version.
- Preference Overlay builds serialize on a distinct user-scoped overlay lock
  and deduplicate by canonical input hash/build key.
- Recommendation builds bind exact profile and overlay IDs before computing
  factors and atomically publish one ranking run.
- If feedback arrives during a Steam rebuild, the later ranking build must
  resolve the newest completed overlay under its repeatable-read snapshot. A
  build key collision cannot publish different inputs.
- If Steam activity changes during feedback processing, each ranking remains a
  coherent pair of one Activity Profile version and one overlay version. A
  later idempotent rebuild may supersede it; fields are never mixed within a
  run.
- Job retries reuse the same idempotency keys and cannot increment profile,
  overlay, or ranking versions twice for the same canonical build.
- Deletion fencing continues to prevent an in-flight worker from recreating
  rows after account deletion.

## 15. Error and Fallback Behavior

### Steam unavailable, private, or malformed

Preserve each last successful source snapshot exactly as today. Do not create a
new successful empty Activity Profile. If the required workflow cannot produce
a coherent activity build, preserve the last published profile and ranking.

### Game enrichment or embeddings unavailable

Use last successful saved game features/embeddings. New games without vectors
may still participate in deterministic exact signals and wishlist candidate
membership, but provide no semantic similarity until enriched. Missing vectors
do not authorize title-based inference.

### OpenAI disabled

Activity vectors, Preference Overlay, retrieval from saved embeddings,
deterministic scoring, and feedback continue to work. Activity Profile summary
publication uses the deterministic fallback. Recommendation explanations retain
their existing deterministic fallback behavior.

### OpenAI transient or invalid output

Never blank the current profile. Keep the previous published version while the
new draft retries. On terminal failure mark the draft failed; only the no-prior-
profile case publishes the deterministic first-run fallback.

### Overlay rebuild failure

The immutable feedback event remains saved. Keep the last successful overlay
and last published ranking, expose the existing retryable/dead workflow status,
and never claim the new reaction affected ranking until the ranking workflow
publishes and reconciles.

### No activity evidence

Do not build a semantic Activity Profile from ownership, wishlist, or feedback.
Record an internal `insufficient` snapshot for coherent recommendation
provenance and return no visible profile, even if an older activity version was
previously published. The recommendation system may still show wishlist release
tracking and bounded catalog discovery with deterministic non-profile factors,
but must not describe them as personalized taste matches.

## 16. Privacy, Security, and Deletion

- All new endpoints use the existing authenticated allowlisted owner boundary;
  mutations require CSRF protection.
- Demo sessions may read the published profile and recommendations but cannot
  trigger freshness synchronization, feedback, overlay, or ranking mutations.
- Overlay hashes and metadata never include raw free text, SteamID, user UUID,
  recommendation UUID, job payload, prompt, session data, or secrets.
- General logs contain only safe counts, versions, stages, durations, and
  enumerated reasons. They never contain game-level reaction data or raw model
  content.
- `preference_overlays` and their embeddings cascade from `users`; recommendation
  provenance cascades with existing user-owned rankings.
- Existing deletion tombstones and user-scoped job fencing cover the new overlay
  and profile publication writes before commit.
- The migration attaches the existing deletion-guard trigger to both new overlay
  tables and extends the user-data deletion transaction to remove them before
  the user row. A late insert or publication after a tombstone fails closed.
- Shared game metadata, embeddings, achievement definitions, and classifications
  retain their existing shared-data lifecycle.

## 17. Rollout and Backfill

Deploy in this order:

1. apply the numbered migration and regenerate sqlc bindings;
2. deploy readers that understand legacy nullable overlay provenance and profile
   publication fields;
3. deploy Activity Profile and Preference Overlay writers with bumped algorithm
   versions;
4. enqueue one idempotent per-user boundary backfill for the allowlisted owner;
5. rebuild recommendations from the resulting exact activity/overlay pair; and
6. remove the legacy factor compatibility decoder only in a later migration
   after every visible ranking has been republished.

The boundary backfill uses saved local snapshots; it does not delete or refetch
Steam data. It builds an activity-only profile, builds the latest-reaction
overlay, and reranks. While it runs, the most recent nonblank legacy profile
remains published with `summarySource='legacy'` and `updating=true`. Publication
of the new activity-only version atomically replaces it.

If the local database has no user, the backfill is a no-op. If it has no played
evidence, it creates an internal `insufficient` Activity Profile snapshot plus
an empty overlay for recommendation provenance and does not publish a fabricated
profile. Backfill idempotency keys include the boundary version so restarts
cannot duplicate versions.

No raw SQL data rewrite is needed for feedback history. Effective-state queries
derive the latest reaction deterministically from existing immutable events.

## 18. Observability

Add safe structured events and diagnostics for:

- Activity Profile build started/succeeded/failed with profile version, activity
  signal count, vector-bearing count, duration, and enumerated reason;
- summary publication started/published/failed with summary source and duration;
- stale-while-revalidate reads with published version and a boolean newer-draft
  flag, not profile text;
- overlay build started/succeeded/failed with overlay version and aggregate
  positive, negative, missing-vector, and effective-correction counts;
- application-entry freshness decisions `missing`, `stale`, `fresh`, or
  `active`; and
- ranking publication with algorithm version plus activity/overlay versions.

Diagnostics must distinguish a failed summary publication from a failed
Activity Profile vector build. A dead optional summary job must not make the
Steam import or deterministic ranking appear to have lost saved data.

## 19. Adjacent Bounded Follow-Up: Inline Evidence Keys

The current `profile-summary-v5-synthesis` instruction says to cite supplied
evidence keys but does not explicitly confine them to the structured
`evidence_keys` field. A model can therefore render `[signal-029]` inside the
summary prose.

A separate bounded patch should:

- explicitly forbid opaque evidence keys in user-facing `summary`,
  `preferences`, `avoidances`, and `uncertainty` text;
- permit them only in structured `evidence_keys`;
- remove bracket groups made entirely of allowed evidence keys before saving,
  without accepting unknown keys; and
- add regression tests for inline citations and wishlist/direct-like wording.

That patch may ship before or after this architecture. It must not reintroduce
wishlist or feedback into Activity Profile evidence.

## 20. Testing Strategy

Write the smallest failing test before each behavior change, then run focused
tests with `GOWORK=off` and `-race` for Go packages.

### Profile repository and vectors

- A played game contributes activity weight.
- Recent activity, unlocked achievements, and completion independently qualify
  a played signal.
- An owned zero-playtime game does not contribute.
- An empty qualifying set creates an `insufficient` snapshot without an
  embedding or summary job.
- Wishlist-only and feedback-only games do not load as activity signals.
- Likes/dislikes do not change Activity Profile vectors, confidence, evidence
  count, or summary evidence.
- A disliked completed game remains completion evidence in the Activity Profile.
- New profiles contain no negative activity embedding.

### Profile publication

- A building v2 does not replace published v1 and makes `updating=true`.
- Publishing v2 atomically returns all v2 fields and `updating=false`.
- Failed v2 preserves v1 and clears the updating indicator.
- An authoritative `insufficient` newest snapshot returns no older visible
  profile.
- OpenAI-disabled first build publishes a deterministic ready profile.
- Terminal invalid output with no prior profile publishes the deterministic
  fallback; the same failure with a prior profile preserves the prior version.
- A late or lease-lost summary result cannot publish.

### Preference Overlay

- `dislike -> like` produces only a positive vector contribution.
- `like -> dislike` produces only a negative vector contribution.
- `remind_me` does not supersede a direct reaction.
- Duplicate idempotent feedback does not add weight.
- Missing embeddings preserve direct effects but add no vector.
- A later like makes a correction from the earlier dislike ineffective.
- Disabled and superseded corrections do not affect ranking or the default API.
- Overlay retries reuse the same version/build result.

### Retrieval and ranking

- An overlay-positive neighbor enters the candidate union and receives a bounded
  deterministic boost.
- Overlay-negative similarity lowers but does not exclude a similar game.
- The exact latest dislike remains excludable and a later like restores it after
  authoritative reconciliation.
- Wishlist changes candidate membership/presentation only; toggling wishlist
  cannot change any affinity factor or score.
- Tag/playtime/recent/achievement/completion anchors contain only played games.
- Saved factors and build keys identify exact activity and overlay versions.
- Stable tie-breaking is unchanged.

### Workflows and freshness

- Feedback and correction-disable workflows never enqueue `profile.rebuild` or
  `ai.profile_summary`.
- They enqueue overlay before ranking and return the published ranking diff.
- Successful Steam sync builds activity then ranking without waiting for prose.
- App entry at 23h59m does not enqueue; at exactly 24h it enqueues.
- No prior successful sync enqueues.
- Existing active sync is reused across tabs.
- Manual sync remains available when fresh.
- Demo entry cannot enqueue.

### API and UI

- `/api/profile` never mixes profile versions and never returns adjustments.
- The TypeScript contract accepts `summarySource`, `publishedAt`, and `updating`.
- The card retains the old summary and displays `Профиль обновляется`.
- First-run progress and deterministic fallback are visible and accessible.
- The profile evidence copy contains no wishlist or feedback claim.
- Explicit corrections render in their own section.
- Existing stale dashboard reload behavior preserves both resources.

### Integration and release gate

- Database migration up/down and generated sqlc checks pass.
- Product-loop E2E proves Steam sync -> activity profile -> recommendations.
- Feedback E2E proves profile version/summary stay unchanged while overlay and
  ranking versions advance.
- Disabled-OpenAI E2E proves first-run deterministic profile, overlay feedback,
  and ranking continue to work.
- Deletion integration tests prove new rows are removed and in-flight jobs cannot
  recreate them.
- Run one fresh `make verify` after focused suites stabilize.

## 21. Acceptance Criteria

- The Activity Profile uses only positive evidence of actual play.
- Wishlist, feedback, corrections, and unplayed ownership cannot alter its
  vectors, confidence, evidence count, summary, or source list.
- Feedback changes the overlay and the next ranking without changing the
  Activity Profile version or scheduling a profile summary.
- The newest direct reaction per game is the only effective semantic reaction;
  a dislike followed by a like never counts on both sides.
- A correction sourced by a superseded negative reaction is no longer effective.
- Likes and dislikes generalize softly to similar games through overlay
  retrieval/scoring, while deterministic code retains final authority.
- Wishlist remains a candidate/release-tracking source with zero direct and
  indirect affinity weight.
- A ready profile never disappears while a newer summary is computing.
- Profile publication is atomic, and failed refreshes preserve the last
  successful version.
- First-run and explicitly disabled OpenAI produce a deterministic, grounded
  fallback instead of an empty card.
- Authenticated owner application entry enqueues at most one Steam workflow when
  the last successful root sync is at least 24 hours old; manual sync always
  remains available.
- Recommendation rows record both Activity Profile and Preference Overlay
  provenance.
- Optional upstream failures do not convert unavailable sources into successful
  empty state.
- User deletion and logging rules remain fail-closed and privacy-safe.

## 22. Expected Implementation Impact

The implementation plan should inspect and update at least these existing
areas; exact line ranges must be resolved against the implementation branch:

- `db/migrations/` and `db/queries/` for publication, overlay, effective-state,
  and provenance queries;
- `internal/store/sqlc/` through `make sqlc` only;
- `internal/profile/repository.go`, `internal/profile/vectors.go`, and
  `internal/profile/service.go` for activity-only builds and published reads;
- `internal/ai/repository.go`, `internal/ai/service.go`, and
  `internal/ai/prompts.go` for activity-only evidence and publication;
- `internal/preferences/repository.go`, `internal/preferences/service.go`, and a
  focused overlay builder in `internal/preferences/`;
- `internal/feedback/service.go` for overlay-only reaction workflows;
- `internal/recommendations/retrieval.go`, `internal/recommendations/scoring.go`,
  `internal/recommendations/evidence.go`, and `internal/recommendations/service.go`
  for split retrieval/factors/provenance;
- `internal/sync/service.go` for exact profile handoff and freshness checks;
- `internal/httpapi/profile_handlers.go`, `internal/httpapi/sync_handlers.go`,
  and `internal/httpapi/router.go` for API boundaries;
- `internal/app/app.go` for dependency wiring and job registration;
- `web/src/api/types.ts`, `web/src/api/client.ts`, and `web/src/api/hooks.ts` for
  contracts, entry freshness, and stale-safe loading; and
- `web/src/components/PreferenceProfile.tsx`, preference-adjustment presentation,
  and dashboard tests for the separate UI sections.

No change is expected in the Steam HTTP client, release precision model,
catalog discovery algorithm, SMTP delivery semantics, or authentication
allowlist.

## 23. Compatibility and Supersession

This design narrows and supersedes the following clauses:

- MVP design section 7 step 9 becomes Activity Profile rebuild plus an
  independent Preference Overlay; step 10 binds both versions.
- MVP design section 8's positive centroid of played, wishlisted, and liked games
  and negative centroid of disliked games is replaced by the Activity Profile /
  Preference Overlay split in sections 5 and 8 of this design.
- MVP design section 9's wishlist score of 30 is replaced by a permanent zero
  affinity weight; wishlist candidate inclusion remains.
- MVP design section 10's instruction to update explicit preferences and rebuild
  profile vectors after feedback is replaced by overlay-only rebuild semantics.
- Achievement Completion Signals design rules that let direct dislike suppress
  completion remain true for ranking, but feedback no longer removes or
  overrides completion inside the Activity Profile or its summary.
- Feedback Preference Corrections design sections 6 and 8 are narrowed so
  adjustments and direct feedback rebuild the Preference Overlay and ranking,
  not the Activity Profile. Existing bounded mode-penalty and disable semantics
  otherwise remain.
- The current implemented profile-summary target of 650–900 Unicode characters
  and tolerant sanity validation remain unchanged by this design. The older
  400–600 target and 320–720 acceptance range in
  `docs/superpowers/specs/2026-10-18-profile-summary-length-design.md` are
  superseded by the current prompt/validator contract, not reintroduced here.

All other MVP rules remain in force, especially single-user scope, PostgreSQL as
the only persistent service, exact release precision, deterministic final
ranking, last-known-good upstream behavior, fail-closed test hooks, and
ambiguous SMTP delivery handling.
