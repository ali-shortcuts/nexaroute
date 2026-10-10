# WP4b security review

**Scope:** PR #224, branch `wp4-rbac-sso-runtime` at the reviewed head before any subsequent WP5–WP8 work.

## Review conclusion

WP4b is **implemented and locally verified** for the configured single OIDC issuer, server-side Admin API authorization, opaque sessions, CSRF, and durable local audit. No live third-party identity provider was configured or tested; real IdP interoperability remains **unverified**. The local security store is single-host only.

## Acceptance evidence

| Requirement | Evidence | Result |
|---|---|---|
| Every Admin API route/method maps to a permission | `internal/httpapi/adminPermissionForRequest`; route registration in `internal/httpapi/server.go`; `admin_authz_test.go`; `coverage_admin_routes_test.go` | PASS; unknown paths and unsupported methods deny by default |
| Unknown/unauthenticated requests deny | `server.go` middleware; `TestAdminMiddlewareDeniesUnmappedRouteAndKeepsOwnerAccess`; malformed/unknown session tests | PASS |
| Legacy Admin-key bootstrap is explicit and gated | `config.Admin.EmergencyAccessEnabled`; `legacyAdminAuthorized`; `legacyAdminIdentity`; `TestOIDCFailureNeverDowngradesToEmergencyAPIKey` | PASS; emergency owner is not an OIDC role and cannot bypass OIDC outage |
| OIDC Authorization Code + PKCE | `oidc.go` uses `oauth2.S256ChallengeOption`; in-process provider flow in `oidc_test.go` | PASS |
| State, nonce, and browser binding | one-time hashed state map, nonce verification, HttpOnly flow cookie, binding hash; callback replay/mismatch tests | PASS |
| Issuer, audience, signature, expiry, clock-skew handling | maintained `coreos/go-oidc` verifier plus exact issuer and configured audience checks; negative token cases in `oidc_test.go` | PASS for configured verifier policy |
| Allow-listed claim-to-role mapping | `claimStringValues`, explicit `role_mappings`, exactly one resolved role; conflicting/unknown role tests | PASS; only viewer/operator/admin are accepted |
| Opaque server-side sessions | random 32-byte URL token; only SHA-256 hash stored; bbolt session records | PASS |
| Secure, rotated, revocable sessions | HttpOnly, scoped, SameSite cookies; login rotates prior session; absolute/idle expiry; logout and policy-change revocation tests | PASS |
| CSRF remains mandatory | same-origin plus strict double-submit for OIDC sessions; missing, mismatched, and cross-origin tests | PASS |
| Durable audit events | transactional bbolt login/logout/session rotation, denied access, authorized/completed mutation records; audit failure blocks protected action before handler | PASS |
| Client secret handling | named environment variable only; no plaintext config/log/audit storage; source and config tests | PASS |

## Commands run locally

```text
export PATH=/usr/local/go/bin:$PATH
 go test ./internal/httpapi ./internal/securitystore ./internal/authz -count=1 -run 'Test(Admin|OIDC|Session|Malformed|Viewer|Legacy|Security|Authorization|CSRF|Audit)'
```

Result: **PASS** (`internal/httpapi`, `internal/securitystore`, and `internal/authz`).

The branch’s latest GitHub PR checks were also inspected before this review: `verify`, Go vulnerability scan, CodeQL (Go), and CodeQL were completed successfully on the then-current PR head. Any later push requires fresh checks before merge.

## Boundaries and unverified items

- No real external IdP was tested; provider-specific interoperability is **unverified**.
- Sessions, audit, and pending login transactions are local to one host/process. Shared Redis/SQL stores, multi-node session coordination, and cross-instance OIDC callback routing are not implemented.
- SAML, SCIM/provisioning, and automated role lifecycle are not implemented.
- The local bbolt store is not intended for a network filesystem.
