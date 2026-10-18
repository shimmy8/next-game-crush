# Source Failure Diagnostics Design

## Goal

Distinguish a genuinely unavailable Steam source from a transient failed request in the sync UI, and leave enough sanitized evidence in logs to identify the upstream failure class next time.

## User-facing behavior

- If any active source has `availability=unavailable`, show: `Некоторые источники недоступны — продолжаем`.
- Otherwise, if any active source has `availability=request_failed`, show: `Не все данные удалось обновить — продолжаем`.
- If both states are present, the unavailable-source message has priority.
- Existing stage details continue to show failed counts and `Ошибка запроса` without exposing AppID values.

## Safe backend diagnostics

- Keep `user_source_states.error` generic and free of URLs, identifiers, response bodies, or upstream messages.
- Classify Steam failures as `timeout`, `rate_limited`, `upstream_unavailable`, `upstream_rejected`, `malformed_response`, or `internal_error`.
- Add `upstream_status` to a finished `sync_source` log only when a sanitized `steam.RequestError` carries an HTTP status in `400..599`.
- Never log request URLs, API keys, SteamID values, AppID values, response bodies, or raw errors.
- For an aggregate achievements source, record the diagnostic selected for the aggregate outcome; a single failed target therefore exposes its safe HTTP class/status without identifying the game.

## Verification

- Backend unit tests cover timeout, 429, retryable 5xx, non-retryable 4xx, malformed response, and unknown errors.
- Observability tests prove `upstream_status` is bounded and absent when unsafe or irrelevant.
- Sync tests prove aggregate achievement logs contain only allowlisted fields and never AppID/error canaries.
- Frontend model tests cover unavailable-only, request-failed-only, both, and no-warning states.
- Run backend race tests, the complete frontend suite, and `make verify`.

