# Next Game Crush MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a locally runnable, single-user Next Game Crush prototype that imports real public Steam data, recommends upcoming releases with deterministic and semantic signals, learns from feedback, renders a calendar, and delivers a test digest to Mailpit.

**Architecture:** A Go modular monolith serves a React/Vite frontend and REST API, runs PostgreSQL-backed background jobs, and integrates with Steam, OpenAI, and SMTP through isolated adapters. PostgreSQL with `pgvector` is the only persistent service; the ranking engine remains deterministic while OpenAI supplies structured features, embeddings, profile summaries, feedback classification, and grounded explanations.

**Tech Stack:** Go 1.27.2, `chi`, `pgx`, `sqlc`, `goose`, PostgreSQL + `pgvector`, React, TypeScript, Vite, Tailwind CSS, pnpm, OpenAI Responses and Embeddings APIs, Mailpit, Docker Compose.

**Spec:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`

## Global Constraints

- Support exactly one user, identified by `ALLOWED_STEAM_ID` after Steam OpenID verification.
- Use Valve-hosted APIs only; do not scrape HTML or request Steam credentials, cookies, Steam Guard codes, or a user's personal API key.
- Import public library, playtime, recent activity, wishlist, and achievements for at most 30 high-signal games.
- Limit the catalog to a reviewed 90-day seed plus all unreleased wishlist games; refresh known entries automatically.
- Preserve exact-day, month, quarter, year, `coming_soon`, and unknown date precision without inventing a day.
- Use `gpt-6-luna` by default for structured text generation and `text-embedding-3-small` for embeddings; both model IDs remain configurable.
- Store vectors in PostgreSQL `pgvector`; do not introduce a separate vector database or Redis.
- Keep numeric ranking deterministic and versioned; LLM output never assigns the final score or release date.
- Accept optional free-text feedback up to 500 characters and treat low-confidence classification as non-actionable.
- Keep secrets in environment variables and out of Git, PostgreSQL, logs, and model inputs.
- Preserve the last successful Steam and OpenAI-derived state when an optional external call fails.
- Run locally with the installed Go 1.27.2, Node 26, pnpm 11, Docker, and Docker Compose toolchains.

### Compatibility and execution contracts

- Resolve current stable releases of `chi`, `pgx`, `goose`, and `pgvector-go` against Go 1.27.2, then commit the exact resolved versions in `go.mod` and `go.sum`.
- Run Go commands with `GOWORK=off` because an unrelated parent `go.work` exists outside this repository.
- Pin the `sqlc` tool version in project tooling so local and CI generation are reproducible.
- Pin PostgreSQL to `pgvector/pgvector:0.8.7-pg16-bookworm`.
- Use `APP_BASE_URL=http://localhost:8080` as the canonical origin. Vite is a development-only proxy for `/api`, `/auth`, and `/actions`.
- `OPENAI_API_KEY` is optional at startup so deterministic ranking remains available; authentication and signing secrets remain required.
- All stored embeddings are exactly 1536 dimensions. Request that dimension and reject an incompatible model response before persistence.
- Host-side database tests use `TEST_DATABASE_URL`, a published port, a readiness check, and an isolated disposable test database; they never recreate the application database.

## Review Focus

- Private or partially private Steam data must become an explicit unavailable/stale state while preserving the last successful snapshot; pin this in Task 4 adapter tests and Task 5 workflow tests.
- Repeated sync and digest requests must not duplicate rows, jobs, or feedback; SMTP acceptance followed by process failure must become an explicit ambiguous delivery rather than an automatic resend. Pin this in Tasks 2, 5, 9, and 10.
- Imprecise and malformed release dates must never be displayed as fabricated exact days; pin this in Task 5 and Task 8.
- OpenAI refusal, invalid schema, timeout, or missing credentials must preserve deterministic recommendations and a template explanation; pin this in Task 6 and Task 7.
- Empty vectors, sparse profiles, repeated dislikes, and low-confidence free text must not corrupt or overfit ranking; pin this in Task 7 and Task 9.

---

## Phase 1: Runnable Foundation

### Task 1: Bootstrap the monorepository and local runtime

**Files:**
- Create: `go.mod`
- Create: `cmd/next-game-crush/main.go`
- Create: `internal/app/app.go`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `internal/httpapi/router.go`
- Create: `internal/httpapi/health.go`
- Create: `internal/httpapi/health_test.go`
- Create: `web/package.json`
- Create: `web/pnpm-lock.yaml`
- Create: `web/tsconfig.json`
- Create: `web/vite.config.ts`
- Create: `web/index.html`
- Create: `web/src/main.tsx`
- Create: `web/src/App.tsx`
- Create: `web/src/styles.css`
- Create: `web/src/test/setup.ts`
- Create: `web/vitest.config.ts`
- Create: `docker-compose.yml`
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `.gitignore`
- Create: `.env.example`
- Create: `Makefile`

