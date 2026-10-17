# Next Game Crush: Local Single-User MVP Design

**Date:** 2026-10-17  
**Status:** Ready for user review  
**Related idea:** `docs/steam-release-recommender-idea.md`

## 1. Objective

Build a locally runnable, end-to-end prototype of Next Game Crush for one
allowlisted Steam user. The prototype imports real public Steam data, builds an
explainable preference profile, recommends upcoming Steam releases, presents
them in a calendar, learns from explicit feedback, and sends a test digest to a
local mailbox.

The first implementation must demonstrate the product loop rather than
production scale:

```text
Steam profile
  -> import library, activity, wishlist, and selected achievements
  -> build deterministic and semantic preference signals
  -> retrieve and rank upcoming releases
  -> explain recommendations with OpenAI
  -> display calendar
  -> collect feedback
  -> update the next ranking
  -> deliver a test digest to Mailpit
```

The service runs locally today. Its boundaries must permit a later move to the
cloud without replacing the domain model.

## 2. Product Scope

### Included

- Steam OpenID login for one allowlisted SteamID.
- Public Steam library, total playtime, recently played games, and wishlist.
- Achievement summaries for the 30 highest-signal games.
- A bounded upcoming-release catalog covering the next 90 days.
- Automatic inclusion of unreleased wishlist games even when they are absent
  from the seed catalog.
- Automatic refresh of known game metadata and release dates through
  Valve-hosted endpoints.
- Structured game features and player-profile summary generated with OpenAI.
- OpenAI embeddings stored and searched with PostgreSQL `pgvector`.
- Deterministic, versioned ranking that uses semantic similarity as one factor.
- A month calendar and a week-oriented agenda view.
- Explanations grounded in stored recommendation factors.
- `Like`, `Dislike`, `Remind me`, predefined feedback reasons, and optional
  free-text feedback.
- Re-ranking after feedback.
- A weekly-style test digest delivered to Mailpit, including signed feedback
  links.
- Diagnostics for synchronization, background jobs, Steam, OpenAI, and email.
- Deletion of imported user data.

### Excluded from this iteration

- Multiple users and general public registration.
- A complete automatically discovered index of all Steam releases.
- External email delivery.
- Scheduled event notifications and release-date change emails.
- Production monitoring, autoscaling, backups, and cloud deployment.
- Cross-store recommendations.

## 3. External Data Policy

The application may call Valve-hosted endpoints that are either documented in
Steamworks or advertised by Steam's live supported-API discovery, even when an
individual Steamworks documentation page is absent. It must not scrape HTML,
use Steam cookies, request Steam credentials or Steam Guard codes, run a friend
bot, or use a user's personal Web API key.

Primary Steam integrations:

- Steam OpenID for proof of SteamID ownership.
- `IPlayerService/GetOwnedGames` for the public library and playtime.
- `IPlayerService/GetRecentlyPlayedGames` for recent activity.
- `ISteamUserStats/GetPlayerAchievements` for selected games.
- `IWishlistService/GetWishlist` for a public wishlist.
- `IStoreService/GetAppList` and bounded Valve Store metadata lookups for known
  catalog entries.

Steam OpenID proves identity but does not grant access to private data. A
private profile, private Game Details, private wishlist, an unsupported game,
or a missing achievement schema is represented as unavailable data rather than
as an application failure.

## 4. Catalog Strategy

The MVP does not crawl the entire Steam catalog.

The repository contains a reviewed `catalog_seed.json` of known upcoming AppIDs
whose releases fall within the next 90 days. A catalog job enriches and refreshes
those AppIDs through Valve-hosted Store endpoints. Every unreleased AppID found
in the user's wishlist is added automatically and refreshed by the same job.

The catalog stores source, fetch time, content hash, release-date precision, and
raw release-date text. Each observed release-date change creates a history row;
the current value is a projection, not an in-place loss of history.

Supported date precision:

- exact day;
- month;
- quarter;
- year;
- `coming_soon`;
- unknown.

