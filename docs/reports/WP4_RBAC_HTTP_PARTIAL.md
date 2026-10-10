# WP4 — HTTP RBAC enforcement (partial)

## Implemented in this branch

- A server-side permission map covers every currently registered `/admin/api/*` route family.
- Unknown Admin API paths and unsupported method/permission combinations fail closed with HTTP 403.
- The existing static Admin API key and existing keyless loopback mode map to one explicit `legacy-admin-break-glass` owner identity. Role claims from request headers are ignored.
- State-changing Admin API actions and forbidden Admin requests emit credential-free audit log lines (method, quoted path, actor, status, request ID); request bodies and tokens are not included.
- Unit tests cover route permission mapping, unmapped paths, unsupported methods, unauthenticated identity denial, and middleware denial/owner compatibility.
- The branch has been refreshed with the current `main` using a merge commit (no rebase).

## Not complete

This is not a complete WP4 implementation. There is no viewer/operator/admin identity assignment, OIDC authorization-code/PKCE flow, issuer/audience/signature/expiry/nonce/state validation, server-side cookie session or revocation, or mock OIDC conformance test. Audit entries are application logs, not a durable structured audit store. The web UI still authenticates through the existing API-key flow. No real identity provider was tested.

## Verification after merging current main

`./scripts/verify.sh` — **PASS** on Go 1.26.8. This includes clean unit/integration tests, `go vet`, race tests, browser control-plane acceptance, Live Visual Agent browser acceptance, bounded fuzzing for HTTP JSON patching and Anthropic content parsing, and Linux amd64/arm64 builds of both binaries.

The project-wide coverage target of 85% is not part of `verify.sh`. A fresh post-merge `go test -coverprofile` measurement reports **75.6%**, below the 85% target.

## Acceptance status

**PARTIAL / NOT ACCEPTED as completion of WP4.** Keep PR #224 a draft and do not merge as WP4 completion. The remaining identity/session work needs a selected and explicitly configured identity-provider contract, a maintained OIDC implementation, explicit role-claim allow-list/mapping, secure server-side sessions integrated with existing CSRF, negative tests using an in-process OIDC provider, and a structured audit sink with tested privileged-action coverage.