**Interfaces:**
- Produces: `config.Load() (config.Config, error)` with the environment names from the spec.
- Produces: `app.New(config.Config) (*app.App, error)` and `(*app.App).Run(context.Context) error`.
- Produces: `httpapi.NewRouter(httpapi.Dependencies) http.Handler`.
- Produces: `GET /api/health -> {"status":"ok"}`.

- [x] **Step 1: Write failing configuration and health tests**

Add tests named `TestLoadRejectsMissingRequiredSecrets`, `TestLoadAllowsMissingOpenAIKey`, `TestLoadUsesDocumentedModelDefaults`, and `TestHealthReturnsOK`. Assert that missing required authentication/signing variables produce named validation errors, a missing OpenAI key produces a disabled capability without startup failure, defaults equal `gpt-6-luna` and `text-embedding-3-small`, and health returns HTTP 200 with JSON status `ok`.

- [x] **Step 2: Run the focused tests and verify failure**

Run: `GOWORK=off go test ./internal/config ./internal/httpapi`

Expected: FAIL because the packages and functions do not exist.

- [x] **Step 3: Implement the minimal Go service and React shell**

Use pinned `chi` for routing. Make the frontend display the product name, API health, and a disabled login action that Task 3 will activate. Configure Vite to proxy `/api`, `/auth`, and `/actions` to the canonical Go origin `http://localhost:8080`. Define `dev`, `build`, `test`, `typecheck`, and `lint` scripts in `web/package.json`; configure Vitest with jsdom and Testing Library while excluding Playwright specs.

- [x] **Step 4: Add local services and developer commands**

Define pinned PostgreSQL with `pgvector`, Mailpit, and the application in Compose. Publish a host test port and define `TEST_DATABASE_URL`. Add `make dev`, `make test`, `make build`, and `make down`; list required secret variable names with empty values in `.env.example`. Treat `OPENAI_API_KEY` as optional and expose disabled AI capability through configuration.

- [x] **Step 5: Verify the foundation**

Run: `GOWORK=off go test ./internal/config ./internal/httpapi && pnpm --dir web install && pnpm --dir web run build && docker compose config`

Expected: tests PASS, frontend build succeeds, and Compose configuration validates.

- [x] **Step 6: Commit**

```bash
git add go.mod go.sum cmd internal/config internal/app internal/httpapi web docker-compose.yml Dockerfile .dockerignore .gitignore .env.example Makefile
git commit -m "chore: bootstrap Next Game Crush runtime"
```

### Task 2: Create the database schema, typed queries, and job queue

**Files:**
- Create: `db/migrations/00001_initial.sql`
- Create: `db/queries/users.sql`
- Create: `db/queries/games.sql`
- Create: `db/queries/sync.sql`
- Create: `db/queries/jobs.sql`
- Create: `sqlc.yaml`
- Create: `internal/store/store.go`
- Create: `internal/store/migrations.go`
- Create: `internal/store/store_test.go`
- Create: `internal/jobs/queue.go`
- Create: `internal/jobs/queue_test.go`
- Create: `internal/jobs/worker.go`
- Create: `internal/jobs/worker_test.go`
- Modify: `internal/app/app.go`
- Generate: `internal/store/sqlc/*`

**Interfaces:**
- Consumes: `config.Config.DatabaseURL` from Task 1.
- Produces: `store.Open(context.Context, string) (*store.Store, error)`.
- Produces: `jobs.Queue.Enqueue(ctx, jobs.Spec) (uuid.UUID, error)`.
- Produces: `jobs.Queue.Claim(ctx, workerID string, lease time.Duration) (*jobs.Lease, error)`, where the lease contains a unique fencing token.
- Produces: `jobs.Queue.Renew(ctx, lease jobs.Lease, duration time.Duration) error`.
- Produces: `jobs.Queue.Complete(ctx, lease jobs.Lease) error` and `Fail(ctx, lease jobs.Lease, cause error, retryAt time.Time) error`; stale tokens must fail.
- Produces: `jobs.Worker.Register(kind string, handler jobs.Handler) error` and `Run(context.Context) error` with graceful shutdown.

- [x] **Step 1: Write failing migration and queue integration tests**

Add `TestMigrateCreatesRequiredTables`, `TestEnqueueIsIdempotent`, `TestClaimSkipsLeasedJob`, `TestExpiredLeaseCanBeReclaimed`, `TestStaleLeaseCannotCompleteOrFail`, `TestRetryBackoffAndAttemptExhaustion`, and `TestParallelClaimsAreUnique`. Assert the presence of every entity named in the spec, a repeated idempotency key returning the same job ID, and fencing-token ownership for every state change.

- [x] **Step 2: Run tests against PostgreSQL and verify failure**

Run: `docker compose up -d db && make wait-test-db && TEST_DATABASE_URL=postgres://next_game_crush_test:next_game_crush_test@localhost:55432/next_game_crush_test?sslmode=disable GOWORK=off go test ./internal/store ./internal/jobs`

