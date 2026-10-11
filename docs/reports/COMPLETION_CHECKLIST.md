# NOT COMPLETE — completion checklist

Repository: `ali-shortcuts/nexaroute`  
Baseline: `main` at `dcb9f9b3c0352d23f1dfa599ea58450b072406c0`  
Source: the operator-provided FULL COMPLETION PROGRAM.  
Status vocabulary: `NOT STARTED`, `IN PROGRESS`, `DONE`, `UNVERIFIED`, `UNSUPPORTED`.

This is the source of truth. Rows are updated after each item reaches a clean boundary. `DONE`, `UNVERIFIED`, and `UNSUPPORTED` require evidence/technical rationale as specified by the program. No item is represented as complete merely because it is listed here.

| ID | item | state | PR | evidence path | notes |
|---|---|---|---|---|---|
| M1-1 | Measure current total coverage, explain historical figures, reach 85% and new-package threshold | NOT STARTED | — | — | Exact required command |
| M1-2 | Complete sourced competitor capability cells/counts | NOT STARTED | — | — | Official deep links only |
| M1-3 | Repair and run benchmark harness; publish full measurements/profile | NOT STARTED | — | — | Preserve diagnostics |
| M1-4 | Fix actionable top benchmark hotspot or record none | NOT STARTED | — | — | Depends on M1-3 |
| M1-5 | Default-config concurrency integration test (>32 upstream in-flight) | NOT STARTED | — | — | Must pass under race |
| M1-6 | Release workflow dispatch dry-run upload/download without publish | NOT STARTED | — | — | Run or mark UNVERIFIED |
| M1-7 | Audit every remote branch for containment; propose, do not delete | NOT STARTED | — | — | Evidence table |
| M1-8 | Align dependency policy with go.mod; check-deps and govulncheck instructions | NOT STARTED | — | — | — |
| M2-1 | Verify/fix existing UI defects and dark/light badge states | NOT STARTED | — | — | Browser evidence |
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
| M3-1 | Privacy leak tests for every provider-egress/content path | NOT STARTED | — | — | Includes issue #246 exclusion |
| M3-2 | `nexaroute privacy audit` CLI | NOT STARTED | — | — | Never print secrets |
| M3-3 | No-eligible-deployment warnings in config validation/dashboard | NOT STARTED | — | — | — |
| M3-4 | Threat model with per-mitigation tests; SSRF/header/tool/key/config/privacy checks | NOT STARTED | — | — | — |
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
| M6-6 | Prepare v0.17.0 notes/artifacts locally after gates; no tag/publication | NOT STARTED | — | — | Operator approval required before tag/publish |

## Progress notes

- Bootstrap: checklist initialized from the full supplied item list before execution.
- Execution order: M1 → M3 → M2 → M4 → M5 → M6.
- Branches are never merged or deleted by this work; the operator performs merges.
