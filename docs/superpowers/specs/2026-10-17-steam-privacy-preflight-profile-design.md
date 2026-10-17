# Steam Privacy Preflight and Profile Identity Design

**Date:** 2026-10-17
**Status:** Ready for user review
**Related design:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`

## 1. Objective

Before starting a Steam synchronization, verify that the signed-in user's
profile exposes the minimum data required by Next Game Crush. At the same time,
refresh the user's Steam display name and avatar for use in the application.

The preflight must distinguish a confirmed privacy restriction from a temporary
upstream failure. It must never erase the last successful profile identity or
turn an unavailable source into a successful empty dataset.

## 2. Required and Optional Sources

A synchronization may start only when both of these conditions hold:

- the Steam profile is public according to `ISteamUser/GetPlayerSummaries/v2`;
- `IPlayerService/GetOwnedGames/v1` exposes Game Details for the allowlisted
  user.

The wishlist, recent activity, and achievements remain optional sources. Their
privacy settings or temporary failures do not block the workflow; they are
reported through source availability and the sync diagnostics.

The application only supports the existing single allowlisted Steam user.

## 3. API Contract

Add an authenticated, CSRF-protected endpoint:

```text
POST /api/sync/preflight
```

Successful transport returns HTTP 200 with a bounded response:

```json
{
  "canSync": true,
  "profileVisibility": "public",
  "gameDetailsVisibility": "public",
  "reason": null,
  "displayName": "Player",
  "avatarUrl": "https://..."
}
```

The response fields are:

- `canSync`: whether the client may immediately call the existing sync start
  endpoint;
- `profileVisibility`: `public`, `private`, or `unknown`;
- `gameDetailsVisibility`: `public`, `private`, or `unknown`;
- `reason`: a stable safe code such as `profile_private`,
  `game_details_unavailable`, or `check_failed`; never a raw upstream error;
- `displayName` and `avatarUrl`: nullable last-known or newly refreshed profile
  identity.

A confirmed private profile returns HTTP 200 with `canSync: false`. Steam does
not always distinguish private Game Details from a genuinely empty library, so
an empty but structurally valid Owned Games response is
`gameDetailsVisibility: unknown` with `reason: game_details_unavailable`; it is
not asserted to be private. A timeout, HTTP rejection, malformed response, or
other transport-level failure returns a non-success HTTP status and
`reason: check_failed`; it must not be described as private.

If `canSync` is false or the preflight fails, no synchronization job is
enqueued. On success, the frontend calls the existing `POST /api/sync`
endpoint. The sync endpoint remains independently authenticated and does not
trust client-supplied preflight fields.

## 4. Steam Integration and Classification

Add a typed client operation for `ISteamUser/GetPlayerSummaries/v2`. Normalize
only the fields required for privacy and identity. Use `GetOwnedGames/v1` as a
real access probe because a public community profile does not by itself prove
that Game Details are public.

Keep two independent concepts:

- **visibility**: `public`, `private`, or `unknown`, representing privacy
  knowledge;
- **availability**: `available`, `unavailable`, or `request_failed`,
  representing the outcome of an import attempt.

Only a Steam response that positively demonstrates a privacy restriction may
produce `private`. An ambiguous empty Owned Games response produces `unknown`
and blocks this MVP's sync because it cannot provide the required library data.
Network failures, rate limits, unexpected status codes, and malformed payloads
produce `unknown` plus a failed check.

## 5. Persistence and Profile Projection

The existing `steam_accounts.display_name` and `steam_accounts.avatar_url`
columns hold the profile identity. A successful Player Summaries response may
update them. A failed or incomplete response preserves the last-known-good
values.

Add a numbered migration to store nullable source visibility on
`user_source_states`. Do not rewrite an applied migration. Source availability
continues to describe import outcomes; visibility records the last confirmed
privacy result.

Extend `GET /api/me` with nullable `displayName` and `avatarUrl` fields. Keep the
Go response types, frontend API types, normalization, and contract tests in
sync.

Accept avatar URLs only when they use HTTPS and a known Steam CDN host. The
frontend uses a local neutral fallback when the URL is absent or the image
fails to load.

## 6. User Experience

Starting a sync first shows a short privacy-check state. If the preflight
succeeds, synchronization begins without another confirmation.

If access is private, show:

- which required setting is closed;
- a concise instruction to open Steam Profile privacy settings and set both
  the profile and Game Details to Public;
- a note that individually hidden games remain unavailable;
- a `Проверить снова` action.

The instruction must not promise that wishlist or achievement data will become
available, because those sources can fail independently.

Display the Steam name and avatar in the authenticated profile area. Identity
decoration must not block navigation or synchronization when a previously
saved value is available.

## 7. Security and Diagnostics

Use the server-side authenticated account only. Do not accept a SteamID in the
request body or query. Do not log SteamID, display name, avatar URL, upstream
payloads, callback data, API keys, or raw errors.

Diagnostics may record only stable stage and outcome codes, request/workflow
correlation identifiers generated by the application, and bounded durations.

## 8. Verification and Acceptance Criteria

Tests must cover:

- public profile and accessible Game Details permit synchronization;
- confirmed private profile and unavailable or unknown Game Details block job
  creation without misclassifying an ambiguous empty library as private;
- timeout, HTTP error, and malformed data are `unknown`/`check_failed`, not
  `private`;
- optional source failures do not block synchronization;
- last-known display name and avatar survive a failed refresh;
- unsafe avatar URLs are rejected and the fallback renders;
- `/api/me` and preflight backend/frontend contracts remain aligned;
- logs and error responses contain no identifiers, personal data, secrets, or
  upstream bodies.

The feature is accepted when a real local account can be checked, a private
configuration receives actionable instructions without enqueuing work, and a
public configuration proceeds to the existing synchronization workflow.
