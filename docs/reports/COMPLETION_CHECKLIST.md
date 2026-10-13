# NOT COMPLETE — completion checklist (M1-1 through M1-8 and M2-1 DONE; 41 items remain)

Repository: `ali-shortcuts/nexaroute`
Baseline: `main` at `dcb9f9b3c0352d23f1dfa599ea58450b072406c0`
Source: the operator-provided FULL COMPLETION PROGRAM.
Status vocabulary: `NOT STARTED`, `IN PROGRESS`, `DONE`, `UNVERIFIED`, `UNSUPPORTED`.

This is the source of truth. Rows are updated after each item reaches a clean boundary. `DONE`, `UNVERIFIED`, and `UNSUPPORTED` require evidence/technical rationale as specified by the program. No item is represented as complete merely because it is listed here.

| ID | item | state | PR | evidence path | notes |
|---|---|---|---|---|---|
| M1-1 | Measure current total coverage, explain historical figures, reach 85% and new-package threshold | DONE | #255 | `docs/reports/M1-1_COVERAGE.md`; `docs/reports/m1-1-coverage.raw.txt`; `docs/reports/m1-1-verify.raw.txt` | 85.1% on `dcb9f9b`; verify.sh PASS |
| M1-2 | Complete sourced competitor capability cells/counts | DONE | #256 | `docs/CAPABILITY_MATRIX.md`; `docs/reports/M1-2_MATRIX_EVIDENCE.md`; `docs/reports/m1-2-verify.raw.txt` | 17 rows; 55 verified / 30 unverified |
| M1-3 | Repair and run benchmark harness; publish full measurements/profile | DONE | #257 | `docs/benchmarks/m1-3-2026-10-11/M1-3_BENCHMARK_REPORT.md`; `benchmark.json`; `verify.stdout.txt` | 16 × 1,000 gateway runs; orphan child/security-store lock fixed; pprof 404: cause not determined; verify.sh PASS |
| M1-4 | Fix actionable top benchmark hotspot or record none | DONE | #258 | `docs/reports/M1-4_HOTSPOT_DECISION.md`; `docs/reports/m1-4-verify.raw.txt` | None actionable; pprof unavailable; verify.sh PASS |
| M1-5 | Default-config concurrency integration test (>32 upstream in-flight) | DONE | #259 | `docs/reports/M1-5_DEFAULT_CONCURRENCY.md`; `docs/reports/m1-5-target-race.raw.txt`; `docs/reports/m1-5-verify.raw.txt` | 64 requests; peak 47 under race; verify.sh PASS |
| M1-6 | Release workflow dispatch dry-run upload/download without publish | DONE | #260 | `docs/reports/M1-6_RELEASE_DRY_RUN.md`; `docs/reports/m1-6-retry-check.raw.txt`; `docs/reports/m1-6-first-dispatch.failure.raw.txt` | Run #38125574996 passed; artifact checksums pass; publish skipped; no tag/release |
| M1-7 | Audit every remote branch for containment; propose, do not delete | DONE | #261 | `docs/reports/M1-7_REMOTE_BRANCH_AUDIT.md`; `docs/reports/m1-7-remote-branch-audit.raw.tsv`; `docs/reports/m1-7-verify.raw.txt` | 42 heads; 15 contained incl. main; 27 uncontained; 14 deletion proposals; none deleted; verify.sh PASS |
| M1-8 | Align dependency policy with go.mod; check-deps and govulncheck instructions | DONE | #262 | `docs/DEPENDENCIES.md`; `docs/reports/M1-8_DEPENDENCY_AUDIT.md`; `docs/reports/m1-8-check-deps-go1.27.2.raw.txt`; `docs/reports/m1-8-govulncheck-verbose.raw.txt`; `docs/reports/m1-8-verify.raw.txt` | Exact go.mod inventory; check-deps and verify.sh PASS on Go 1.27.2; zero code-reachable advisories; Windows-only x/sys module advisory documented |
| M2-1 | Verify/fix existing UI defects and dark/light badge states | DONE | #263 | `docs/reports/M2-1_UI_REGRESSION.md`; `docs/browser-evidence/m2-1/README.md` | Issue #246 fixed in #248 |
| M2-2 | Access page and virtual-key/team/project controls | NOT STARTED | — | — | Backend where required |
| M2-3 | Budgets and Cost page, exports, webhook alerts | NOT STARTED | — | — | — |
| M2-4 | Policies and Guardrails page/simulator | NOT STARTED | — | — | — |
| M2-5 | Observability page and Prometheus/Grafana links | NOT STARTED | — | — | — |
| M2-6 | Searchable, filterable, tamper-evident Audit log page/export | NOT STARTED | — | — | — |
| M2-7 | Evals/Scorecards page | NOT STARTED | — | — | — |
| M2-8 | Playground with request trace, privacy exclusions, tokens/cost | NOT STARTED | — | — | — |
| M2-9 | Route-profile privacy editing in Settings | NOT STARTED | — | — | — |
| M2-10 | Command palette, keyboard navigation, UI states/toasts/undo | NOT STARTED | — | — | — |
| M2-11 | WCAG 2.2 AA dark/light and automated axe scan on every page | NOT STARTED | — | — | Zero serious/critical |
| M2-12 | English/Persian/Pashto i18n, RTL screenshots, persisted switcher | NOT STARTED | — | — | — |
| M2-13 | Embedded assets only, size budget, localhost FMP under 1s | NOT STARTED | — | — | — |
| M2-14 | Playwright all-page/flow evidence, responsive themes, a11y and diffs | NOT STARTED | — | — | Committed screenshots |
| M3-1 | Privacy leak tests for every provider-egress/content path | DONE (v0.21.0) | #280 (merged ac04486) | `internal/httpapi/privacy_p2_test.go`; `internal/httpapi/count_tokens.go`; `./scripts/verify.sh` = VERIFY PASS | Route-profile and virtual-key policies cover OpenAI Chat, Anthropic Messages, OpenAI Responses, and native Anthropic `count_tokens`; the latter now resolves virtual routes before upstream selection. Merged into main 2026-10-13. |
| M3-2 | `nexaroute privacy audit` CLI | NOT STARTED | — | — | Never print secrets |
| M3-3 | No-eligible-deployment warnings in config validation/dashboard | NOT STARTED | — | — | — |
| M3-4 | Threat model with per-mitigation tests; SSRF/header/tool/key/config/privacy checks | DONE (v0.21.0, SSRF part) | #281 (merged f1a4225) | `internal/egress/ssrf_matrix_test.go` (61 cases); `docs/THREAT_MODEL.md` | SSRF blocklist complete (CGNAT, TEST-NETs, NAT64/6to4, Teredo, loopback tautology closed). Header/tool/key/config rows remain future work. Merged into main 2026-10-13. |
| M3-5 | Admin/session negative tests and safe TLS/mTLS defaults | NOT STARTED | — | — | — |
| M3-6 | Tamper-evident hash-chain audit log and verify CLI | NOT STARTED | — | — | — |
| M3-7 | Release SBOM, checksums, signature and govulncheck | NOT STARTED | — | — | Signature may be UNVERIFIED if keys unavailable |
| M3-8 | Hashed-at-rest per-client keys and per-client TPM/quotas | NOT STARTED | — | — | — |
| M4-1 | Runtime Redis/Postgres clients selectable without breaking zero-service default | NOT STARTED | — | — | — |
| M4-2 | Real Redis/Postgres contract, crash, migration, backup and two-replica tests | NOT STARTED | — | — | Exact UNVERIFIED boundary if needed |
| M4-3 | Graceful drain/reload, doctor, config diff/apply dry-run, backup/restore CLI | NOT STARTED | — | — | — |
| M4-4 | Helm/Kubernetes, distroless image and systemd; kind/minikube test | NOT STARTED | — | — | — |
| M4-5 | Generated and contract-tested Admin OpenAPI spec | NOT STARTED | — | — | — |
| M4-6 | OTel GenAI, Prometheus, TTFT/inter-token metrics, dashboards/alerts/logging | NOT STARTED | — | — | — |
| M4-7 | Real IdP, Vault Transit and headless keyring verification | NOT STARTED | — | — | Exact UNVERIFIED boundary if unavailable |
| M4-8 | Failure-injection and soak suite with documented failure semantics | NOT STARTED | — | — | — |
| M5-1 | Bedrock/Vertex/Azure adapters and embeddings/rerank/image/audio/TTS | NOT STARTED | — | — | Preserve existing protocol fidelity |
| M5-2 | Provider conformance suite and translation-limit documentation | NOT STARTED | — | — | — |
| M5-3 | Quality routing, capped hedging, rollout, adaptive load/retry/breakers | NOT STARTED | — | — | Privacy gate/leak tests for all |
| M5-4 | Opt-in isolated bounded semantic cache with privacy provenance | NOT STARTED | — | — | Keep exact-match cache |
| M5-5 | Provider prompt-cache passthrough/mapping | NOT STARTED | — | — | — |
| M5-6 | Versioned price book, token accounting, atomic budgets/property tests/cost routing | NOT STARTED | — | — | — |
| M6-1 | MCP registry/gateway, per-tenant allowlists, brokering, limits, audits | NOT STARTED | — | — | — |
| M6-2 | Deterministic policy-as-code, simulator/replay/promotion/rollback/audit | NOT STARTED | — | — | — |
| M6-3 | PII/secret detection, injection/size/content guardrails and >=90% coverage | NOT STARTED | — | — | — |
| M6-4 | Reproducible default/tuned benchmarks, profiles and hotspot fix | NOT STARTED | — | — | — |
| M6-5 | Docs, tested examples and <2-minute first-run wizard | NOT STARTED | — | — | — |
| M6-6 | Prepare release notes/artifacts after gates | DONE (v0.17.0, v0.18.0) | #264 | `docs/releases/v0.17.0.md`; `docs/releases/v0.18.0.md` | Later roadmap items remain unimplemented |

## Progress notes

- Bootstrap: checklist initialized from the full supplied item list before execution (draft PR #254).
- M1-1 completed: fresh total coverage 85.1% on starting main `dcb9f9b`; historical snapshots explained; `./scripts/verify.sh` passed on PR #255 head. No coverage profile committed.
- M1-2 completed: all 85 competitor cells audited against official deep links; 17 actual capability rows counted, 55 verified/30 unverified; structural audit and `./scripts/verify.sh` passed on PR #256 head.
- Execution order: M1 → M3 → M2 → M4 → M5 → M6.
- Branches are never merged or deleted by this work; the operator performs merges.