Expected: FAIL because migrations and queue do not exist.

- [x] **Step 3: Add schema and typed SQL**

Enable the `vector` extension. Use `github.com/google/uuid` at domain boundaries and define explicit repository conversions for sqlc/pgx UUID values, including nullability. Map vectors through `pgvector-go` and add an insert/read/cosine-query round trip. Use UUID primary keys for application entities, Steam AppID as `BIGINT`, SteamID as text, `vector(1536)` for embeddings, JSONB only for source snapshots and algorithm metadata, and unique constraints for sync/delivery idempotency keys.

- [x] **Step 4: Implement queue leasing**

Claim jobs transactionally with `FOR UPDATE SKIP LOCKED` and issue a new fencing token on every claim. Renewal, completion, and failure updates match job ID, running status, worker ID, and fencing token. A failed job increments attempts, records the error, and moves to `retryable` or `dead` according to `max_attempts`. Wire the worker into `app.App` and stop it through the parent context.

- [x] **Step 5: Generate queries and verify**

Run: `make sqlc && TEST_DATABASE_URL=postgres://next_game_crush_test:next_game_crush_test@localhost:55432/next_game_crush_test?sslmode=disable GOWORK=off go test ./internal/store ./internal/jobs`

Expected: PASS, including idempotency and competing-worker tests.

- [x] **Step 6: Commit**

```bash
git add db sqlc.yaml internal/store internal/jobs internal/app go.mod go.sum Makefile docker-compose.yml
git commit -m "feat: add persistence and PostgreSQL job queue"
```

### Task 3: Implement single-user Steam OpenID authentication

**Files:**
- Create: `internal/auth/openid.go`
- Create: `internal/auth/service.go`
- Create: `internal/auth/session.go`
- Create: `internal/auth/service_test.go`
- Create: `internal/httpapi/auth_handlers.go`
- Create: `internal/httpapi/auth_handlers_test.go`
- Modify: `internal/httpapi/router.go`
- Modify: `db/queries/users.sql`
- Modify: `web/src/App.tsx`
- Create: `web/src/api/client.ts`
- Create: `web/src/pages/LoginPage.tsx`

**Interfaces:**
- Consumes: `config.Config.AppBaseURL`, `AllowedSteamID`, and `SessionSecret`.
- Produces: `auth.Service.Begin(returnPath string) (redirectURL string, error)`.
- Produces: `auth.Service.Complete(ctx, callback url.Values) (auth.Session, error)`.
- Produces: `auth.Middleware(next http.Handler) http.Handler` and `auth.RequireCSRF(next http.Handler) http.Handler`.
- Produces: `GET /auth/steam`, `GET /auth/steam/callback`, `POST /auth/logout`, and `GET /api/me`.

- [x] **Step 1: Write failing authentication tests**

Add tests named `TestCompleteAcceptsVerifiedAllowlistedSteamID`, `TestCompleteRejectsDifferentSteamID`, `TestCompleteRejectsInvalidReturnTo`, `TestCompleteRejectsUnsolicitedOrReplayedState`, `TestCompleteRejectsClaimedIDProviderMismatch`, `TestLoginRotatesSession`, `TestSessionExpires`, `TestLogoutInvalidatesSession`, `TestSessionCookieIsHTTPOnlySameSite`, `TestHTTPSCookieIsSecure`, `TestMutationRejectsMissingOrWrongCSRF`, and `TestFixtureAuthCannotStartOutsideTestHarness`.

- [x] **Step 2: Verify the tests fail**

Run: `go test ./internal/auth ./internal/httpapi -run 'Auth|Steam|Session|CSRF|Complete'`

Expected: FAIL because authentication is not implemented.

- [x] **Step 3: Implement OpenID verification and allowlisting**

Validate the provider response server-side, extract the 64-bit SteamID from the claimed ID, compare it exactly with `ALLOWED_STEAM_ID`, upsert the user/account, rotate the session ID after login, and validate the configured realm and return URL.

- [x] **Step 4: Add the login UI**

Show what data will be imported, a Steam login button, private-data limitations, authenticated identity, and logout. Fetch `/api/me` on application startup.

- [x] **Step 5: Verify authentication behavior**

Run: `go test ./internal/auth ./internal/httpapi && pnpm --dir web run build`

Expected: PASS and frontend production build succeeds.

- [x] **Step 6: Commit**

```bash
git add internal/auth internal/httpapi db/queries/users.sql web/src
git commit -m "feat: add allowlisted Steam OpenID login"
```

## Phase 2: Real Steam Data and Catalog

### Task 4: Build typed Steam and Store API adapters

**Files:**
- Create: `internal/steam/types.go`
- Create: `internal/steam/client.go`
- Create: `internal/steam/http_client.go`
- Create: `internal/steam/http_client_test.go`
- Create: `internal/steam/testdata/owned_games.json`
- Create: `internal/steam/testdata/recent_games.json`
- Create: `internal/steam/testdata/wishlist.json`
- Create: `internal/steam/testdata/achievements.json`
- Create: `internal/steam/testdata/private_profile.json`
- Create: `internal/steam/testdata/store_app.json`

