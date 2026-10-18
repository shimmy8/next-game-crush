# AI Feature Recovery Design

## Goal

Stop valid OpenAI feature extraction attempts from failing because the model invents unsupported evidence keys, prevent stale retry errors from making recovered jobs look failed, and expose safe AI failure categories in structured logs.

## Evidence-key contract

- `ExtractGameFeatures` builds its JSON Schema after normalizing game evidence.
- `evidence_keys.items` is a string enum containing exactly the normalized evidence keys supplied in that request.
- The array requires at least one item and keeps the existing maximum of 50.
- Runtime `validateCitations` remains in place as defense in depth and rejects duplicate citations because OpenAI strict Structured Outputs does not support the JSON Schema `uniqueItems` keyword.
- The accepted output contract does not change, so existing valid cached features remain valid and no prompt/hash version bump is required.
- No evidence values, prompts, game identifiers, or model responses are logged.

## Retry error lifecycle

- Claiming a queued, retryable, or expired running job starts a fresh attempt and clears `sync_jobs.last_error` atomically with the claim.
- Historical attempt errors remain in `job_attempts`; only the current job presentation loses the stale error.
- A later failure stores the new bounded error normally.
- A feature job that succeeds and waits for its embedding child therefore appears as waiting, not failed.

## Safe AI reasons

- AI sentinel errors may expose an allowlisted `SafeReason() string` classification without exposing their message context.
- The worker maps `invalid_evidence` and `invalid_output` through this interface before falling back to `internal_error`.
- Observability allowlists both reasons.
- Logs retain workflow/job correlation but never include job payloads, evidence keys, AppID, prompts, or raw model output.

## Recovery of the active workflow

After the local app is rebuilt, currently retryable feature jobs use the constrained schema on their next attempt. Jobs already waiting for embeddings retain their children and continue normally. Dead jobs are not silently resurrected; a later manual synchronization creates a new root workflow if any jobs exhausted all attempts before deployment.

## Verification

- OpenAI request tests inspect the emitted enum, supported array bounds, absence of unsupported schema keywords, and absence of evidence values from the schema.
- Runtime rejection tests remain.
- Queue integration tests prove stale `last_error` is cleared on retry claim and replaced on a subsequent failure.
- Worker tests prove safe AI reason propagation and hostile-reason fallback.
- Full Go race, frontend, Docker, and Playwright verification passes.

