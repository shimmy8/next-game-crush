# Discovery and Wishlist Presentation Design

**Date:** 2026-10-18  
**Status:** Ready for user review  
**Related catalog design:** `docs/superpowers/specs/2026-10-18-steam-catalog-discovery-design.md`  
**Delivery:** May be implemented independently against the current seed catalog.

## 1. Objective

Make Next Game Crush visibly recommend games the user has not already chosen.
Wishlist games remain useful release-watch candidates, but they must not occupy
the entire primary recommendation surface merely because wishlist membership
adds a large deterministic score bonus.

Success means the dashboard leads with non-wishlist discoveries matched to the
profile, shows wishlist games in a clearly labeled secondary section, and keeps
the calendar capable of displaying both sets with explicit filters.

## 2. Current Baseline

The current pipeline retrieves all eligible catalog games and always includes
wishlist games. The default score gives wishlist membership 30 points. The
dashboard then globally sorts by score and renders only the first seven cards.

Observed local distribution:

- 20 non-wishlist and 22 wishlist recommendations exist in the latest ranking;
- all seven dashboard cards are wishlist games;
- non-wishlist scores range from 8.76 to 18.47;
- wishlist scores average 40.53 and reach 45.86.

The lower calendar contains both sets, but the primary experience looks like a
wishlist mirror. This is a presentation and candidate-product problem, not an
absence of embeddings.

## 3. Product Model

The product exposes two intentional collections from one immutable ranking
version:

### Discoveries

Games not present in the user's wishlist or owned library. They answer: "What
should I discover next?" They are ordered by the existing deterministic score
with wishlist contribution fixed at zero by definition.

### Wishlist watchlist

Unreleased games already present in the user's wishlist. They answer: "What is
happening with games I already follow?" They remain scored by affinity,
feedback, recency, and date confidence, but wishlist membership is a category
marker rather than a 30-point advantage over discoveries.

The database may continue storing one recommendation set and complete factor
snapshots. Separation happens in the read model and API, not by duplicating
recommendation records.

## 4. Ranking Policy

Change the default weights in a new algorithm version:

- `wishlist` weight becomes `0` for ordering;
- wishlist remains a boolean factor and explanation fact;
- all other deterministic and semantic factors retain their current weights in
  this feature;
- feedback continues to affect both collections;
- owned, released, hidden, and repeatedly disliked games remain excluded.

This avoids a quota hack in a single global list. A wishlist game can be a
strong wishlist candidate, but it cannot outrank an unseen game solely because
the user already wishlisted it.

The algorithm version and build key must change. Existing published rankings
remain readable; a new ranking is built and atomically published.

## 5. API Read Model

Add authenticated `GET /api/recommendations/overview`. The frontend loads it in
parallel with the existing `GET /api/profile`; there is no pre-existing combined
dashboard endpoint. Keep `GET /api/calendar` for one compatibility cycle,
derived from the same published ranking, and migrate the dashboard to the new
route before removing the old route in a later change.

The overview response is:

```json
{
  "discoveries": {
    "featured": null,
    "items": [],
    "total": 0
  },
  "wishlist": {
    "items": [],
    "total": 0,
    "status": "stale",
    "availability": "unavailable",
    "lastSuccessfulFetchAt": null,
    "degradedReason": "private_or_unavailable"
  },
  "calendar": {
    "exactDates": [],
    "month": [],
    "quarter": [],
    "year": [],
    "comingSoon": [],
    "tba": []
  },
  "catalogStatus": {
    "status": "backfilling",
    "activeDiscoveredGames": 0,
    "lastSuccessfulScanAt": null,
    "backfillCompletedAt": null
  }
}
```

Rules:

- `featured` is `Recommendation | null` and, when present, is the
  highest-scoring discovery rather than the highest global score;
- `discoveries.items` contains the next six discoveries;
- `wishlist.items` contains up to six wishlist games;
- `wishlist.status` is one of `ready`, `stale`, `unavailable`, or `unknown`;
  `availability` is the normalized Steam source availability,
  `lastSuccessfulFetchAt` is a nullable RFC 3339 value, and
  `degradedReason` is a nullable allowlisted safe reason;
- an available empty wishlist is represented by `status=ready`, zero items,
  and a non-null successful-fetch timestamp; preserved items after a private or
  failed refresh are represented as `stale` and are never indistinguishable
  from a current empty result;
- totals describe the complete published ranking, not only rendered cards;
- calendar contains the complete published ranking and retains current release
  buckets; "complete" means the configured maximum of 100 non-wishlist
  candidates plus every eligible wishlist candidate, not every game in the
  global catalog;
