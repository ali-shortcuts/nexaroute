# WP4 — RBAC and SSO design

## Decision

NexaRoute will separate **authentication** from **authorization**. The new
`internal/authz` package is the transport-independent authorization contract;
HTTP, OIDC, SAML, and legacy Admin-key adapters must produce an immutable
`authz.Identity` and call `Authorize(permission)`. No adapter may infer a role
from an untrusted browser header.

The existing Admin API key remains the backward-compatible bootstrap path until
an SSO provider is configured. It is not an identity system and therefore must
be treated as the owner/bootstrap principal, not as a source of arbitrary role
claims.

## Role matrix

| Role | Read | Write | Sensitive operations |
|---|---|---|---|
| `owner` | config, providers, routing, usage, audit | config, providers, routing | keys, evaluation, video |
| `admin` | same as owner | same as owner | keys, evaluation, video |
| `operator` | config, providers, routing, usage, audit | routing | evaluation, video |
| `developer` | config, providers, routing, usage, audit | none | evaluation only |
| `viewer` | config, providers, routing, usage, audit | none | none |

The matrix is fail-closed. Unknown roles, unauthenticated identities, and
unrecognized permissions are denied. External permission claims are additive
only and support a bounded namespace wildcard such as `video.*`.

## SSO contract

The future SSO adapter must:

1. validate the issuer, signature, audience, expiry, nonce/state, and PKCE for
   OIDC; or validate the signed assertion, audience restriction, recipient,
   clock skew, and `InResponseTo` for SAML;
2. map a stable `(issuer, subject)` pair to an internal identity;
3. map only allow-listed group/claim values to the five roles;
4. reject missing or ambiguous mappings; and
5. emit audit events for login, logout, denied permission, role-map changes, and
   emergency bootstrap use without logging tokens or assertions.

Session cookies should be opaque, `HttpOnly`, `Secure` under TLS,
`SameSite=Lax/Strict` as the deployment requires, rotated after login, and
revoked server-side. The existing CSRF token remains mandatory for browser
state-changing requests. Bearer/API-key clients continue to use the stateless
path and do not receive browser sessions.

## Migration plan

1. Keep `Admin.APIKey` as the local bootstrap owner while the SSO adapter is
   disabled.
2. Add an explicit SSO configuration block containing issuer/metadata URL,
   client ID, redirect URI, allowed issuers/audiences, role claim, and an
   environment-variable name for the client secret. Never persist client
   secrets or refresh tokens in plaintext config.
3. Wire the HTTP middleware to create `authz.Identity`; preserve legacy key
   authentication only for the bootstrap owner and emit a deprecation warning
   when SSO is enabled.
4. Add endpoint-level permission requirements from the matrix, with negative
   tests for every write and sensitive route.
5. Add provider-backed conformance tests before claiming OIDC/SAML support.

## Current implementation boundary

WP4 delivers and tests the authorization core and this contract. OIDC/SAML
network clients, token validation, server-side sessions, and endpoint wiring
remain intentionally unimplemented until provider choice and deployment
metadata are supplied. This prevents a misleading “SSO enabled” claim and
keeps the existing gateway behavior stable.