**Interfaces:**
- Produces: `steam.Client.OwnedGames(ctx, steamID string) ([]steam.OwnedGame, steam.Availability, error)`.
- Produces: `RecentlyPlayed(ctx, steamID string) ([]steam.RecentGame, steam.Availability, error)`.
- Produces: `Wishlist(ctx, steamID string) ([]steam.WishlistItem, steam.Availability, error)`.
- Produces: `Achievements(ctx, steamID string, appID int64) (steam.AchievementSummary, steam.Availability, error)`.
- Produces: `GameDetails(ctx, appID int64) (steam.GameDetails, error)`.

- [x] **Step 1: Write fixture-based failing contract tests**

Cover normal public responses, empty wishlist, missing achievements, malformed release dates, HTTP 429 retry metadata, and a private profile. Assert that private or absent data returns a typed `Availability` and does not masquerade as an empty successful import.

- [x] **Step 2: Verify contract tests fail without network access**

Run: `go test ./internal/steam`

Expected: FAIL because the adapter is absent.

- [x] **Step 3: Implement the HTTP adapters**

Use injected `http.Client` and base URLs so tests use `httptest.Server`. Add request timeouts, bounded concurrency, `Retry-After` parsing, response-size limits, and sanitized errors that never contain the Web API key.

- [x] **Step 4: Verify all adapter states**

Run: `go test ./internal/steam -race`

Expected: PASS with no live Steam calls.

- [x] **Step 5: Commit**

```bash
git add internal/steam
git commit -m "feat: add typed Steam API adapters"
```

### Task 5: Implement synchronization, achievements, and catalog history

**Files:**
- Create: `catalog_seed.json`
- Create: `internal/catalog/date.go`
- Create: `internal/catalog/date_test.go`
- Create: `internal/catalog/service.go`
- Create: `internal/catalog/service_test.go`
- Create: `internal/sync/service.go`
- Create: `internal/sync/service_test.go`
- Create: `internal/sync/achievements.go`
- Create: `internal/sync/achievements_test.go`
- Create: `db/queries/catalog.sql`
- Create: `db/queries/library.sql`
- Modify: `internal/jobs/queue.go`
- Modify: `internal/jobs/worker.go`
- Modify: `internal/app/app.go`
- Create: `internal/httpapi/sync_handlers.go`
- Modify: `internal/httpapi/router.go`

**Interfaces:**
- Consumes: Task 4 `steam.Client` and Task 2 repositories/queue.
- Produces: `sync.Service.Start(ctx, userID uuid.UUID) (jobID uuid.UUID, error)`.
- Produces: `sync.Service.Run(ctx, userID uuid.UUID) error`.
- Produces: `sync.SelectAchievementTargets(games []sync.UserGame, limit int) []int64` with `limit=30`.
- Produces: `catalog.ParseReleaseDate(raw string, now time.Time) catalog.ReleaseWindow`.
- Produces: `POST /api/sync` and `GET /api/sync/{id}`.
- Produces: registered job handlers for `steam.sync`, `catalog.refresh`, and `achievements.sync`, with versioned payload schemas and parent-workflow completion only after required child stages finish.

- [x] **Step 1: Write failing date and achievement-selection tests**

Assert exact dates, localized month/year, quarter, year, `Coming Soon`, and malformed text map to the correct precision without a fabricated day. Assert target selection is capped at 30 and orders games by recency, playtime, then stable AppID.

- [x] **Step 2: Write failing synchronization tests**

Add `TestSyncIsIdempotent`, `TestSyncAddsWishlistGameToCatalog`, `TestSyncPreservesSnapshotWhenProfileBecomesPrivate`, and `TestReleaseChangeAppendsHistory`. Use fake Steam clients and a real test database.

- [x] **Step 3: Run and verify failure**

Run: `go test ./internal/catalog ./internal/sync`

Expected: FAIL because services are absent.

- [x] **Step 4: Implement the workflow and catalog seed**

Make each stage independently idempotent. Persist availability and fetch time separately from data rows. Seed 20 AppIDs manually reviewed from Steam's visible upcoming-release pages on the implementation date, recording the source URL and observed date beside each entry; append unreleased wishlist AppIDs before metadata refresh. Manual seed curation is a development input, never an automated HTML scraper.

- [x] **Step 5: Add authenticated sync endpoints**

Return HTTP 202 with a job ID from `POST /api/sync`; expose stage, progress, last successful fetch time, and sanitized error details from the status endpoint. Require authentication and a valid CSRF token, and test both negative cases. Register the handlers with the worker and test enqueue-to-completion through a real queue.

- [x] **Step 6: Verify workflow behavior**

Run: `go test ./internal/catalog ./internal/sync ./internal/httpapi -race`

