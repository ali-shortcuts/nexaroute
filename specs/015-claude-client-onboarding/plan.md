# Implementation plan

1. Add `client_base_url` to the authoritative Go config and environment overlay.
2. Validate URL scheme, authority, path-only prefix, and local HTTP restriction before persistence or hot reload.
3. Publish bounded, secret-free `client_access` metadata in the admin snapshot and expose the setting through `/admin/api/settings`.
4. Update embedded Connect and Settings UI to use configured URL or clearly labelled fallback; preserve route mode/order and hidden retry values.
5. Align Claude Code docs and sample config.
6. Run focused tests, syntax checks, full available Go gates, and inspect final diff/status.
