# Feedback Preference Corrections and Calendar Actions Design

**Date:** 2026-10-18  
**Status:** Ready for user review  
**Related presentation design:** `docs/superpowers/specs/2026-10-18-discovery-wishlist-presentation-design.md`

## 1. Objective

Let free-text feedback correct future recommendations at the level the user
actually described. A comment such as "не люблю коопы" should softly lower
other cooperative games, not ban them and not affect only the single game that
received the comment.

Make this feedback loop available from every recommendation presentation,
including the calendar grid, exact-date list, and imprecise release buckets.
Those calendar/list items must also show the game poster and provide a direct,
safe link to the Steam store page.

Success means that one high-confidence negative co-op preference produces a
visible, explainable soft penalty on future co-op candidates, while unrelated
games and missing/uncertain feature data are not penalized.

## 2. Current Baseline

The current workflow already stores `like`/`dislike`, an optional predefined
reason, and up to 500 Unicode characters of free text. OpenAI classifies free
text into a broad taxonomy and the worker rebuilds the profile and ranking.

Current limitations:

- `multiplayer / negative` classification is not persisted as a reusable rule
  such as `mode=co-op → avoid`.
- The negative profile vector is built from the embedding of the disliked game,
  so similar games may be lowered, but the behavior is indirect and cannot
  reliably express "all co-op games".
- An actionable negative classification increases the same source game's
  negative profile weight in addition to the direct dislike. It does not create
  a global co-op rule and should not double-count the source game.
- Candidate eligibility counts direct dislikes; two direct dislikes can exclude
  the exact game. Category-level feedback must not use that hard-exclusion path.
- Full dashboard cards render `FeedbackForm`; compact calendar cards and release
  buckets do not.
- Calendar grid events and release buckets show only text and score even though
  `Recommendation` already contains `coverImageUrl` and `storeUrl`.

## 3. Product Semantics

The first version supports explicit negative **mode preferences**. Canonical
targets are:

- `co-op`;
- `competitive-multiplayer`;
- `massively-multiplayer`;
- `local-multiplayer`;
- `online-multiplayer`;
- `single-player`.

Russian and English aliases such as `кооп`, `кооператив`, `coop`, and `co-op`
normalize to `co-op`. A general dislike with reason `multiplayer` may normalize
to the broad target `multiplayer`, which matches every multiplayer target but
not `single-player`.

Rules:

- only a `dislike` with negative polarity, confidence at least `0.70`, and a
  supported canonical mode creates an actionable preference adjustment;
- a low-confidence or unsupported classification remains stored and visible as
  "сохранён без влияния на профиль";
- a preference adjustment is always a soft score penalty and never an
  eligibility exclusion;
- missing game-mode features produce no mode penalty;
- the game on which feedback was submitted receives the existing direct-game
  feedback penalty, not an additional category penalty from its own adjustment;
- other games matching the canonical mode receive the category penalty;
- repeated feedback for the same mode does not stack without bound;
- the user can disable an applied adjustment and trigger a new profile/ranking
  rebuild.

Positive mode rules, hard category exclusions, automatic library hiding, and
arbitrary free-form rule dimensions are outside this iteration.

## 4. Structured Classification Contract

Bump the feedback prompt and schema versions. Extend the structured output with
an optional normalized target:

```json
{
  "taxonomy": "multiplayer",
  "polarity": "negative",
  "confidence": 0.92,
  "actionable": true,
  "preference": {
    "dimension": "mode",
    "target": "co-op",
    "direction": "avoid"
  },
  "evidence_keys": ["feedback-text"]
}
```

`preference` is either `null` or a strict object. Dimension, target, and
direction are schema enums; runtime validation repeats the allowlist check.
Unknown targets, unknown evidence keys, duplicate citations, malformed output,
refusals, and incomplete responses never create adjustments.

