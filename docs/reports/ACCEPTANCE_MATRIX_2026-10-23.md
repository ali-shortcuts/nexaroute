# Acceptance Matrix — Final Completion Pass, 2026-10-23

Scope: the 2026-10-23 final engineering completion mission on top of
v0.24.0. Tree under acceptance: `main` at `79df87d5a869f7bf6926f9a26f733784a752f0bc` (after PRs #638,
#639, #640, #641). Umbrella gates on that exact tree: `scripts/verify.sh`
(local) and CI + Security workflows (GitHub Actions), plus the
Architecture & Quality Audit workflow run `39945085701` — see
`QUALITY_AUDIT_FINAL.md`, 2026-10-23 addendum, for the recorded
numbers (coverage `85.3%`).

Result vocabulary: **PASS** (executed evidence), **FAIL**,
**BLOCKED** (infrastructure/access unavailable), **NOT_RUN**,
**UNVERIFIED** (code-reviewed, no executed live evidence).

| # | Feature / contract | Implementation | Verification evidence | Result |
|---|---|---|---|---|
| 1 | Admin API authentication gate (fail-closed, incl. security-store outage → 503) and admin cookie security (`Secure` on all session/CSRF/OIDC cookies) | `internal/httpapi` admin plane; fix in PR #635 | Repo suite; CodeQL alerts 22–27 transitioned to `fixed` on the post-#635 scan; security-lane re-audit 2026-10-23 | PASS |
| 2 | OIDC login flow (PKCE, state, nonce, JWKS freshness, session rotation) | `internal/httpapi` OIDC handlers | Code-level audit 2026-10-23 (all checks present); live tenant login never executed | UNVERIFIED (live) |
| 3 | Client API keys (digest-only storage, constant-time compare) | `internal/httpapi` client auth | Security-lane audit 2026-10-23 | PASS |
| 4 | Config validation coherence (per-section validators; golden corpus; `evaluation.live_enabled` requires `evaluation.enabled`) | `internal/config` (PRs #636, #637) | `internal/config/validate_golden_test.go` — 57 cases incl. cross-section precedence; CI verify on both merge heads | PASS |
| 5 | Unknown-model fallback contract (fallback only for unknown models when enabled; known-but-unavailable never escapes) | `internal/router` (PR #589) | `internal/router` suite + `internal/httpapi/fallback_unknown_model_test.go`, re-run by the routing lane 2026-10-23 | PASS |
| 6 | No failover after response commit; mid-stream cut preserves partial content; client cancellation propagates upstream | `internal/httpapi` ingress loops, `statusWriter.Committed()` | `gameDayMidstreamKill`, `TestFailureInjectionMidStreamCut`, `TestProtocolMatrixE2EClientCancellation` — re-run by the routing lane 2026-10-23 | PASS |
| 7 | Protocol translation matrix (4 ingress × upstream families: text, streaming, tools, usage, stop reasons) | `internal/protocol/canonical`, `internal/translate` | `TestProtocolMatrix*` suite green 2026-10-23 | PASS |
| 8 | Fail-closed protocol decoding: built-in Responses tools/tool_choices rejected; `status:"failed"` is an upstream error; unsupported content parts rejected (never dropped/textualized); `previous_response_id` rejected; Chat error envelope in HTTP 200 rejected | `internal/protocol/canonical` (PR #640) | `internal/protocol/canonical/fail_closed_test.go` (7 tests + controls); e2e matrix case `openai_audio_part_to_gemini` (502, zero upstream calls); red proven by probes + assertion-level test failures on the pre-fix tree | PASS |
| 9 | Circuit-breaker sync across replicas (opt-in; remote `retired` = bounded 30 min cooldown demotion only; store outage never clears local truth; 10 s cadence) | `internal/breakersync`, ADR 0003 | Unit suite incl. `TestSyncOutageLeavesLocalStateUntouched`; PG/Redis two-instance integration suites run in CI | PASS (CI integration) |
| 10 | Config authority: local file is authoritative; control plane never holds config (ADR 0001) | `internal/config`, `cmd/gateway` | `check-ha-docs.sh`; `/api/hello` reports `ConfigSnapshot=ScopePerInstance` unconditionally | PASS |
| 11 | Video job durability — file backend (idempotency, lease recovery, terminal guards) | `internal/video/queue` file store, orchestrator | `internal/video/...` suite incl. deterministic idempotency race test | PASS |
| 12 | Video job durability — Postgres backend (atomic claim with DB clock, expired-lease recovery, terminal-replay guard) | `internal/video/queue/pg_store.go` (guard fix in PR #639) | CI `integration-postgres` job on PR #639 head `b0b5df4` (run 39939662334): SUCCESS; red on the tests-only head (run 39938301294): FAILED on `TestPostgresVideoStoreTerminalReplayAfterClaim`. Locally NOT_RUN — no PostgreSQL in the dev sandbox | PASS (CI) / NOT_RUN (local) |
| 13 | Video cost ledger exactly-once (reserve/settle state machine, restart-durable aggregates on Postgres; loud read-degradation) | `internal/video/cost` (F-08 logging in PR #639) | Unit suite + CI integration; degradation contract pinned by `TestPostgresLedgerAggregateReadsDegradeToZeroOnStoreError` | PASS |
| 14 | Video asset download integrity (recorded SHA-256 verified before serving) | `internal/video/api` (PR #639) | `TestHandlerDownloadVerifiesRecordedChecksum` — red (tampered file served, tests-only head) → green (fix head) in CI runs above | PASS |
| 15 | Video API bearer auth fails closed when auth is required but no token is configured | `internal/video/api` (PR #638) | `TestHandlerEmptyBearerTokenRejectsEverything`; merged as `37fa779e` with all checks green | PASS |
| 16 | SSRF defenses (dial-time IP validation, cross-origin redirect refusal) and request body caps | `internal/providers`, `internal/httpapi` | Security-lane audit 2026-10-23 (all scopes confirmed) | PASS |
| 17 | HA / state documentation coherence | `docs/HA_STATE_INVENTORY.md`, `docs/KNOWN_GAPS.md`, ADRs | `scripts/check-ha-docs.sh` green on the reconciled tree | PASS |
| 18 | Live external providers (OpenAI/Anthropic/Gemini tenants) | — | Zero paid calls by mission rule; loopback stubs only | NOT_RUN / UNVERIFIED |
| 19 | Live Bedrock / Vertex / Azure tenants; real OTLP collector; live K8s install; Vault / OS keyring round-trips; production soak | — | No credentials or clusters available; see `docs/EXTERNAL_VERIFICATION.md` status register | UNVERIFIED / BLOCKED |
| 20 | In-browser acceptance (axe, FCP, RTL screenshots) | `web/` | Sandbox Chromium cannot reach the loopback gateway (standing boundary); repo browser-evidence CI is the executed route | BLOCKED (local) |

## Notes

- Rows marked PASS rest on executed tests named in the evidence
  column, on the stated tree or on the PR head that merged into it.
- UNVERIFIED rows are deliberate: the mission forbids fabricating
  external evidence, and no live credentials, clusters, or paid
  provider calls were available or authorized.
- Two accepted-behavior notes: (a) half-open recovery under non-ready
  routing strategies admits concurrent trial requests (bounded by
  admission control and the provider semaphore) — there is no
  single-flight trial lease; documented in `docs/FAILURE_SEMANTICS.md`.
  (b) `PostgresStore.Update`'s terminal guard can only over-block,
  never under-block, by construction.