Expected: PASS, including private-profile snapshot preservation and duplicate prevention.

- [x] **Step 7: Commit**

```bash
git add catalog_seed.json internal/catalog internal/sync internal/httpapi db/queries/catalog.sql db/queries/library.sql internal/jobs internal/app internal/store/sqlc
git commit -m "feat: synchronize Steam profile and release catalog"
```

## Phase 3: AI Profile and Recommendations

### Task 6: Add OpenAI structured generation and embeddings

**Files:**
- Create: `internal/ai/types.go`
- Create: `internal/ai/client.go`
- Create: `internal/ai/openai.go`
- Create: `internal/ai/openai_test.go`
- Create: `internal/ai/prompts.go`
- Create: `internal/ai/testdata/feature_response.json`
- Create: `internal/ai/testdata/profile_response.json`
- Create: `internal/ai/testdata/explanation_response.json`
- Create: `internal/ai/testdata/refusal_response.json`
- Create: `internal/ai/testdata/embedding_response.json`
- Create: `db/queries/features.sql`
- Create: `db/queries/embeddings.sql`
- Modify: `internal/jobs/worker.go`
- Modify: `internal/app/app.go`
- Generate: `internal/store/sqlc/*`

**Interfaces:**
- Produces: `ai.Client.ExtractGameFeatures(ctx, ai.GameEvidence) (ai.GameFeatures, error)`.
- Produces: `SummarizeProfile(ctx, ai.ProfileEvidence) (ai.ProfileSummary, error)`.
- Produces: `ExplainRecommendation(ctx, ai.RecommendationEvidence) (ai.Explanation, error)`.
- Produces: `ClassifyFeedback(ctx, ai.FeedbackText) (ai.FeedbackClassification, error)`.
- Produces: `Embed(ctx, []string) ([][]float32, error)`.

- [x] **Step 1: Write failing schema and failure-mode tests**

Assert strict decoding for every structured output, rejection of unknown evidence IDs, free-text length enforcement, redaction of secrets, refusal detection, timeout propagation, invalid-vector dimension rejection, no persistence of invalid output, and deterministic disabled-capability behavior when `OPENAI_API_KEY` is absent.

- [x] **Step 2: Verify tests fail**

Run: `go test ./internal/ai`

Expected: FAIL because the adapter and schemas do not exist.

- [x] **Step 3: Implement OpenAI REST calls**

Use the Responses API with strict JSON Schema for generation and `/v1/embeddings` for vectors. Request exactly 1536 dimensions and reject any other returned length. Send only normalized game evidence and opaque local evidence keys; never send SteamID, API keys, session IDs, or email data. Register versioned `ai.features`, `ai.embedding`, and `ai.profile_summary` job handlers.

- [x] **Step 4: Implement content-hash caching**

Hash canonical model ID, schema version, prompt version, and normalized input. Reuse the latest valid result for the same hash; an error must leave the previous valid row unchanged.

- [x] **Step 5: Verify the adapter without live calls**

Run: `go test ./internal/ai -race`

Expected: PASS using fixtures and `httptest.Server` only.

- [x] **Step 6: Commit**

```bash
git add internal/ai db/queries/features.sql db/queries/embeddings.sql internal/jobs internal/app internal/store/sqlc
git commit -m "feat: add structured OpenAI and embedding adapters"
```

### Task 7: Build semantic profiles and deterministic ranking

**Files:**
- Create: `internal/profile/service.go`
- Create: `internal/profile/service_test.go`
- Create: `internal/profile/vectors.go`
- Create: `internal/profile/vectors_test.go`
- Create: `internal/recommendations/types.go`
- Create: `internal/recommendations/retrieval.go`
- Create: `internal/recommendations/scoring.go`
- Create: `internal/recommendations/scoring_test.go`
- Create: `internal/recommendations/service.go`
- Create: `internal/recommendations/service_test.go`
- Create: `db/queries/profiles.sql`
- Create: `db/queries/recommendations.sql`
- Create: `internal/httpapi/profile_handlers.go`
- Create: `internal/httpapi/recommendation_handlers.go`
- Modify: `internal/httpapi/router.go`
- Modify: `internal/jobs/worker.go`
- Modify: `internal/app/app.go`
- Generate: `internal/store/sqlc/*`

**Interfaces:**
- Consumes: Tasks 5 and 6 catalog, user signals, structured features, and embeddings.
- Produces: `profile.Service.Rebuild(ctx, userID uuid.UUID) (profile.Profile, error)`.
- Produces: `recommendations.Score(recommendations.Factors, recommendations.Weights) recommendations.Result`.
- Produces: `recommendations.Service.Rebuild(ctx, userID uuid.UUID) ([]recommendations.Recommendation, error)`.
- Produces: `GET /api/profile`, `GET /api/calendar`, and `GET /api/recommendations/{id}`.
- Produces: registered `profile.rebuild` and `recommendations.rebuild` handlers; completion records the new profile/ranking versions.