An AI-derived adjustment is created only when every cross-field invariant is
true: the source action is `dislike`, `actionable=true`, `polarity=negative`,
`confidence >= 0.70`, `preference` is non-null,
`preference.dimension=mode`, `preference.direction=avoid`, and its target is in
the canonical mode allowlist. The taxonomy and target must also form an
allowlisted pair: `taxonomy=single-player` is valid only with
`target=single-player`, while `taxonomy=multiplayer` is valid with the broad
`multiplayer` target or any canonical multiplayer target. Every other pairing
is treated as contradictory output and creates no adjustment. The deterministic
predefined-reason fallback follows its separate allowlisted mapping and does
not fabricate an AI classification.

The model sees only the submitted feedback text and existing opaque evidence
key. It does not receive SteamID, user UUID, recommendation UUID, job payload,
session data, or API credentials. Raw prompts and responses are never logged.

When OpenAI is unavailable, a selected predefined `multiplayer` reason can
create the deterministic broad `multiplayer` adjustment. Free text alone is
saved but does not create a semantic rule without a valid classification.

## 5. Canonical Game Modes

Ranking mode evidence is derived deterministically from normalized Steam
categories, not from the title and not from LLM-generated game modes. The
canonicalizer maps supported category aliases to the fixed vocabulary; `co-op`
matches direct cooperative categories plus their online/local aliases.
The broad `multiplayer` adjustment matches all multiplayer canonical modes.

Add `game_modes(game_id, mode, source, source_content_hash,
last_successful_fetch_at)` with a primary key on `(game_id, mode, source)`.
In this version `source` is constrained to `steam_category`. Refreshing Steam
categories replaces only the successfully refreshed projection; a failed Steam
call preserves the last successful modes. Store tags are deliberately excluded
because the current Steam adapter has no supported tag source and the catalog
design forbids HTML scraping and third-party providers.

The same canonical vocabulary is shared by Steam metadata normalization and
feedback validation; it must not have separate frontend/backend lists. AI game
features may still mention modes for explanations, but they are not accepted as
ranking-rule evidence. If Steam categories do not establish a mode, the factor
remains zero. This makes the rule "never infer a mode from the title alone"
enforceable.

## 6. Persistence

Add a numbered migration; do not rewrite applied migrations.

### `preference_adjustments`

One row per actionable feedback-derived adjustment:

- `id`;
- `user_id`;
- `source_feedback_id`, unique;
- `source_game_id`;
- `dimension`, constrained to `mode`;
- `target`, constrained to the canonical mode vocabulary plus `multiplayer`;
- `direction`, constrained to `avoid`;
- `confidence` in `[0,1]`;
- `strength` in `[0,1]`, initially `1`;
- `active`;
- nullable `disable_rebuild_job_id`;
- nullable `disable_previous_ranking_version`;
- `created_at`, `updated_at`, and nullable `disabled_at`.

Multiple active rows for the same `(user_id, dimension, target)` are permitted
for auditability. Ranking aggregates them with `max(confidence * strength)`, so
repetition strengthens evidence only up to `1` and never adds unbounded score
penalties.

Extend `feedback_classifications` with nullable
`preference_dimension`, `preference_target`, and `preference_direction`
columns using the same database checks as the structured output. It remains the
immutable classification record and lets a resumed workflow reproduce the
adjustment without calling the model again. Disabling an adjustment changes
only `preference_adjustments.active`; it does not delete the original feedback
or classification.

Remove classification polarity from the direct per-game like/dislike count in
`profile.LoadSignals`. Direct actions contribute once. Structured adjustments
are loaded through their own repository method and factor path.

## 7. Ranking

Add `negativeModePreference` to `Factors` and
`negativeModePreferenceWeight` to `Weights`. Bump the ranking algorithm version.

For each candidate:

1. Load its canonical modes.
2. Select active mode adjustments whose target matches the candidate. Candidate
   retrieval loads these adjustments once per ranking build rather than issuing
   one query per game.
3. Ignore an adjustment when `candidate.game_id == adjustment.source_game_id`;
   the direct game feedback already represents that event.
4. Set `negativeModePreference` to the maximum matching
   `confidence * strength`, clamped to `[0,1]`.
5. Apply the default weight `-5`.

