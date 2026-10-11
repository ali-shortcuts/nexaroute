# NexaRoute full completion checklist

**Program source:** user-provided `pasted_content_3.txt` (2026-10-11).  
**Baseline:** latest `main` at start of this program: `dcb9f9b`.  
**Rule:** every item begins as `NOT STARTED`; after each item is completed or verified, update its state, PR, evidence path, and notes. `PARTIAL` is temporary only. At a clean stop, remaining items must be reported plainly; this file is the source of truth.

| ID | Item | State | PR | Evidence path | Notes |
|---|---|---|---|---|---|
| M1-1 | Re-measure total coverage on current main with the exact required command; reconcile historical percentages by commit; reach ≥85.0% with behavior tests; each new package ≥90%; do not commit profile. | DONE | #249 | `docs/reports/M1_COVERAGE_2026-10-11.md` | Main `dcb9f9b`: 85.1%; `/tmp/c.out` not committed. `./scripts/verify.sh` PASS. |
| M1-2 | Competitor capability matrix for LiteLLM, Portkey, Kong AI Gateway, Bifrost, Envoy AI Gateway: official capability-specific deep links only, otherwise unverified; verified/unverified counts; no unsourced negative claims. | NOT STARTED | — | — | — |
| M1-3 | Repair benchmark logging/config; prove compiled Go mock, persistent connections, TCP_NODELAY, warmup, ≥1,000 requests at concurrency 1/16/64/256, default and tuned modes, cap 256 vs 32, streaming TTFT, RSS, p50/p95/p99, RPS; attempt pprof at c=256 and report cause if unavailable. | NOT STARTED | — | — | Prior failed run reported process exit 1; preserve diagnostics. |
| M1-4 | Fix the clear top benchmark hotspot with before/after proof, or record “none actionable” if no safe clear hotspot exists. | NOT STARTED | — | — | Depends on M1-3. |
| M1-5 | With default config, prove 64 concurrent requests reach mock upstream with observed in-flight >32; run under `-race`. | NOT STARTED | — | — | — |
| M1-6 | Add release workflow dispatch dry-run that builds and uploads/downloads artifacts without publishing; run it, or mark UNVERIFIED with exact boundary. | NOT STARTED | — | — | Only this item authorizes editing `release.yml`. |
| M1-7 | Audit every remote branch by ancestor/tree comparison against main; commit table and propose deletion only; delete nothing. | NOT STARTED | — | — | — |
| M1-8 | Match dependency policy to go.mod exactly (Go 1.24, go-oidc, go-jose, bbolt, oauth2, x/sys); pass check-deps and document local govulncheck use. | NOT STARTED | — | — | — |
| M2-1 | Verify/fix one title, consistent top offset ±2px, separated empty-state buttons, reachable nav at 390px, and yes/warning, no/safe, unknown/neutral badges in dark/light with screenshots. | NOT STARTED | — | — | — |
| M2-2 | Access page: virtual keys create/rotate/revoke/expire (shown once), teams/projects/tenants, roles, per-key model/route allowlists, RPM/TPM, IP/CIDR allowlist, privacy requirement; implement missing backend. | NOT STARTED | — | — | — |
| M2-3 | Budgets and Cost page: per key/team/project hard/soft budgets, usage/cost per request/day, CSV/JSON export, webhook alerts. | NOT STARTED | — | — | — |
| M2-4 | Policies and Guardrails page: view/edit policies, allow/redact/block/route-to-private actions, simulator output. | NOT STARTED | — | — | — |
| M2-5 | Observability page: per-deployment health, latency/error charts, SSE live view, Prometheus/Grafana links. | NOT STARTED | — | — | — |
| M2-6 | Audit log page: search, filters, tamper-evident status, export. | NOT STARTED | — | — | — |
| M2-7 | Evals/Scorecards page. | NOT STARTED | — | — | — |
| M2-8 | Playground: send a gateway request and show routing trace, candidates, privacy exclusions/count, chosen deployment/reason, tokens and cost. | NOT STARTED | — | — | — |
| M2-9 | Settings: edit route-profile privacy requirement; keep data-handling editor consistent. | NOT STARTED | — | — | — |
| M2-10 | Command palette Ctrl/Cmd+K, full keyboard navigation, consistent loading/empty/error/skeleton states, toasts, confirmation and undo for destructive actions. | NOT STARTED | — | — | — |
| M2-11 | WCAG 2.2 AA dark/light: contrast, focus, ARIA, reduced motion, screen-reader labels; automated axe scan with zero serious/critical violations on every page. | NOT STARTED | — | — | — |
| M2-12 | English/Persian/Pashto i18n; no hard-coded UI strings; RTL layout screenshots; persisted language switcher. | NOT STARTED | — | — | — |
| M2-13 | Embedded plain JS/CSS served by Go; no CDN/runtime font downloads; JS+CSS size-budget test; localhost first meaningful paint <1s measured. | NOT STARTED | — | — | — |
| M2-14 | Playwright tests/evidence for every page/flow at 1440×900, 768×1024, 390×844, dark/light, populated with ≥3 privacy-class providers, ≥2 routes and live mock traffic; commit screenshots; assert no console errors, no mobile overflow, axe zero serious, visual diffs. | NOT STARTED | — | — | — |
| M3-1 | Leak tests for every provider-content path (video submit/poll, admin test/discovery, decision provider, cache, hedging/racing, credential failover, affinity, retries/fallbacks, count_tokens, Responses, canonical path); document payloads and fix gaps. | NOT STARTED | — | — | Privacy gate is mandatory. |
| M3-2 | `nexaroute privacy audit`: report provider data-handling declarations, route/key requirements, zero-eligible route/key flags; never reveal secrets. | NOT STARTED | — | — | — |
| M3-3 | `config validate` and dashboard warning for no_training route/key with zero eligible deployments. | NOT STARTED | — | — | — |
| M3-4 | `docs/THREAT_MODEL.md` plus tests for SSRF/base_url, header injection, tool prompt-injection, key leakage, config tampering, privacy misdeclaration; verify private/link-local/metadata egress blocks. | NOT STARTED | — | — | — |
| M3-5 | Negative tests for CSRF, cookie flags, idle timeout, brute-force lockout, non-loopback admin refusal without auth; safe-default TLS and optional mTLS for client/admin listeners. | NOT STARTED | — | — | — |
| M3-6 | Tamper-evident hash-chain audit log and `nexaroute audit verify`. | NOT STARTED | — | — | — |
| M3-7 | Release-generated SBOM (CycloneDX/SPDX), checksums, signature; if signing keys unavailable, exact UNVERIFIED boundary; retain govulncheck gate. | NOT STARTED | — | — | — |
| M3-8 | Per-client hashed-at-rest client keys and per-client TPM/quotas identified in KNOWN_GAPS. | NOT STARTED | — | — | — |
| M4-1 | Register runtime Redis (shared counters/rate limits/leases/health/breakers) and Postgres (control-plane source of truth) clients; selectable configs work; default binary needs zero services; document trade-offs. | NOT STARTED | — | — | — |
| M4-2 | Real Redis/Postgres tests: contracts, crash/restart, lossless migration, backup/restore, two-replica budget/health correctness; otherwise identify exact UNVERIFIED capabilities. | NOT STARTED | — | — | — |
| M4-3 | Graceful drain, zero-downtime reload, `nexaroute doctor`, `config validate|diff|apply --dry-run`, backup/restore commands. | NOT STARTED | — | — | — |
| M4-4 | Helm/Kubernetes manifests with probes/resources/PDB/securityContext, distroless non-root image, systemd unit; test kind/minikube or state exact UNVERIFIED boundary. | NOT STARTED | — | — | — |
| M4-5 | Generate Admin OpenAPI spec and contract-test against implementation. | NOT STARTED | — | — | — |
| M4-6 | OpenTelemetry GenAI traces/metrics, Prometheus endpoint, TTFT/inter-token latency, bounded label cardinality, Grafana dashboard/alerts; structured body-free logs and optional redacted request store with retention. | NOT STARTED | — | — | — |
| M4-7 | Exercise real IdP (Keycloak/Dex), Vault transit dev server, and headless secret-service where possible; otherwise record exact UNVERIFIED boundaries. | NOT STARTED | — | — | — |
| M4-8 | Failure-injection/chaos/soak for provider timeouts, 429, 5xx, truncated SSE, slow clients, store outage; document each feature’s fail-open/fail-closed behavior. | NOT STARTED | — | — | — |
| M5-1 | Native Bedrock (stdlib SigV4), Vertex, Azure OpenAI, embeddings, rerank, image generation, audio transcription/TTS; preserve lossless tools/streaming for existing protocols. | NOT STARTED | — | — | — |
| M5-2 | Provider conformance suite with timeout, 429, 5xx, truncated/malformed SSE, slow-client injection; document lossy translation boundaries. | NOT STARTED | — | — | — |
| M5-3 | Quality-aware routing from live evidence/offline scorecards, deterministic explanations, cost-capped hedging, canary/shadow/weighted rollout, per-variant metrics, adaptive concurrency/load shedding/priority, jittered retry budgets, per-capability breakers; privacy gate and leak tests for all. | NOT STARTED | — | — | — |
| M5-4 | Opt-in embedding semantic cache: tenant isolation, TTL/size bounds, no tool/stream cache by default, threshold, config invalidation, provenance, privacy-class-aware; retain exact-match cache. | NOT STARTED | — | — | — |
| M5-5 | Prompt-cache passthrough/mapping for providers that support it. | NOT STARTED | — | — | — |
| M5-6 | Versioned price book, accurate or explicitly estimated token accounting, atomic budget reservation/settlement with -race property tests, cost-aware routing/spend caps, per-request Activity cost. | NOT STARTED | — | — | — |
| M6-1 | MCP gateway: registry, deny-by-default per-key/tenant tool allowlists, safe auth brokering, per-tool rate limits/timeouts/health, every-call audit, privacy awareness. | NOT STARTED | — | — | — |
| M6-2 | Restricted deterministic policy language (no arbitrary code), simulator, redacted opt-in traffic replay, draft→simulate→canary→active promotion, instant rollback/audit. | NOT STARTED | — | — | — |
| M6-3 | Guardrails for PII/secrets redaction, injection heuristics, size/token limits, blocked content, allow/redact/block/route-to-private; internal/guardrail coverage ≥90%. | NOT STARTED | — | — | — |
| M6-4 | Reproducible default/tuned benchmark suite, c=256 CPU/heap pprof, fix top hotspot; publish only NexaRoute data unless competitors share methodology/hardware. | NOT STARTED | — | — | — |
| M6-5 | Quickstart, Concepts, Security, Operations, provider guides, LiteLLM/Portkey migration, Troubleshooting, FAQ, tested examples, <2-minute first-run wizard, escaped snippets for Claude Code/OpenAI SDK/Anthropic SDK/curl. | NOT STARTED | — | — | — |
| M6-6 | After all gates pass, prepare v0.17.0 notes/artifacts locally; never tag or publish without operator approval. | NOT STARTED | — | — | — |

## Execution order

M1 → M3 → M2 → M4 → M5 → M6. Finish each item (tests, docs, evidence, checklist update) before starting the next. Do not merge PRs. Do not edit `scripts/verify.sh`. Do not edit workflow files except where explicitly allowed. Never touch issue #246. At every clean stop, report remaining states, raw gate results, risks, and next steps without claiming completion prematurely.
