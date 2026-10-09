# Client-auth body preservation fix — 2026-10-09

Branch: `phase1a-client-auth-body-fix` (based on `phase1a-encrypted-secrets`)

The fix preserves large request bodies while client authentication inspects request metadata and adds regression coverage for body preservation and per-key bucket limits. No assertions were weakened and no tests were skipped.

Raw command output is recorded in [CLIENT_AUTH_BODY_FIX_RAW_2026-10-09.txt](CLIENT_AUTH_BODY_FIX_RAW_2026-10-09.txt).

Status: local validation passed; a separate PR is opened from this branch so Phase 1a can review the fix independently.
