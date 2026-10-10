# WP4 — RBAC, OIDC, secure sessions, and durable audit

## Decision and implementation

Authentication is separate from authorization. `internal/authz` defines the permission contract; `internal/httpapi` authenticates a verified OIDC identity or an explicitly enabled emergency owner, then the central middleware maps each registered Admin route and HTTP method to permission checks. Request headers never supply trusted identity, role, or permission claims.

The OIDC client is provider-agnostic and uses maintained libraries: `github.com/coreos/go-oidc/v3` for discovery and ID-token verification, and `golang.org/x/oauth2` for authorization-code exchange and PKCE S256. The client requires an exact issuer match, secure discovery endpoints (HTTPS, except loopback HTTP for local development), configured client ID and expected audience, a valid signing key/signature, token expiry, nonce, and a single-use state bound to an HttpOnly browser cookie. The callback does not accept caller-controlled return URLs. The state, nonce, PKCE verifier, and browser binding are one-time and bounded by a short pending-login lifetime.

OIDC role values are explicitly allowlisted through `admin.oidc.role_mappings`; a verified identity must resolve to exactly one of `viewer`, `operator`, or `admin`. Unknown, missing, malformed, or conflicting mapped claims fail closed. Role permissions are enforced server-side and documented in `docs/CONFIGURATION.md`.

The web UI uses opaque 32-byte session identifiers in HttpOnly cookies; only their SHA-256 hashes and server-verified session records are persisted. Login rotates any previous session atomically with its audit record; logout revokes the session atomically with its audit record. Absolute and idle expiry are validated and enforced, and a policy fingerprint change invalidates existing sessions. Browser mutations require the existing same-origin and double-submit CSRF protections.

A local bbolt store persists sessions and structured audit events. It uses transactions, restrictive 0700 parent/0600 file permissions, rejects unsafe existing paths, and survives process restart on the same host. The audit schema excludes request bodies and secrets. Privileged actions require a durable pre-action record; audit-write failure prevents the action. Completion events are also written when possible, while the pre-action record remains the durable intent if a completion write fails.

## Role matrix

| Role | Read permissions | Write / privileged permissions |
|---|---|---|
| `viewer` | config, providers, routing, usage | none |
| `operator` | config, providers, routing, usage | routing, run evaluation, video management |
| `admin` | config, providers, routing, usage, audit | config, providers, routing, run evaluation, client keys, video management |

The emergency `owner` identity is not mapped from OIDC. It is available only when `admin.emergency_access_enabled` is explicitly true, OIDC is disabled, and the configured key/keyless-loopback circumstances apply. It is audited. OIDC provider failure never silently downgrades to emergency authentication.

## Automated evidence

An in-process `httptest` provider supplies real discovery, JWKS, authorization/token endpoints, RSA-signed ID tokens, and controllable negative responses. Integration tests exercise the actual handlers/middleware and cover PKCE, issuer/client-ID/audience/signature/expiry/nonce, state replay/tampering/browser binding, endpoint metadata, role mapping, session rotation/expiry/logout, CSRF, forged cookies/headers, emergency behavior, audit redaction, and audit-store failures.

The provider is test-only; no external IdP credentials are needed to run tests. Real-provider interoperability remains a deployment smoke test and is not represented as having been performed.

## Operational limitations and acceptance

- The security DB and pending OIDC login transactions are single-host. Do not share the DB over a network filesystem or use multiple gateway processes without a future distributed backend. Complete an OIDC round-trip on the instance that initiated it.
- SAML, automated identity provisioning/deprovisioning, and distributed session/audit stores are not implemented.
- WP4 meets its local coverage and exact-revision verification gates on head `aa5a864`: repository-wide coverage is 85.12%, and fresh GitHub `CI/verify`, vulnerability scan, CodeQL (Go), and CodeQL checks passed. PR #224 is ready for review but remains unmerged. Any follow-up commit requires fresh checks before merge. The store remains single-host and real-provider IdP interoperability was not tested; current evidence is recorded in `WP4_RBAC_HTTP_PARTIAL.md`.