An imprecise date is never converted into a fabricated exact day. Exact dates
appear on calendar days; month, quarter, year, and unknown windows appear in
separate calendar buckets.

## 5. Architecture

The application is a modular monolith in a monorepository.

### Runtime components

- **Frontend:** React, TypeScript, Vite, and Tailwind CSS.
- **Backend:** Go HTTP service using `chi`.
- **Database:** PostgreSQL with `pgvector`.
- **Data access:** `pgx` and `sqlc`; SQL migrations managed by `goose`.
- **Background work:** a PostgreSQL-backed job table processed by a worker in
  the Go service.
- **AI:** OpenAI Responses API for structured generation and the Embeddings API
  for semantic vectors.
- **Local email:** SMTP delivery to Mailpit.
- **Local orchestration:** Docker Compose for PostgreSQL, Mailpit, and the
  application.

The production frontend build is static. The Go service serves those assets and
the JSON API from one origin. During development, Vite proxies API requests to
Go.

The Go binary supports API and worker roles internally. Local development runs
both roles in one process; a cloud deployment may split them without moving
business logic.

### Backend modules

- `auth`: OpenID flow, sessions, allowlist, and CSRF protection.
- `steam`: typed clients, response normalization, privacy handling, and rate
  limiting.
- `catalog`: seed import, metadata refresh, release-date history, and date
  precision.
- `profile`: deterministic signals, semantic vectors, and profile versions.
- `recommendations`: candidate retrieval, scoring, factor snapshots, and
  ranking versions.
- `feedback`: explicit reactions, free-text classification, and preference
  updates.
- `llm`: OpenAI prompts, schemas, caching, refusals, and fallbacks.
- `notifications`: digest assembly, signed actions, and SMTP delivery.
- `jobs`: enqueueing, leases, retries, idempotency, and diagnostics.

## 6. Data Model

The minimum persistent entities are:

- `users` and `sessions`;
- `steam_accounts`;
- `games` and `game_features`;
- `game_embeddings`;
- `game_release_dates`;
- `user_games`;
- `wishlist_entries`;
- `achievement_summaries`;
- `preference_profiles` and `profile_embeddings`;
- `recommendations` and `recommendation_factors`;
- `feedback_events` and `feedback_classifications`;
- `notification_preferences`;
- `email_deliveries`;
- `sync_jobs` and `job_attempts`.

Every source-derived row records its source and last successful fetch time.
Recommendations record the algorithm version, profile version, score, model
IDs, and a complete factor snapshot. LLM-derived records also store an input
content hash so unchanged inputs are not processed twice.

## 7. Synchronization and Job Flow

After login, the backend enqueues one idempotent synchronization workflow:

1. Fetch the library and playtime.
2. Fetch recent activity.
3. Fetch the wishlist.
4. Upsert wishlist games into the bounded catalog.
5. Select up to 30 achievement targets using playtime, recency, and data
   availability; fetch each independently.
6. Refresh known catalog metadata and dates.
7. Generate or refresh structured game features.
8. Generate missing embeddings.
9. Rebuild the deterministic and semantic player profile.
10. Generate candidate recommendations and factor snapshots.
11. Generate grounded explanations for top recommendations.

Jobs use a unique idempotency key, a lease, a bounded retry count, and an error
record. Workers claim jobs using `FOR UPDATE SKIP LOCKED`. Failure in one
optional stage does not roll back earlier successful stages.

## 8. AI and Embeddings

### Models

- Text generation default: `gpt-6-luna`, configurable with `OPENAI_MODEL`.
- Embeddings default: `text-embedding-3-small`, configurable with
  `OPENAI_EMBEDDING_MODEL`.

### Structured generation

The Responses API uses strict Structured Outputs for three operations:

1. Extract semantic game features from available descriptions, genres, tags,
   and categories.
2. Summarize the player profile from aggregated evidence.
3. Produce recommendation explanations from saved factors.