- [x] **Step 1: Write failing vector-profile tests**

Assert weighted positive and negative centroids, safe handling of zero vectors, sparse profiles, deterministic ordering for ties, and no NaN/Inf values.

- [x] **Step 2: Write failing scoring tests**

Use the exact weights from the spec. Assert wishlist adds 30, semantic similarity contributes at most 25, one dislike applies a soft penalty rather than a hard exclusion, repeated explicit feedback can exclude, uncertainty subtracts 5, and output clamps to 0–100.

- [x] **Step 3: Write failing service tests**

Assert candidate retrieval excludes released and explicitly hidden games, persists factor snapshots and algorithm version, uses template explanations after an OpenAI refusal, and returns deterministic results when embeddings are absent.

- [x] **Step 4: Run and verify failure**

Run: `go test ./internal/profile ./internal/recommendations`

Expected: FAIL because profile and ranking services are absent.

- [x] **Step 5: Implement profile and retrieval**

Generate positive/negative centroids, retrieve nearest upcoming games with `pgvector`, merge mandatory wishlist candidates, and pass the bounded candidate set to deterministic scoring.

- [x] **Step 6: Implement scoring and grounded explanations**

Persist every factor before requesting an explanation. Supply only those evidence rows to OpenAI; fall back to a template built from the same factors.

- [x] **Step 7: Add read APIs and verify**

Run: `go test ./internal/profile ./internal/recommendations ./internal/httpapi -race`

Expected: PASS, including sparse-profile and OpenAI-fallback cases.

- [x] **Step 8: Commit**

```bash
git add internal/profile internal/recommendations internal/httpapi db/queries/profiles.sql db/queries/recommendations.sql internal/jobs internal/app internal/store/sqlc
git commit -m "feat: add semantic profiles and deterministic ranking"
```

## Phase 4: Product Experience and Feedback Loop

### Task 8: Build synchronization, profile, and calendar interfaces

**Files:**
- Create: `web/src/api/types.ts`
- Create: `web/src/api/hooks.ts`
- Create: `web/src/components/SyncProgress.tsx`
- Create: `web/src/components/ProfileSummary.tsx`
- Create: `web/src/components/RecommendationCard.tsx`
- Create: `web/src/components/ReleaseWindowBucket.tsx`
- Create: `web/src/calendar/dateBuckets.ts`
- Create: `web/src/calendar/dateBuckets.test.ts`
- Create: `web/src/pages/DashboardPage.tsx`
- Create: `web/src/pages/CalendarPage.tsx`
- Create: `web/src/pages/CalendarPage.test.tsx`
- Modify: `web/src/App.tsx`
- Modify: `web/src/styles.css`

**Interfaces:**
- Consumes: Tasks 3, 5, and 7 authentication, sync, profile, and calendar APIs.
- Produces: month grid, agenda, filters, sync status, profile evidence, and recommendation detail UI.
- Produces: `bucketRelease(window: ReleaseWindow): CalendarBucket`.

- [x] **Step 1: Write failing date-bucket and calendar tests**

Assert an exact date appears on the correct day, month/quarter/year/coming-soon/unknown values appear only in their named buckets, filters affect grid and agenda identically, and no imprecise value receives a fabricated day.

- [x] **Step 2: Run frontend tests and verify failure**

Run: `pnpm --dir web test --run`

Expected: FAIL because components and bucketing do not exist.

- [x] **Step 3: Implement data hooks and loading/error states**

Poll an active sync job with bounded backoff. Distinguish unavailable, stale, incomplete, failed, and ready states in visible copy.

- [x] **Step 4: Implement profile and calendar views**

Render a responsive month grid, week agenda, release-window buckets, evidence drawer, wishlist marker, score, confidence, and deterministic/AI explanation source.

- [x] **Step 5: Verify UI behavior and production build**

Run: `pnpm --dir web test --run && pnpm --dir web run typecheck && pnpm --dir web run build`

Expected: tests, typecheck, and production build PASS.

- [x] **Step 6: Commit**

```bash
git add web
git commit -m "feat: add profile and release calendar UI"
```

### Task 9: Add feedback classification and visible re-ranking

**Files:**
- Create: `internal/feedback/service.go`
- Create: `internal/feedback/service_test.go`
- Create: `db/queries/feedback.sql`
- Create: `internal/httpapi/feedback_handlers.go`
- Create: `internal/httpapi/feedback_handlers_test.go`
- Modify: `internal/httpapi/router.go`
- Modify: `internal/jobs/worker.go`
- Modify: `internal/app/app.go`
- Create: `web/src/components/FeedbackForm.tsx`
- Create: `web/src/components/FeedbackForm.test.tsx`
- Modify: `web/src/components/RecommendationCard.tsx`
- Modify: `web/src/pages/CalendarPage.tsx`

