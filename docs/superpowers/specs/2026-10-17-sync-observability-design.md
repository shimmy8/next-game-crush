# Synchronization Observability Design

**Date:** 2026-10-17
**Status:** Ready for user review
**Related design:** `docs/superpowers/specs/2026-10-17-next-game-crush-mvp-design.md`

## 1. Objective

Add structured, privacy-safe logs that make a local synchronization diagnosable
from request acceptance through every background-job attempt. The logs must
show whether work is progressing, waiting, retrying, or terminal without
revealing the user's Steam activity, identifiers, secrets, or upstream data.

This design does not change the progress API or add a log viewer to the
frontend. Logs are operational output for the local service.

## 2. Logger Ownership

Construct one JSON `slog` logger in application composition and inject it into
the HTTP layer, worker, and synchronization services. Tests may inject a
discarding logger. Domain packages must not create independent global loggers.

Every event uses an explicit allowlist of fields. Logging a Go struct, request
object, job payload, upstream response, or arbitrary error value is forbidden.

## 3. Correlation Model

Use application-generated opaque identifiers for correlation:

- `request_id` for an HTTP request;
- `workflow_id` for the synchronization workflow;
- `job_id` and nullable `parent_job_id` for queued work.

Do not trust or echo a client-provided request ID. Correlation IDs must be
random and carry no encoded SteamID, user ID, AppID, or other domain data.

## 4. Events

### Sync request events

Emit `sync_request` with an `outcome` of `accepted` or `failed`. Allowed fields
are request ID, workflow ID when created, bounded HTTP status, and duration.

### Worker attempt events

Emit `job_attempt` with one of these stages:

- `started`;
- `heartbeat`;
- `deferred`;
- `succeeded`;
- `retry_scheduled`;
- `dead`;
- `cancelled`;
- `lease_lost`.

Allowed fields are correlation IDs, safe job kind, safe logical stage, attempt
number, maximum attempts, whether the stage is required, duration, and bounded
retry delay. A heartbeat is emitted at a controlled interval only for a long
attempt, so silence can be distinguished from a stalled process without
flooding output.

The queue failure path returns a bounded typed outcome so the worker can log
`retry_scheduled` versus `dead` without parsing an error string or re-reading a
payload.

### Steam source events

Emit `sync_source` at `started` and `finished` for:

- owned games;
- recently played;
- wishlist;
- achievements.

The finished event may include only a bounded availability outcome, safe reason
code, duration, and aggregate counts. Achievement logs aggregate totals across
games; they never identify a game or emit one event containing its AppID or
name.

### Worker infrastructure events

Emit bounded events for worker startup/shutdown, claim failures, lease-renewal
failures, and database availability problems. Include only safe categories and
correlation fields that already meet this design.

## 5. Data That Must Never Be Logged

The following are forbidden in all new synchronization logs:

- SteamID, internal user ID, AppID, game name, display name, or avatar URL;
- Steam/OpenAI keys, cookies, sessions, CSRF values, signatures, or tokens;
- OpenID callback URLs, query parameters, state, or claimed identifiers;
- job payloads, idempotency keys, worker IDs, fencing tokens, or database rows;
- raw upstream request/response bodies or headers;
- raw error strings whose content is not controlled by the application;
- prompts, model responses, embeddings, free-text feedback, or catalog text.

Failures map to reviewed safe reason codes such as `timeout`, `rate_limited`,
`upstream_unavailable`, `malformed_response`, `lease_lost`, and
`persistence_failed`. Unknown errors use `internal_error`.

## 6. Severity and Volume

Use `INFO` for lifecycle milestones and successful source completion, `WARN`
for retryable failures, optional source unavailability, and lease loss, and
`ERROR` for terminal required-stage failures or worker infrastructure failure.

Heartbeats use `INFO`, are rate-limited per attempt, and contain no changing
payload. Routine polling and every processed item are not logged. Aggregate
counts replace per-game events.

## 7. User-Facing Relationship

The frontend continues to use the synchronization status API, not log output.
Operational logs explain why the status is waiting or retrying, while the UI
uses localized, bounded messages. Safe reason codes may be shared internally,
but log fields and API error bodies remain independently allowlisted.

## 8. Verification and Acceptance Criteria

Tests must capture JSON logs and assert:

- accepted sync requests carry request/workflow correlation;
- job attempts emit start, terminal outcome, duration, and attempt metadata;
- retry scheduling and permanent death are distinguishable;
- long attempts emit bounded heartbeats and short attempts do not;
- Steam imports emit source begin/end and only aggregate achievement counts;
- logger injection works without package-global state;
- canary values placed in every forbidden input are absent from complete log
  output;
- unknown errors map to `internal_error` and never use their raw string;
- all emitted event names, stages, reason codes, and field keys belong to a
  strict allowlist.

The feature is accepted when a local sync can be followed from request to
completion or failure using only correlation IDs and safe lifecycle data, and
the log-canary suite proves that personal or secret values are not emitted.