One confident co-op rule therefore lowers another co-op game's score by up to
5 points. This is calibrated against the current non-wishlist score range so a
matching candidate remains meaningfully ranked rather than collapsing to zero.
It never marks the game ineligible. Direct dislike keeps its current
per-game penalty and repeated direct dislikes may still exclude that exact
game. Negative semantic similarity remains an independent, softer signal from
the negative profile vector.

The factor and weight are saved in `recommendation_factors`, included in the
ranking build key together with a deterministic digest of active adjustments,
exposed through the existing API types, and translated into
human-readable evidence such as "Вы просили реже предлагать кооперативные
игры". Internal enum names and numeric weights are not shown to the user.

## 8. Feedback Workflow

The feedback workflow becomes:

```text
save immutable feedback event
  -> classify free text / deterministic predefined reason
  -> save classification
  -> upsert preference adjustment when actionable
  -> rebuild profile
  -> rebuild and atomically publish recommendations
  -> return ranking diff plus applied-adjustment summary
```

Retries remain idempotent by feedback event. A resumed workflow cannot create a
second adjustment for the same `source_feedback_id`.

The diff response adds:

```json
{
  "adjustment": {
    "id": "opaque-id",
    "label": "Кооперативные игры",
    "effect": "понижаем в рекомендациях",
    "active": true
  }
}
```

Extend authenticated `GET /api/profile` with:

```json
{
  "adjustments": [
    {
      "id": "opaque-id",
      "dimension": "mode",
      "target": "co-op",
      "label": "Кооперативные игры",
      "effect": "понижаем в рекомендациях",
      "confidence": 0.92,
      "active": true,
      "createdAt": "2026-10-18T00:00:00Z"
    }
  ]
}
```

Only the authenticated user's active adjustments are returned by default.

Add authenticated, CSRF-protected
`DELETE /api/preference-adjustments/{id}`. The first call atomically marks the
adjustment inactive, enqueues the versioned profile/ranking rebuild with
idempotency key `adjustment:<id>:disable:v1`, stores its job ID, and returns
HTTP `202`:

```json
{
  "adjustmentId": "opaque-id",
  "rebuildJobId": "opaque-job-id",
  "previousRankingVersion": 7,
  "alreadyInactive": false
}
```

A repeated call is idempotent and returns the stored job/version with
`alreadyInactive=true`. Missing or foreign adjustment IDs return `404` without
revealing ownership. The frontend polls the existing ranking-diff endpoint with
the returned version and job ID. Both `disable_rebuild_job_id` and
`disable_previous_ranking_version` are stored in the same transaction as the
state change so the response remains stable after the rerank completes.
Disabling never deletes the feedback event.

## 9. Feedback in Calendar and Lists

Use one reusable `FeedbackLauncher` and one accessible feedback dialog for all
compact presentations. The launcher receives recommendation ID, ranking
version, game name, CSRF token, and the existing ranking-update callback.

The dialog contains:

- game name and poster;
- `Нравится`, `Не моё`, and `Напомнить` actions;
- the existing predefined reason selector;
- the existing 500-character comment field and Unicode counter;
- submit, pending, success, classification/adjustment, retry, and error states.

`Напомнить` follows the existing contract: it submits immediately and never
shows or sends a reason/comment. Only `Нравится` and `Не моё` reveal the reason
and free-text fields.

Calendar integration:

- each exact-date grid event has an `Оценить` button that opens the dialog;
- each exact-date list card has the same launcher;
- every month/quarter/year/Coming Soon/TBA bucket item has the same launcher;
- submitting feedback refreshes the overview/calendar after the ranking job
  succeeds while preserving visible month, grid/list mode, source filter,
  precision filter, and score filter;
- the ranking diff is announced through the existing live status region;
- duplicate clicks reuse the event idempotency key and disable submit while the
  request is active.

The dense month grid does not embed a full textarea in each table cell. Its
button opens the shared dialog, keeping keyboard navigation and cell layout
usable.

## 10. Posters and Steam Links in Calendar and Lists