**Interfaces:**
- Consumes: Task 6 `ClassifyFeedback` and Task 7 profile/recommendation rebuild services.
- Produces: `feedback.Service.Record(ctx, feedback.Command) (feedback.Accepted, error)`, where `Accepted` contains event ID, rebuild job ID, and previous ranking version.
- Produces: `POST /api/recommendations/{id}/feedback` and `POST /api/recommendations/{id}/reminder`.
- Produces: `GET /api/rankings/{version}/diff` with rebuild status, current version, and added/removed/reordered recommendation IDs.

- [x] **Step 1: Write failing service tests**

Add `TestRecordIsIdempotent`, `TestLowConfidenceTextDoesNotChangeProfile`, `TestLikeRaisesRelatedCandidate`, `TestSingleDislikeDoesNotBanGenre`, and `TestTextOver500CharactersIsRejected`.

- [x] **Step 2: Write failing handler and component tests**

Assert authentication and CSRF are required, the UI accepts a predefined reason plus optional text, displays pending re-ranking, and marks added/removed/reordered results after refresh.

- [x] **Step 3: Run and verify failure**

Run: `go test ./internal/feedback ./internal/httpapi && pnpm --dir web test --run FeedbackForm`

Expected: FAIL because the feedback loop is absent.

- [x] **Step 4: Implement feedback persistence and classification**

Persist the original recommendation snapshot before classification. Apply only high-confidence structured signals automatically; enqueue versioned `feedback.classify`, profile rebuild, and recommendation rebuild jobs with stable idempotency keys. Scope keys to the event/workflow ID so retries deduplicate while later legitimate feedback and sync workflows still run.

- [x] **Step 5: Implement feedback UI and visible changes**

Use optimistic reaction state only for the saved event, not for the new ranking. Poll the accepted rebuild job, then fetch and display the server-computed ranking diff. Require authentication and CSRF for feedback and reminder endpoints and test both failures.

- [x] **Step 6: Verify the full feedback loop**

Run: `go test ./internal/feedback ./internal/httpapi ./internal/recommendations -race && pnpm --dir web test --run && pnpm --dir web run build`

Expected: PASS, including idempotency, confidence threshold, and visible ranking-diff tests.

- [x] **Step 7: Commit**

```bash
git add internal/feedback internal/httpapi db/queries/feedback.sql web/src internal/jobs internal/app internal/store/sqlc
git commit -m "feat: learn from structured and free-text feedback"
```

### Task 10: Deliver a Mailpit digest with signed feedback links

**Files:**
- Create: `internal/notifications/signer.go`
- Create: `internal/notifications/signer_test.go`
- Create: `internal/notifications/digest.go`
- Create: `internal/notifications/digest_test.go`
- Create: `internal/notifications/smtp.go`
- Create: `internal/notifications/smtp_test.go`
- Create: `internal/httpapi/digest_handlers.go`
- Create: `internal/httpapi/email_action_handlers.go`
- Create: `internal/httpapi/email_action_handlers_test.go`
- Create: `db/queries/notifications.sql`
- Modify: `internal/httpapi/router.go`
- Modify: `internal/jobs/worker.go`
- Modify: `internal/app/app.go`
- Generate: `internal/store/sqlc/*`
- Create: `web/src/components/TestDigestButton.tsx`
- Modify: `web/src/pages/DashboardPage.tsx`

**Interfaces:**
- Produces: `notifications.Signer.Sign(notifications.Action) (string, error)` and `Verify(token string, now time.Time) (notifications.Action, error)`.
- Produces: `notifications.Service.SendTestDigest(ctx, userID uuid.UUID) (deliveryID uuid.UUID, error)`.
- Produces: `POST /api/digests/test` and `GET /actions/feedback/{token}`.

- [x] **Step 1: Write failing signing and digest tests**

Assert token expiry, signature tampering, purpose/recommendation binding, duplicate delivery-job prevention, deterministic ordering, no repeated unchanged recommendations, escaped user/game text in HTML, failure before SMTP acceptance returning to retryable state, and failure after SMTP acceptance/before database confirmation becoming `ambiguous` rather than automatically resending.

- [x] **Step 2: Write failing email-action tests**

Assert a valid link records exactly one feedback event without opening a session, while expired, altered, or replayed action tokens fail safely.

- [x] **Step 3: Run and verify failure**

Run: `go test ./internal/notifications ./internal/httpapi -run 'Digest|Signer|EmailAction'`

Expected: FAIL because notification services are absent.

- [x] **Step 4: Implement digest composition and SMTP delivery**

Include upcoming wishlist releases, top new recommendations, grounded explanations, and Like/Dislike/Remind links. Use a delivery idempotency key based on user, digest kind, content hash, and ISO week. Persist `pending`, `sending`, `sent`, `failed`, and `ambiguous` states with attempt ownership. Never claim unconditional exactly-once SMTP delivery: an ambiguous attempt requires manual inspection or an explicit resend action.

- [x] **Step 5: Implement signed email actions**

