# WP4 — OIDC, sessions, RBAC, and audit (acceptance pending)

## Implementation delivered on this branch

The previous “route-permission-only” description is obsolete. The draft PR branch now contains the end-to-end implementation below:

- **OIDC sign-in:** maintained `coreos/go-oidc` verifier and `golang.org/x/oauth2` Authorization Code + PKCE S256 flow; discovery issuer, endpoint transport/URL safety, client ID, configured audience, JWKS/signature, token expiry, nonce, state, and browser-binding are checked. Unsafe or mismatched provider metadata fails closed. No live third-party IdP credentials were available for a provider-specific deployment test; an in-process OIDC provider exercises the complete protocol path.
- **Role assignment and authorization:** only verified OIDC claim values in the explicit `role_mappings` allowlist become `viewer`, `operator`, or `admin`; request-supplied role/subject/permission headers do not grant authority. Registered Admin API path/method pairs map to server-side permissions; unknown paths/methods deny by default.
- **Sessions and browser protection:** opaque random cookies, server-side bbolt session records (only the SHA-256 session-ID hash is persisted), absolute and idle expiry, session rotation/revocation, OIDC-policy invalidation, same-origin checks, and strict double-submit CSRF for session mutations and logout. Cookies are `HttpOnly`, `SameSite`, scoped to `/admin/`, and `Secure` for HTTPS.
- **Break-glass:** static Admin API key and keyless loopback compatibility are explicit opt-in emergency access, map only to the fixed owner identity, and are audited. Older config files that omit `emergency_access_enabled` load disabled; explicit `false` survives subsequent saves. Break-glass never downgrades an enabled but unavailable OIDC provider.
- **Durable audit:** structured, sanitized records in local bbolt storage; session creation/rotation and login audit, plus revocation and logout audit, are transactional. Privileged actions require durable pre-action audit and record completion; audit-storage failures fail closed with HTTP 503. Store directory/file permissions are enforced.
- **Dashboard and operations:** the UI supports OIDC login/logout and the explicitly configured emergency path; docs now describe the implemented contract and its deployment boundaries.

## Security and integration coverage added

- In-process OIDC end-to-end tests cover successful code exchange, PKCE, nonce, audience, state replay/tampering, browser binding, role mapping, session rotation/logout, token and JWKS failures, issuer mismatch, unsafe discovery endpoints, and secret-free audit.
- Actual Admin-handler tests exercise viewer/operator/admin read/write permission boundaries, valid CSRF, privileged settings mutation and durable completion audit; middleware tests cover expiry/idle timeout, cookie attributes, policy-change revocation, malformed/unknown sessions, role-header injection, audit failure, and emergency-access behavior.
- Config validation tests cover HTTPS/loopback URL rules, unsafe URL components, environment-variable naming, role allowlists, and input bounds. Store tests cover durability across reopen, atomic rotation, retention, sanitization, and filesystem permissions.

## Local verification on the current source tree

All commands below passed on Go 1.26.8 unless otherwise stated:

| Check | Result |
|---|---|
| `gofmt` on changed Go files | **PASS** |
| `go test ./...` | **PASS** |
| `go test -race ./...` | **PASS** |
| `go vet ./...` | **PASS** |
| `./scripts/verify.sh` | **PASS** — formatting/syntax, full tests and race checks, browser control-plane and Live Visual Agent acceptance, bounded fuzz checks, Linux amd64/arm64 builds |
| `./scripts/smoke-local.sh` | **PASS** |
| `./scripts/build-release.sh v0.7.0` | **PASS** |
| `./scripts/test-install.sh` | **PASS** — installer end-to-end including checksum rejection and upgrade/config-preservation paths |
| `git diff --check` | **PASS** |

The measured aggregate statement coverage is **75.6%**, using `go test -coverprofile=... ./...` followed by `go tool cover -func=...`; this remains **9.4 percentage points below the stated 85% target**. No narrower package subset or executable 85% CI gate was found in the repository; this report treats the target as repository-wide. The Go test output separately identifies subpackages with no test files or 0% package coverage. The target is not met and must not be described as passing.

## Deployment boundaries

The bbolt session/audit backend and pending OIDC transactions are single-host/process facilities; use one instance or sticky routing through the full OIDC callback. Distributed session/audit coordination, SAML, automated identity provisioning/deprovisioning, and a live provider-specific IdP acceptance run remain outside this branch's verified scope.

## Acceptance status

**Implementation substantially delivered; WP4 acceptance remains pending.** Keep PR #224 in draft and do not merge or describe the work package as accepted until (1) repository-wide measured coverage reaches the documented 85% target and (2) fresh required GitHub checks complete successfully on the exact proposed head. The local functional, security, browser, fuzz, race, and build gates passed, but they do not waive the unmet coverage target or replace fresh remote checks.
