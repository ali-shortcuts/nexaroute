# M2-2 — Access page and virtual-key/team/project controls

The client-keys admin API already supports the full record
(name, tenant, project, team, role, allowed models/routes, RPM,
TPM, expiry, revoke, rotate) and usage is tracked per identity —
but the UI only had a compact Settings card whose create form
exposed name/tenant/role/RPM/TPM. This unit adds a dedicated
Access page:

- Nav entry + page: summary (keys, active, revoked, tenants),
  search across key fields, and a table with scope
  (tenant/project/team), role, model/route scope, limits, privacy
  requirement (shown when set server-side), per-key usage merged
  from the identities endpoint, status, and rotate/revoke.
- The create form gains project, team, allowed models, and
  allowed routes fields (all supported by the existing API).
- Settings' Consumer keys card links to the page.
- Verification: embedded-asset contract tests (the established
  webFS pattern) + the existing client-keys API tests.