Tokens contain only opaque IDs, action, issued-at, expiry, nonce, and signature. Mark a nonce consumed transactionally with recording feedback. Require authentication and CSRF for `POST /api/digests/test`; register the digest handler with the worker.

- [x] **Step 6: Verify against Mailpit**

Run: `docker compose up -d db mailpit && go test ./internal/notifications ./internal/httpapi -race`

Expected: PASS; a manual `POST /api/digests/test` in the running app produces one readable Mailpit message.

- [x] **Step 7: Commit**

```bash
git add internal/notifications internal/httpapi db/queries/notifications.sql web/src internal/jobs internal/app internal/store/sqlc
git commit -m "feat: add signed local recommendation digest"
```

## Phase 5: Diagnostics and End-to-End Verification

### Task 11: Add diagnostics, deletion, documentation, and smoke coverage

**Files:**
- Create: `internal/httpapi/diagnostic_handlers.go`
- Create: `internal/httpapi/data_handlers.go`
- Create: `internal/httpapi/data_handlers_test.go`
- Modify: `internal/httpapi/router.go`
- Create: `web/src/pages/DiagnosticsPage.tsx`
- Create: `web/src/pages/DiagnosticsPage.test.tsx`
- Modify: `web/src/App.tsx`
- Create: `web/tests/e2e/smoke.spec.ts`
- Create: `web/playwright.config.ts`
- Modify: `web/package.json`
- Modify: `Makefile`
- Modify: `README.md`
- Create: `docs/local-demo.md`

**Interfaces:**
- Consumes: every previous task's health, job, sync, profile, feedback, and notification interfaces.
- Produces: `GET /api/jobs`, expanded `GET /api/health`, and `DELETE /api/data`.
- Produces: `make verify` as the single complete verification command.

- [x] **Step 1: Write failing deletion and diagnostic tests**

Assert deletion removes user-derived library, wishlist, achievements, profiles, vectors, recommendations, feedback, jobs, and deliveries while retaining shared catalog games. Assert diagnostics never return secret values or raw OpenAI prompts. Add a concurrent-worker test proving an in-flight job cannot recreate deleted user data, plus unauthenticated and wrong-CSRF deletion tests.

- [x] **Step 2: Write the failing smoke test**

Use test-only fixture authentication and fake Steam/OpenAI servers. Prove fixture authentication fails closed unless the dedicated test-harness flag is set. Cover login, sync, achievements, profile, calendar buckets, feedback, visible re-ranking, test digest, signed email feedback, retry-safe repeated sync, and deletion.

- [x] **Step 3: Run and verify failure**

Run: `go test ./internal/httpapi -run 'Delete|Diagnostic' && pnpm --dir web exec playwright test tests/e2e/smoke.spec.ts`

Expected: FAIL because diagnostics, deletion, and the test harness are incomplete.

- [x] **Step 4: Implement diagnostics and deletion**

Return component state, freshness timestamps, and sanitized job errors. Delete user-owned rows in a transaction and invalidate active sessions after success.

- [x] **Step 5: Document local setup and real-account verification**

Document prerequisites, environment variables, Steam profile visibility, OpenAI key setup, seed-catalog editing, startup, Mailpit, cleanup, and a manual demo script. Keep real credentials out of examples. Document `APP_BASE_URL` as the Go origin and the Vite URL as development-only.

- [x] **Step 6: Add the complete verification target**

`make verify` must create and wait for an isolated disposable test database, run migrations there, run pinned `sqlc` generation with a diff check scoped to `internal/store/sqlc`, run Go tests with race detection, frontend tests/typecheck/lint/build, install the pinned Playwright Chromium browser when absent, and run smoke tests. Configure Playwright `webServer` entries for fake services, Go, and Vite with readiness URLs; Vitest must exclude Playwright specs.

- [x] **Step 7: Run all verification**

Run: `make verify`

Expected: all migrations, generated-code checks, Go tests, frontend checks, production builds, and the end-to-end smoke test PASS.

- [x] **Step 8: Perform a real local smoke run**

Run: `docker compose up --build`

Expected: the allowlisted Steam account can log in, complete synchronization, see recommendations in both calendar views, submit feedback, observe re-ranking, and receive one test digest in Mailpit.

- [x] **Step 9: Commit**

```bash
git add internal/httpapi web/src web/tests web/playwright.config.ts web/package.json Makefile README.md docs/local-demo.md
git commit -m "test: verify the complete local MVP flow"
```

## Final Review Gate

- [ ] Confirm every acceptance criterion in the design spec has an automated or manual verification above.
- [ ] Run `git status --short` and confirm `docs/steam-release-recommender-idea.md` remains untouched unless the user separately requests committing it.
- [ ] Run `make verify` once more from a clean application database.
- [ ] Review the final branch diff for secrets, generated artifacts, accidental fixture PII, and unrelated changes.
- [ ] Record the exact real-Steam limitations observed during the smoke run in `docs/local-demo.md`.