Every exact-date event, exact-date list card, and imprecise bucket item renders:

- the existing `coverImageUrl` poster when available;
- the existing deterministic cover fallback when the image is absent or fails;
- the game name as a direct `storeUrl` link;
- an accessible external-link label;
- `target="_blank"` with `rel="noopener noreferrer"`.

Layout rules:

- month-grid poster: compact 32×48 px, fixed aspect ratio, lazy-loaded;
- list/bucket poster: 48×72 px, fixed aspect ratio, lazy-loaded;
- long names wrap without expanding the calendar column beyond its grid width;
- failed images switch to the fallback without removing the Steam link;
- posters are decorative when the adjacent linked game name supplies the
  accessible label; otherwise their alt text is `Обложка игры <name>`.

The API already returns `coverImageUrl`, `storeUrl`, and `steamAppId`; no new
remote image proxy or database column is required for this UI change.

## 11. Adjustment Visibility

The profile panel gains a compact "Учитываем ваши корректировки" section that
lists active adjustments with their human labels and a `Не учитывать` action.
Only active rules affect ranking. Disabled rules remain available in local
diagnostics but are not shown in the default profile panel.

The feedback success state names the applied correction. It must not merely say
"отзыв учтён" when the classification was low-confidence or unsupported; in
that case it explicitly says the comment was saved without changing the
preference rules.

## 12. Failure and Privacy Behavior

- Feedback-event persistence succeeds or fails before any UI success message.
- Classification failure preserves the direct like/dislike and skips the
  adjustment; it does not block the ranking rebuild.
- Adjustment persistence and disable operations are transactional and
  user-scoped.
- A failed rebuild preserves the last published ranking and lets the user retry
  status polling.
- Calendar controls require the same authenticated session and CSRF protection
  as dashboard feedback.
- Recommendation IDs, adjustment IDs, feedback text, and classification output
  are never written to general logs.
- External Steam links use only the server-generated store URL for a positive
  numeric AppID; upstream-provided arbitrary URLs are not rendered.

## 13. Acceptance Criteria

- `dislike + "не люблю коопы"` with confidence at least `0.70` creates one
  active `mode=co-op, direction=avoid` adjustment.
- Replaying the workflow does not duplicate the adjustment.
- The source game receives one direct negative feedback contribution, not an
  additional category contribution from its own adjustment.
- Another game with canonical `co-op` mode receives a deterministic soft
  penalty of at most 5 points, remains eligible, and remains above zero for the
  representative baseline score fixture used by ranking tests.
- A non-co-op game and a game with unknown modes receive no mode penalty.
- Multiple matching feedback events aggregate with `max`, not an unbounded sum.
- Disabling the adjustment removes the mode penalty after an atomic rerank while
  retaining the original feedback history.
- Low-confidence, neutral, unknown-target, refused, malformed, and unavailable
  AI classifications create no adjustment.
- Deterministic predefined multiplayer feedback works when OpenAI is disabled.
- Calendar grid events, exact-date list cards, and every imprecise release bucket
  expose feedback through the shared accessible dialog.
- A successful calendar/list feedback submission refreshes ranking data without
  resetting the current calendar view and filters.
- Every calendar/list item displays a poster or deterministic fallback and a
  valid direct Steam link.
- Image failure, long names, keyboard-only interaction, dialog focus return,
  mobile layout, duplicate submission, pending rebuild, and stale ranking states
  have frontend tests.
- Backend unit/integration tests cover classifier validation and cross-field
  invariants, canonical Steam category aliases,
  idempotent persistence, factor calculation, no double-counting, adjustment
  disable, build-key/version changes, and authorization/CSRF boundaries.

## 14. Explicit Non-Goals

- Hard-excluding all co-op or multiplayer games.
- Treating low-confidence free text as an actionable preference.
- Supporting positive mode rules or arbitrary dimensions in the first version.
- Editing or deleting immutable feedback history.
- Replacing deterministic ranking with the classifier or an LLM.
- Adding posters to email digests in this change.
- Proxying, downloading, or permanently caching Steam poster images.

