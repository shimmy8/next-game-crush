# Grounded Recommendation Explanations Design

**Date:** 2026-10-18
**Status:** Approved
**Related MVP design:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`
**Related presentation design:** `docs/superpowers/specs/2026-10-18-discovery-wishlist-presentation-design.md`
**Delivery:** May land after or together with discovery/wishlist presentation. This
design supersedes the explanation details in section 8 of that document where
the two differ.

## 1. Objective

Make every displayed recommendation explanation specific enough to answer
"why this game?" without inventing facts, exposing internal scores, or relying
on OpenAI availability.

Success means that two games with different supporting data normally show
different reasons; wishlist membership is presented as context rather than
proof of taste; unsupported novelty is not claimed; deterministic fallback is
useful on its own; and a card never repeats the same explanation as both a
sentence and a bullet list.

The ranking remains deterministic. OpenAI may summarize supplied evidence but
does not select candidates, assign scores, add facts, or determine ordering.

## 2. Observed Baseline

The latest local published ranking contained 42 recommendations:

- 6 stored a generated explanation and 36 used deterministic fallback;
- the 42 rows contained only 10 distinct explanation strings and 7 distinct
  cited-evidence sets;
- most wishlist games selected the same `wishlist`, semantic-similarity, and
  novelty labels;
- most discoveries selected the same semantic-similarity, novelty, and tag
  labels.

This follows directly from the current implementation:

- `novelty` is set to `1` for every candidate;
- wishlist membership contributes a fixed large value;
- public evidence keeps the three largest numeric factor contributions;
- each factor maps to one generic Russian sentence, with no matching tags,
  mechanics, or anchor games;
- OpenAI receives only those generic sentences and therefore can only
  paraphrase them;
- any model, citation, validation, timeout, or HTTP failure is silently
  replaced with fallback;
- the card renders the fallback sentence and then repeats its ingredients in
  the expanded evidence list.

The problem is therefore the evidence contract and presentation model, not
only prompt wording or model quality.

## 3. Product Principles

### Specific before fluent

An awkward but concrete deterministic reason is more useful than a polished
generic statement. AI prose is optional presentation over already useful
facts.

### Explain affinity, not bookkeeping

Wishlist membership, release precision, catalog source, and confidence are
context. They may affect sections, badges, filtering, or uncertainty, but they
do not by themselves explain why the user may enjoy a game.

### Claims require displayable evidence

A factor may contribute to ranking without being eligible for explanation. If
the application cannot name the supporting tags, mechanics, prior games, or
feedback, it must not turn the number into a broad user-facing claim.

### Preserve the ranking snapshot

Explanation evidence is derived and stored when a ranking version is built.
Reads do not reinterpret an old recommendation using a newer profile or newer
Steam snapshot.

### Failure is visible but not disruptive

OpenAI failure never blocks publication and never removes a valid
deterministic explanation. The system records only an allowlisted failure
category, never raw prompts, bodies, keys, IDs, or provider error text.

## 4. Explanation Evidence Model

Introduce a private persisted `GroundingEvidence` representation and keep the
public API DTO deliberately smaller.

Persisted evidence contains:

- stable opaque `id` within the recommendation;
- allowlisted `kind`;
- Russian `publicText` ready for deterministic display;
- normalized `strength` used only for deterministic selection;
- private source references needed to prove how the fact was derived.

Public evidence contains only:

```json
{
  "id": "playtime-anchor-1",
  "kind": "playtime_anchor",
  "text": "По темам близка к SOMA, которой вы уделили много времени"
}
```

The API never exposes source UUIDs, factor values, weights, embedding values,
raw feature JSON, exact playtime, prompt text, or upstream response data.

The existing `recommendations.explanation_evidence` JSONB column stores the
new versioned JSON shape, so the evidence change itself needs no migration.
The explanation input content hash includes the evidence schema version and
prompt version; the schema becomes `recommendation-explanation-v2`, forcing
generic v1 inputs to be regenerated on the next ranking rebuild.

## 5. Eligible Evidence Kinds

Evidence is derived only from saved local data available at build time.

### Feature overlap

`feature_overlap` names up to three normalized themes, mechanics, modes, or
tone values shared by the candidate and positively weighted profile sources.
Example: `Совпадают темы и механики: psychological horror, exploration`.

Values are selected deterministically by weighted source support, then
case-insensitive value and raw value. Empty, excessively long, duplicate, or
unsafe values are omitted. This is the preferred explanation for the existing
tag-affinity factor.

### Similar-game anchor

`similar_game` names one or at most two owned, recently played, wishlisted, or
positively rated games that materially support the positive profile and are
closest to the candidate using stored embeddings. Example: `По описанию и
механикам близка к SOMA и Signalis`.

An anchor is eligible only when both embeddings use the configured model and
the source game has a positive profile weight. A negative, hidden, or disliked
game cannot become a positive anchor. Ties use normalized game name, raw name,
then game UUID.

### Playtime anchor

`playtime_anchor` names the highest-overlap owned game whose recency-adjusted
playtime contribution is material. It may say the user spent substantial time
with that game but does not expose minutes or call an old lifetime record a
current interest.

### Recent-activity anchor

`recent_activity_anchor` names a matching game only when its saved
`last_played_at` or recent-minutes signal satisfies the profile algorithm's
current recency window. Missing recency data cannot produce a recency claim.

### Achievement anchor

`achievement_anchor` names a matching owned game only when the saved
achievement ratio and feature overlap both materially contribute. It must not
claim completion or mastery from an unbounded raw achievement count.

### Direct positive feedback

`positive_feedback` is allowed when prior feedback exists for the same game.
The copy states that the user previously marked this game positively. Feedback
about another game supports its own anchor or profile weight; it is not
presented as direct feedback on the candidate.

### Wishlist context

`wishlist_context` may state that the game is already in the wishlist, but it
is contextual evidence and never occupies a substantive "why you may like it"
slot. The existing `♥ В желаемом` badge is the primary presentation.

If a wishlist item has no substantive affinity evidence, use watchlist copy:
`Вы уже добавили эту игру в желаемое — следим за датой выхода.` Do not show a
"why you may like it" heading and do not call OpenAI.

## 6. Novelty Policy

The current constant `novelty = 1` does not prove that a candidate adds
anything new. This design therefore removes novelty from explanation evidence
and from OpenAI inputs immediately.

The stored factor and current ranking weight remain unchanged in this feature
to avoid silently changing scores or minimum-score behavior while the
discovery/wishlist ranking design is in progress. A later ranking-version
change may redefine novelty from a documented diversity metric or set its
weight to zero. Until then, no user-facing copy may claim novelty.

## 7. Deterministic Selection and Copy

Build all eligible evidence first, then select at most two substantive items.
Selection is deterministic:

1. discard evidence whose contribution is below `0.5` ranking points;
2. discard evidence below 5% of the strongest substantive contribution;
3. prefer different evidence kinds over two variants of the same kind;
4. sort by contribution descending, kind priority, public text, then evidence
   ID;
5. keep at most two substantive items.

These thresholds affect explanation eligibility only, not ranking.

The deterministic explanation is composed from the selected concrete facts,
not from factor labels. It is one or two short Russian sentences and must not
include internal values. Examples:

- `По темам игра близка к SOMA: psychological horror и exploration.`
- `Напоминает Signalis по атмосфере и механикам; к тому же похожие игры вы
  запускали недавно.`
- `Рекомендация основана на вашем игровом профиле, но конкретных совпадений
  пока недостаточно.`

The last neutral sentence is used only when no concrete evidence survives. It
must not be followed by fabricated or generic evidence bullets.

## 8. Optional AI Summary

OpenAI receives the game name and only the selected public evidence objects.
It may produce a concise Russian summary and cite one or both supplied IDs.
It may not introduce new game properties, genres, comparisons, dates, scores,
or user-history claims.

AI output is accepted only when:

- the text passes the existing Russian and technical-leak checks;
- every citation is supplied, unique, and substantive;
- at least one selected evidence item is cited;
- the output does not imply wishlist membership is evidence of enjoyment;
- the output contains no unsupported novelty claim.

Invalid output falls back to the deterministic explanation built from the
same selected evidence. The fallback must remain equally specific even when
all OpenAI calls fail.

This feature does not add retries inside the ranking transaction or delay
ranking publication for optional prose. Temporary provider retry policy may be
designed separately after failure categories are observable.

## 9. Failure Status and Diagnostics

Add a numbered migration with two fields on `recommendations`:

- `explanation_source`: `ai`, `deterministic`, or `none`;
- nullable `explanation_failure_reason` using the allowlist below.

`explanation_source` is constrained and non-null. The migration backfills
`ai` where the existing `explanation_generated` flag is true and
`deterministic` otherwise, then uses `deterministic` as the insert default
during the compatibility cycle. New builds use `none` only when no substantive
or wishlist evidence exists, or when legacy content is discarded and cannot be
reconstructed from the immutable snapshot.

Allowed failure reasons are:

- `disabled`;
- `rate_limited`;
- `timeout`;
- `provider_unavailable`;
- `refusal`;
- `invalid_output`;
- `invalid_citation`;
- `unsafe_text`;
- `unknown`.

`unknown` is a last-resort classification, not a place to store an error
message. Successful AI output clears the reason. A deliberate no-AI path such
as membership-only wishlist copy uses source `deterministic` with a null
failure reason.

The recommendation rebuild result and sanitized job diagnostics expose only
aggregate counts by source and allowlisted failure reason. Per-game failure
details remain internal. No raw exception, prompt, provider body, SteamID, or
recommendation payload is logged or returned.

Keep `generatedExplanation` in the public JSON for one compatibility cycle,
derived as `explanation_source == "ai"`. Add `explanationSource` as the new
typed field and migrate the frontend to it.

## 10. Read-Time Compatibility

New recommendations display their stored versioned evidence. The read path
validates the persisted shape, allowed kinds, text safety, and AI citations;
it does not regenerate rich evidence from current profile state.

For legacy rows:

- safe generated explanations with valid existing citations remain readable;
- technical, malformed, or generic deterministic evidence is discarded;
- the UI receives neutral deterministic copy with an empty evidence list;
- the next normal ranking rebuild replaces legacy content with v2 evidence.

A read must never fail merely because explanation JSON is old or malformed.
It also must not turn a failed Steam or OpenAI refresh into an empty successful
profile or overwrite the last published ranking.

## 11. Recommendation Card UX

The card has one explanation surface, not two equivalent ones.

### AI source

Show the generated summary as the main paragraph. A collapsed `На чём
основано` control may list the concrete cited evidence because it adds
traceability beyond the prose. The source label reads `AI-сводка по вашим
игровым данным`.

### Deterministic source

Show the selected concrete evidence directly as one or two visible reasons.
Do not also render a sentence that concatenates the same reasons. The source
label reads `По вашим игровым данным`.

### No substantive evidence

Show neutral recommendation copy with no evidence drawer. A membership-only
wishlist item shows the watchlist copy from section 5 and keeps its wishlist
badge. A truly evidence-free item has source `none`; the neutral copy is UI
copy and is not presented as a grounded deterministic explanation.

Compact cards continue omitting explanation details. Accessible headings and
controls remain available on full cards. The UI must not expose failure
reasons; those belong to authenticated diagnostics.

## 12. Data Flow and Component Boundaries

The build flow becomes:

1. retrieve candidates and the saved profile inputs used by ranking;
2. calculate deterministic factors and score as today;
3. derive a versioned `GroundingEvidence` snapshot from the same saved inputs;
4. select at most two substantive evidence items;
5. build the deterministic explanation;
6. optionally request an AI summary from exactly those items;
7. persist factors, selected evidence, explanation source, safe failure
   category, input hash, and text before atomically publishing the ranking;
8. serve the stored snapshot through the public DTO;
9. render exactly one explanation treatment based on its source.

Keep responsibilities separate:

- recommendation retrieval loads candidate and profile-source data;
- an evidence builder derives and selects grounded facts;
- the AI adapter validates structured output and classifies safe failures;
- the repository persists immutable ranking-version presentation data;
- the read service performs compatibility validation, not re-ranking;
- the card chooses presentation by source without reconstructing evidence.

The evidence builder must be independently unit-testable and must not depend on
HTTP, React, or a live OpenAI client.

## 13. Testing Strategy

### Backend unit tests

- different feature/anchor inputs produce different concrete evidence;
- constant novelty never appears in public evidence or AI input;
- wishlist context does not displace substantive evidence;
- membership-only wishlist items skip OpenAI and use watchlist copy;
- selection thresholds, kind diversity, and all tie-breaks are deterministic;
- old playtime without recency does not become recent-interest copy;
- negative or disliked games cannot become positive anchors;
- unsafe feature values and internal identifiers never reach public text;
- AI refusal, invalid citation, unsafe text, timeout, rate limit, and disabled
  capability each produce specific deterministic fallback and the correct
  allowlisted reason.

### Repository and API contract tests

- versioned evidence round-trips without a migration rewrite of old JSON;
- an old or malformed evidence object degrades to safe neutral copy;
- explanation source and compatibility boolean agree;
- no private grounding references, factor values, raw errors, or prompts appear
  in JSON;
- latest published ranking remains visible when a new build fails;
- aggregate diagnostics contain counts only and never identify games or users.

### Frontend tests

- deterministic cards show concrete reasons once;
- AI cards show prose plus an optional evidence disclosure;
- neutral and membership-only states have no misleading "why" disclosure;
- source labels use user-facing copy rather than implementation jargon;
- compact cards, wishlist badges, feedback, Steam links, and cover fallbacks
  continue to work;
- legacy unsafe explanation strings remain suppressed.

### Integrated verification

Use a fixture ranking with at least two games that have intentionally different
anchors and feature overlap. Prove their rendered explanations differ when the
AI client is disabled and remain grounded when the AI client succeeds. Include
all-fallback, mixed-source, malformed-output, wishlist-only, and no-evidence
browser states.

Backend changes run focused Go tests with `GOWORK=off` and `-race`. Frontend
changes run test, typecheck, lint, and build. Because this crosses persistence,
API, ranking workflow, and browser presentation, final verification uses
`make verify` with the isolated test database.

## 14. Acceptance Criteria

- Two fixture candidates with different supporting data render different
  deterministic explanations without OpenAI.
- Every displayed reason names a concrete tag/mechanic, anchor game, direct
  feedback fact, or truthful wishlist context.
- No explanation claims novelty while novelty remains the constant current
  factor.
- Wishlist membership never consumes a substantive affinity slot and never by
  itself produces a "why you may like it" heading.
- Deterministic cards do not repeat the same reasons in a paragraph and list.
- AI receives only selected public evidence and cannot add uncited facts.
- A complete OpenAI outage still produces useful specific explanations and
  does not block ranking publication.
- Safe aggregate diagnostics distinguish disabled, rate-limited, timed-out,
  unavailable, refused, invalid, unsafe, and unknown outcomes.
- Legacy technical or malformed content degrades safely until the next rebuild.
- Go and TypeScript contracts, normalization, tests, and browser fixtures agree
  on explanation source and evidence shape.
- Ranking remains deterministic, release precision is unchanged, and no new
  persistent service is introduced.

## 15. Explicit Non-Goals

- Letting an LLM assign scores, reorder recommendations, or choose candidates.
- Building a general natural-language recommendation engine.
- Showing numeric factor contributions or embedding similarity to the user.
- Exposing raw prompts, provider bodies, source UUIDs, SteamIDs, or exact
  playtime in explanations or diagnostics.
- Retrying ambiguous or permanent AI failures indefinitely.
- Redefining the ranking novelty metric in this delivery.
- Changing release-date parsing, calendar buckets, feedback taxonomy, catalog
  discovery, or multi-user scope.
