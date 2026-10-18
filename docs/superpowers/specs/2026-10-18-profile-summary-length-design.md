# Profile Summary Length Design

## Goal

Make Russian OpenAI profile summaries reliably pass validation by expressing the desired size in the model instructions and validating Unicode characters with a tolerant range.

## Contract

- Ask the model for a Russian `summary` of 400–600 Unicode characters.
- Accept a non-blank trimmed `summary` of 320–720 Unicode characters.
- Ask for concise `preferences` and `avoidances` entries of at most 120 Unicode characters.
- Normalize those auxiliary arrays by trimming whitespace, dropping blank items, removing duplicates while preserving order, and dropping entries longer than 144 Unicode characters.
- Auxiliary cleanup must not invalidate an otherwise usable profile summary.
- Keep evidence citation validation strict: at least one supplied evidence key, no unknown keys, and no duplicates.
- Count user-visible Unicode code points rather than UTF-8 bytes.
- Bump the profile prompt version so previously failed or cached work uses the new contract.

## Recovery

After rebuilding the local service, retry the current dead profile-summary job without exposing its payload or resetting unrelated jobs. Verify only aggregate state and summary availability.

