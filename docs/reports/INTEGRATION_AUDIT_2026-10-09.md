# NexaRoute Integration and Security Audit

**Snapshot date:** 2026-10-09  
**Audited repository:** `ali-shortcuts/nexaroute`  
**Audited main SHA:** `c6307c9299ae084397f6dd9188a159b2c59d540c`  
**Working branch:** `chore/video-gateway-audit-hardening`  
**Scope:** reconcile PRs #211–#216, verify the current head, and record the first independently reproducible Video Gateway hardening results.

## Evidence captured

The current head was tested locally with Go `1.23.2`:

```text
go vet ./...                         PASS
go test -count=1 ./...              PASS
./scripts/verify.sh                 PASS
go test -race ./internal/video/...   PASS
go build ./cmd/videogen              PASS
git diff --check                     PASS
```

`./scripts/verify.sh` also reported passing browser acceptance, short fuzz checks, and Linux amd64/arm64 builds in this environment. The raw terminal output is retained by the execution environment; the repository does not claim an external CI result for this local run.

The coverage run was not interpreted as a repository-wide 85% result. Several packages remain below that target, and the newly added Video Gateway packages are not yet covered uniformly. This is an open coverage item, not a completed acceptance criterion.

## PR reconciliation

| PR | Current state | Head SHA | Base | Current checks | Finding |
|---|---|---|---|---|---|
| #211 | open | `825b99ae85b0dabc8433493fe6421ad6e6bcaf81` | `main` at `8aa9d37` | verify, govulncheck, CodeQL: success | Encrypted secrets at rest. Not integrated into current `main`. |
| #212 | open | `80da0c058cfb2a38a64d4450c013387b667e73b0` | `main` at `8aa9d37` | verify, govulncheck, CodeQL: success | TLS/mTLS, CSRF, configuration CLI. Must not be merged independently if #215 is used. |
| #213 | merged | `c004be845b0c1e40505df74d15927ffd1d736eb5` | `main` at `8aa9d37` | all reported checks: success | Client-auth body preservation is already on current main (`c6307c9`). |
| #214 | open | `f6625e44c8b02bfe0c990f71d64a52cff0ad53d4` | `phase1a-encrypted-secrets` | verify: success | Duplicate/stacked client-auth body-fix work; do not merge independently after #213. |
| #215 | open | `c986bdad515f954b1a00539222a6f6a972eae3e6` | `phase1a-encrypted-secrets` | verify: success | Integration stack for #211 + #212. This is the candidate path, but it must be rebased and re-tested on current main. |
| #216 | open draft | `f5f415f33d8f2b29df9206f4bc8936723a6e7720` | current main `c6307c9` | verify, govulncheck, CodeQL: success | Virtual-key/tenant policy intersection. Review and merge only after resolving draft status and testing against current auth behavior. |

## Recommended merge order

1. Preserve current `main` and do not merge #214; #213 already supplies the body-preservation fix.
2. Rebase or recreate #215 on current `main`, retaining the unique encrypted-secret implementation from #211 and the TLS/mTLS/CSRF/config-CLI implementation from #212. Do not merge #212 separately before the stack is reconciled.
3. Run the full integrated security suite on the rebased result, including secret migration, TLS transport, mTLS, CSRF, config CLI, client-auth body preservation, and Docker/runtime smoke tests.
4. Review #216 against the integrated auth implementation. Keep its unique regression tests, remove duplicate logic, resolve draft status, and require a fresh full verification run.
5. Merge only through reviewed pull requests. No direct push, force-push, release, or merge was performed by this audit.

## Current repository classification

### Implemented and locally verified

- Existing LLM gateway routing, protocol compatibility, provider health, failover, client-auth body preservation, browser acceptance, fuzz smoke checks, and release builds as exercised by `scripts/verify.sh`.
- Video domain models, request validation, fake asynchronous provider, bounded queue, in-memory idempotency store, durable single-process JSON store, polling orchestrator, cancellation, budget gate, local asset storage, SHA-256 metadata, FFmpeg normalize/concat helpers, standalone API handler, CLI skeleton, state-transition validation, and webhook HMAC/replay/deduplication helpers.
- The new Video Gateway package passes unit tests and race tests. It is not yet wired into the main NexaRoute HTTP server.

### Implemented but not independently verified against production dependencies

- PR #211 encrypted secret storage: the PR reports successful branch checks, but the implementation is not on current main and was not merged or re-tested here.
- PR #212 TLS/mTLS/CSRF/config CLI: the PR reports successful branch checks, but the implementation is not on current main and was not merged or re-tested here.
- PR #216 virtual-key/tenant intersection: the draft PR reports successful checks, but it was not integrated or re-tested with the current main head here.

### Partial or advisory

- Existing SSRF hardening is strong for the remote-decision client, but provider and proxy egress remains an operator-trusted boundary according to `SECURITY.md` and `docs/KNOWN_GAPS.md`.
- Existing budgets, usage, rate limits, and control-plane contracts are present, but distributed correctness depends on optional integrations and must be verified at runtime with PostgreSQL/Redis integration tests.
- Existing guardrails are bounded pattern/policy checks; they are not comprehensive semantic safety classifiers.
- Coverage has meaningful package-level gaps; a green test suite is not equivalent to the 85% target.

### Not implemented or not yet connected

- Video API registration in `cmd/gateway` and the primary `internal/httpapi.Server`.
- Durable multi-process Video job store, distributed leases, worker recovery, and external queue adapters.
- Full provider conformance suite and a verified real video-provider adapter.
- Video episode composition with audio mixing, narration, music ducking, Foley, and Persian RTL subtitle burn-in.
- OpenTelemetry/OTLP export for the new Video Gateway, complete admin surfaces, OpenAPI, and production CLI-to-gateway operation.

## Highest-priority follow-up

The next reviewable PR should integrate the security stack in the order above rather than adding a second implementation. Separately, the next Video PR should wire the standalone handler through an explicit feature flag and durable-store configuration, with an integration test proving that existing LLM routes are unchanged. Until those changes land and pass fresh integrated gates, the system must not be described as production-complete Video Gateway support.

## Subsequent integration checkpoint

On the audit branch, the optional Video Runtime was attached to `cmd/gateway` without changing existing LLM routes. `video.enabled=false` remains the default and creates no video handler or workers. When enabled, the gateway mounts `/v1/video/`, initializes the single-process durable JSON job store, starts bounded workers, and closes the queue during shutdown. The development fake provider is opt-in only and no real provider is claimed verified.

Fresh evidence after this integration:

```text
go test -count=1 ./...                         PASS
go vet ./...                                  PASS
go test -race -count=1 ./internal/video/... ./internal/config ./internal/httpapi ./cmd/gateway  PASS
./scripts/verify.sh                           PASS
git diff --check                               PASS
```

This closes the “standalone handler not mounted” gap, but does not close the multi-node store, real-provider, full episode composition, or production admin/API gaps listed above.