- `catalogStatus.status` is one of `ready`, `backfilling`, `stale`,
  `unavailable`, or `disabled`; timestamps are nullable RFC 3339 values;
- `catalogStatus` exposes aggregate coverage only and never the internal AppID
  cursor or a user identifier;
- items are not duplicated between collections;
- an empty discovery set stays empty and shows honest coverage copy; wishlist
  items are never relabeled as discoveries.

Backend construction keeps ordering deterministic: score descending, then
case-insensitive name, original name, and game UUID.

## 6. Dashboard UX

The main column becomes:

1. **"Новая находка для вас"** — featured non-wishlist recommendation.
2. **"Ещё новые игры"** — up to six additional discoveries.
3. **"Уже в вашем желаемом"** — a visually secondary watchlist section with
   up to six cards.

Wishlist cards keep the `♥ В желаемом` badge. Discovery cards do not show a
negative badge such as "не в желаемом"; novelty is conveyed by the section.

If discoveries are empty:

- show "Пока не нашли новых игр вне вашего списка желаемого";
- explain whether catalog discovery is still running, stale, or complete for
  the current bounded scan;
- provide synchronization as the only existing action;
- continue showing the wishlist section and calendar normally.

The right rail shows catalog coverage next to recommendation/profile status:
active discovered games, last catalog update, and whether backfill is still in
progress. It does not expose internal cursor values.

## 7. Calendar and Filters

The calendar continues to merge discoveries and wishlist games because release
planning is the purpose of that view.

Replace the single `Только желаемое` checkbox with a three-state source filter:

- `Все`;
- `Новые для меня`;
- `В желаемом`.

The existing minimum-score and date-precision filters remain. Counts on release
buckets reflect the active filters. Changing dashboard collection limits does
not remove items from the calendar response.

## 8. Explanations and Feedback

Discovery explanations emphasize profile evidence: semantic similarity,
genres/tags, playtime, achievements, recent activity, and feedback. They must
not say that an unseen game was wishlisted.

Wishlist explanations may state that the game is already in the wishlist. This
membership evidence is selected independently of its zero ranking contribution
and therefore does not disappear when the weight becomes zero. When at least
one substantive affinity signal exists, the explanation must include one of
semantic similarity, genres/tags, playtime, achievements, recent activity, or
feedback. With membership alone, the UI uses deterministic watchlist copy and
does not ask the model to invent a "why you may like it" claim.

Like, dislike, remind-me, predefined reasons, and free text work unchanged in
both collections. Hard exclude is not a browser action in this iteration.
After reranking, the ranking diff may move games
within their collection or remove them; it cannot move a game between
collections unless the underlying wishlist membership changes.

## 9. Compatibility and Failure Handling

- API types change in Go and TypeScript together, with contract tests.
- The calendar remains present so existing date-bucket components can be
  reused.
- Stored recommendation rows and factor snapshots require no migration.
- If the discovery catalog is unavailable, the last published discovery list
  remains visible with stale status.
- If there are no non-wishlist candidates, the dashboard does not promote a
  wishlist game into the discovery hero.
- If wishlist access is private/unavailable, preserve the last successful
  wishlist snapshot and label its freshness; do not silently reclassify those
  games as discoveries.
- A failed new ranking never replaces the previously published ranking.

## 10. Acceptance Criteria

- With at least seven eligible discoveries and seven wishlist games, the hero
  and six primary cards are all non-wishlist discoveries.
- Wishlist games appear in the separately labeled watchlist section.
- Wishlist membership contributes zero ordering points in the new algorithm
  version but remains present in factor snapshots and UI badges.
- A high-affinity discovery can rank first without competing against an
  automatic 30-point wishlist bonus.
- Empty discoveries render an honest empty state rather than a wishlist
  fallback.
- Calendar source filters produce mutually correct discovery/wishlist sets and
  preserve date-precision filters.
- The API returns deterministic collection order and complete totals.
- Feedback reranking updates both collections without cross-classification.
- Private/stale wishlist data is represented explicitly and never converted
  into a successful empty snapshot.
- Frontend unit, API contract, Go race, and browser smoke tests cover mixed,
  discovery-only, wishlist-only, empty, and stale-source states.

## 11. Explicit Non-Goals

- Removing wishlist games from recommendations or the calendar.
- Replacing deterministic ranking with an LLM.
- Adding infinite scroll, pagination, or user-configurable section sizes.
- Changing the recommendation horizon or release-date precision rules.
- Reweighting semantic, tag, playtime, achievement, recency, or feedback
  factors beyond setting wishlist ordering weight to zero.
- Automatically adding discovered games to the user's Steam wishlist.

