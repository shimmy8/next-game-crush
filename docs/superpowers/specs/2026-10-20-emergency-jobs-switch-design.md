# Emergency Jobs Switch Design

## Goal

Keep the HTTP server available while stopping all background job execution during an operational emergency.

## Design

Add a strict `JOBS_ENABLED` runtime setting. It defaults to enabled so existing environments are unchanged. Values `0` and `false` disable job execution; values `1` and `true` enable it; any other non-empty value prevents startup.

When disabled, the application starts the HTTP server but does not start the interactive worker, catalog worker, or catalog scheduler. Existing queued jobs remain untouched, and HTTP requests may still enqueue work that will remain pending until jobs are enabled again.

Both development and production Compose files pass the setting through with an enabled default. Operators disable execution by setting `JOBS_ENABLED=0` in the deployment `.env` and recreating the app container.

## Verification

- Configuration tests cover the default, accepted disabled/enabled values, and invalid input.
- Application lifecycle tests prove that HTTP starts and stops normally while no job runner starts.
- Focused race tests and the repository verification gate must pass before the change is committed.