The model never invents or changes release dates and never determines the final
numeric score. Explanations separate facts, inferences, and uncertainty.

If OpenAI is unavailable, ranking continues with existing features and vectors.
The UI displays a deterministic explanation assembled from recommendation
factors. Invalid, refused, or incomplete model output is recorded and does not
replace the previous valid result.

### Semantic retrieval

Each game is embedded from a canonical text containing its name, description,
genres, tags, categories, and structured features. The content hash determines
whether a new embedding is required.

The positive profile vector is a weighted centroid of highly played, recently
played, wishlisted, and liked games. The negative profile vector is a weighted
centroid of disliked games. Candidate semantic affinity is calculated as:

```text
positive cosine similarity
  - weighted negative cosine similarity
```

`pgvector` performs nearest-neighbor retrieval. The selected games and their
stored evidence are then supplied to the text model to generate grounded
explanations. This is the MVP's retrieval-augmented generation path.

## 9. Deterministic Ranking

The first ranking version uses normalized factors and configurable weights:

```text
semantic_similarity       25
wishlist                   30
tag_affinity               15
playtime_affinity          10
achievement_affinity        5
recent_activity_affinity    5
positive_feedback          20
novelty                     5
negative_feedback         -30
date_uncertainty           -5
```

The raw weighted value is normalized to a display score from 0 to 100. Candidate
retrieval and final scoring remain separate: embeddings find semantically
plausible candidates, while the deterministic formula orders them.

A single dislike applies to the candidate and softly penalizes similar games.
A hard exclusion requires an explicit manual exclusion or repeated consistent
feedback. Ranking weights and algorithm version are saved with each result.

## 10. Feedback

A recommendation accepts:

- `like` or `dislike`;
- an optional predefined reason;
- optional free text of at most 500 characters;
- `remind_me` as a separate action.

Free text is retained and classified by OpenAI into a fixed taxonomy such as
genre, setting, multiplayer, single-player, difficulty, length, early access,
already known, not now, or other. Structured output includes polarity and
confidence. A low-confidence classification is visible but does not
automatically modify the profile.

After feedback, the service stores the original recommendation context,
updates explicit preferences, rebuilds profile vectors, and recomputes
recommendations. The UI identifies added, removed, and reordered results.

## 11. User Interface

### Login

- Explain imported data and privacy boundaries.
- Start Steam OpenID.
- Reject a SteamID that does not match `ALLOWED_STEAM_ID`.
- Explain private or partially unavailable profile data.

### Synchronization and profile

- Show workflow stages and current progress.
- Show errors per optional data source.
- Show last successful synchronization time.
- Show an AI profile summary with expandable source signals.

### Calendar

- Month grid for exact dates.
- Week-oriented agenda view.
- Separate buckets for month, quarter, year, `Coming Soon`, and unknown windows.
- Filters for wishlist/recommendations, minimum score, and date precision.
- Recommendation cards with artwork, date, wishlist marker, score, confidence,
  explanation, evidence, and feedback actions.

### Diagnostics and settings

- Start synchronization and re-ranking manually.
- Send a test digest.
- Link to Mailpit.
- Show Steam, OpenAI, SMTP, and job status.
- Delete imported data.

## 12. API Surface

The initial HTTP surface is:

- `GET /auth/steam`
- `GET /auth/steam/callback`
- `POST /auth/logout`
- `GET /api/me`
- `POST /api/sync`
- `GET /api/sync/{id}`
- `GET /api/profile`
- `GET /api/calendar`
- `GET /api/recommendations/{id}`
- `POST /api/recommendations/{id}/feedback`
- `POST /api/recommendations/{id}/reminder`
- `POST /api/digests/test`
- `DELETE /api/data`
- `GET /api/jobs`
- `GET /api/health`
- `GET /actions/feedback/{token}`

Mutating API calls require an authenticated session and CSRF token. Email action
tokens are signed, purpose-bound, recommendation-bound, and expire.

## 13. Failure Handling

