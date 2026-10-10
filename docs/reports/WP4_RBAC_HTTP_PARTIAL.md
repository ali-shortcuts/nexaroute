# WP4 — HTTP RBAC enforcement (partial)

## Implemented in this branch

- Added a server-side permission map for every currently registered `/admin/api/*` route family.
- Unknown Admin API paths and unsupported method/permission combinations fail closed with HTTP 403.
- The existing static Admin API key and existing keyless loopback mode map to one explicit `legacy-admin-break-glass` owner identity. Role claims from request headers are ignored.
- State-changing Admin API actions and forbidden Admin requests emit credential-free audit log lines (method, quoted path, actor, status, request ID); request bodies and tokens are not included.
- Added unit tests for route permission mapping, unmapped paths, unsupported methods, unauthenticated identity denial, and middleware denial/owner compatibility.

## Not complete

This is not a complete WP4 implementation. There is no viewer/operator/admin identity assignment, no OIDC authorization-code/PKCE flow, no issuer/audience/signature/expiry/nonce/state validation, no server-side cookie session or revocation, and no mock OIDC conformance test. Audit entries are application logs, not a durable structured audit store. The web UI still authenticates through the existing API-key flow. No real identity provider was tested.

## Verification in this branch so far

- `gofmt` on changed Go files — run.
- `go test ./internal/httpapi ./internal/authz` — PASS after adding the permission map and middleware tests.
- Full race/verify/browser/release/install gate for this branch — not yet run at the time of writing.

## Acceptance status

**PARTIAL / NOT ACCEPTED as completion of WP4.** Do not claim SSO or multi-user RBAC. The remaining concrete work is to implement a maintained-library OIDC code+PKCE adapter, explicit role-claim allow-list/mapping, secure server-side sessions integrated with existing CSRF, negative tests using an in-process OIDC provider, and a structured audit sink with tested privileged-action coverage.
