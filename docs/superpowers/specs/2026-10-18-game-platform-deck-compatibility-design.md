# Game Platform and Steam Deck Compatibility Design

## Goal

Show each recommended game's supported desktop platforms and Steam Deck
compatibility, and let the user filter the release calendar by either signal.
The feature must keep the local MVP usable when Steam's optional Deck endpoint
is unavailable and must never replace a previously successful compatibility
observation with failed or malformed data.

## Scope

The feature covers catalog games that can appear in recommendations:

- Import Windows, macOS, and Linux support from the existing Steam Store
  `appdetails` JSON response.
- Import the Steam Deck compatibility category from the public Steam Store
  `saleaction/ajaxgetdeckappcompatibilityreport` JSON response.
- Preserve and expose compatibility independently from recommendation ranking.
- Display platform and Deck state on recommendation cards.
- Filter all calendar buckets in both month and list views.

The feature does not infer the user's device, change deterministic ranking,
introduce ProtonDB or another external provider, or scrape Steam HTML.

## Upstream Contracts

### Desktop platforms

`GameDetails` gains a platform value parsed from the `platforms` object in the
existing Store `appdetails` response:

```json
{"windows": true, "mac": false, "linux": true}
```

The object is required for a successful platform observation. A missing object
or non-boolean member makes platform compatibility unavailable for that fetch;
it does not turn every platform into `false`.

### Steam Deck

The Steam client gains a separate Deck compatibility request. The adapter maps
Steam's numeric `resolved_category` as follows:

| Steam category | Application status |
| --- | --- |
| `3` | `verified` |
| `2` | `playable` |
| `1` | `unsupported` |
| `0` | `unknown` |

A missing or unrecognized category is a malformed response, not `unknown`.
The endpoint is public JSON but is not part of Steam's documented stable Web
API. It therefore remains isolated behind the Steam client boundary and is
covered by sanitized fixtures. A Deck request failure must not fail the normal
catalog refresh.

## Data Model

Add a numbered migration that creates `game_compatibility`:

- `game_id uuid PRIMARY KEY REFERENCES games(id) ON DELETE CASCADE`
- `windows boolean NOT NULL`
- `mac boolean NOT NULL`
- `linux boolean NOT NULL`
- `platforms_last_successful_fetch_at timestamptz NOT NULL`
- `deck_status text NOT NULL DEFAULT 'unknown'` constrained to
  `verified`, `playable`, `unsupported`, or `unknown`
- `deck_availability text NOT NULL DEFAULT 'unknown'`
- `deck_last_attempt_at timestamptz`
- `deck_last_successful_fetch_at timestamptz`

A row exists only after a successful platform observation. Platform upserts
replace the three platform booleans and platform timestamp without changing the
Deck fields. Deck updates set `deck_availability` and the attempt timestamp;
they replace `deck_status` and the success timestamp only after a successful,
valid response. Failed, unavailable, or malformed Deck responses retain the
last successful status.

Existing games are not backfilled with guessed values. Their compatibility is
unknown until the next successful catalog refresh.

## Catalog Flow

For each known catalog game:

1. Fetch and validate the existing Store details response.
2. Persist normal catalog data, release information, and supported platforms.
3. Fetch Deck compatibility independently.
4. Persist a successful Deck category, or record its safe availability state
   while retaining the prior category.

The required Store details behavior remains unchanged: its failure follows the
existing catalog failure path. Deck compatibility is optional enrichment, so
its failure is recorded but does not abort `RefreshKnown` and does not prevent
recommendations from being published.

Diagnostics and logs must use safe operation names and fixed error categories;
they must not expose upstream response bodies.

## Recommendation API

Every recommendation may include:

```json
{
  "platforms": {"windows": true, "mac": false, "linux": true},
  "deckCompatibility": "verified"
}
```

`platforms` is omitted when no successful platform observation exists.
`deckCompatibility` is always present and defaults to `unknown` when there is
no successful Deck observation or Steam reports category `0`.

Both `Latest` and `Get` load the same compatibility contract. Compatibility is
presentation metadata and does not alter the ranking score, evidence, build
key, or algorithm version.

## User Interface

Recommendation cards display a compact compatibility row:

- One badge for every supported desktop platform: Windows, macOS, Linux.
- `Платформы неизвестны` when `platforms` is absent.
- One Steam Deck badge with localized states:
  - `Steam Deck: проверено`
  - `Steam Deck: можно играть`
  - `Steam Deck: не поддерживается`
  - `Steam Deck: неизвестно`

The Deck badge uses distinct visual treatments but never relies on color alone.
Compact cards retain the compatibility row so filtered list results remain
understandable.

The calendar filter bar gains two single-select controls:

- Platform: `Все`, `Windows`, `macOS`, `Linux`, `Неизвестно`.
- Steam Deck: `Все`, `Проверено`, `Можно играть`, `Не поддерживается`,
  `Неизвестно`.

Platform `Неизвестно` matches only recommendations whose `platforms` property
is absent. A selected concrete platform matches when its boolean is true. The
Deck filter compares the exact status. These filters combine with each other
and all existing source, date-precision, and score filters using logical AND.
They apply to all six release buckets and both month and list views.

## Error Handling and Freshness

- Store details failure preserves the current catalog snapshot and current
  compatibility row through the existing failure path.
- Deck HTTP, timeout, rate-limit, unavailable, or malformed-response failures
  preserve `deck_status` and `deck_last_successful_fetch_at`.
- A successful Steam category `0` is stored as `unknown` and advances the Deck
  success timestamp.
- Games without compatibility data remain visible when filters are set to
  `Все`; they are excluded by concrete platform filters and included by the
  corresponding `Неизвестно` filters.
- Frontend normalization treats a missing or invalid Deck value as `unknown`
  and does not invent supported platforms.

## Verification

Backend coverage must prove:

- Strict parsing of the Store platform object.
- Mapping of all four Deck categories and rejection of unknown numeric values.
- Independent platform and Deck persistence.
- Preservation of the last successful Deck category on failure.
- Matching compatibility fields from recommendation `Latest` and `Get`.
- Migration upgrade and downgrade behavior and generated sqlc consistency.

Frontend coverage must prove:

- Every platform/Deck presentation state, including absent platform data.
- Every new filter option across all release buckets.
- Logical AND composition with the existing filters.
- Equivalent filtering in month and list views.
- Safe normalization of missing or invalid compatibility fields.

Run focused Go tests with `GOWORK=off` and `-race`, the frontend test,
typecheck, lint, and build commands, then `make verify` because this is a
cross-cutting persistence and API-contract change.