- A repeated synchronization is idempotent and does not create duplicates.
- Temporary Steam failure retains the last successful snapshot.
- Unavailable achievements remove only that optional signal.
- OpenAI failure retains previous features and explanations or uses a template.
- A missing exact date preserves the known release window.
- A failed job records diagnostics and can be retried safely.
- A later-private profile marks imported data stale rather than deleting it.
- An email delivery has an idempotency key and cannot be sent twice by a retry.

The UI distinguishes unavailable, stale, incomplete, and failed states.

## 14. Security and Privacy

- Secrets are provided through environment variables and never persisted in
  PostgreSQL or committed to Git.
- Sessions use `HttpOnly` and `SameSite` cookies; secure cookies are enabled
  whenever the configured base URL uses HTTPS.
- The OpenID response, return URL, nonce/state, and allowlisted SteamID are
  verified.
- Mutating browser requests require CSRF protection.
- Only aggregated signals and necessary game metadata are sent to OpenAI.
- Steam API keys, session cookies, email addresses, and internal identifiers
  are excluded from model input.
- Imported data can be deleted from the diagnostics/settings screen.

## 15. Local Development Configuration

Required environment variables:

```text
APP_BASE_URL=http://localhost:8080
ALLOWED_STEAM_ID=<64-bit SteamID>
STEAM_WEB_API_KEY=<server-side key>
OPENAI_API_KEY=<server-side key>
OPENAI_MODEL=gpt-6-luna
OPENAI_EMBEDDING_MODEL=text-embedding-3-small
DATABASE_URL=postgres://...
SMTP_URL=smtp://mailpit:1025
EMAIL_ACTION_SECRET=<random secret>
SESSION_SECRET=<random secret>
```

`.env.example` contains names and safe local defaults but no secrets. If the
Steam OpenID provider cannot return to a localhost URL in the user's setup,
`APP_BASE_URL` may point to a temporary HTTPS tunnel without changing the app.

## 16. Testing

- Unit tests cover scoring, date precision, profile weighting, feedback rules,
  signatures, and idempotency keys.
- Steam and OpenAI adapter tests use recorded, sanitized fixtures and make no
  network calls.
- Repository integration tests run against PostgreSQL with `pgvector`.
- Frontend tests cover calendar date buckets, filters, and feedback state.
- One end-to-end smoke test covers login bypass in test mode, synchronization
  fixtures, recommendations, feedback, re-ranking, and Mailpit delivery.
- The verification command runs Go tests and static checks, frontend typecheck
  and lint, database migrations, and a production build.

## 17. Acceptance Criteria

The local MVP is complete when:

1. A documented command starts the application, PostgreSQL, and Mailpit.
2. The allowlisted user can complete Steam OpenID login.
3. Public library, playtime, recent activity, wishlist, and selected
   achievements are imported and visible through synchronization status.
4. The catalog includes the seed releases and unreleased wishlist items.
5. Game features, embeddings, profile vectors, and the AI profile summary are
   generated and cached.
6. The month grid and agenda show upcoming releases without inventing exact
   dates for imprecise windows.
7. Every recommendation exposes a deterministic score, confidence, evidence,
   and grounded explanation.
8. Like, dislike, predefined reasons, and free text are stored; a new ranking
   visibly reflects relevant feedback.
9. A test digest arrives in Mailpit and its signed feedback link works.
10. Repeating synchronization and digest jobs creates no duplicate records or
    messages.
11. Simulated Steam and OpenAI failures leave the last successful experience
    usable.
12. The complete automated verification command passes.

## 18. Follow-Up Sequence

After the local MVP:

1. Stabilize API adapters using observed real-account responses and rate limits.
2. Add cloud deployment, external email, secrets management, backups, and
   monitoring.
3. Generalize authentication and data isolation for multiple users.
4. Expand automated release discovery beyond the bounded seed catalog.
5. Add scheduled digests, reminders, date-change detection, and event emails.
6. Add recommendation-quality analytics, offline evaluation, and controlled
   weight experiments.

