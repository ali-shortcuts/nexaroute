# NexaRoute LLM Gateway Capability-Parity Specification

**Status:** Proposed implementation specification; not evidence that any listed feature has been implemented.  
**Repository:** `ali-shortcuts/nexaroute`  
**Audience:** coding agents (Gemini CLI, Jules, OpenCode, human contributors)  
**Purpose:** evolve NexaRoute toward broad, production-grade capability parity with the useful capabilities represented by the 57 gateways/routers in the research inventory, without copying proprietary code, branding, or undocumented internals.

## 1. Mission

Implement a coherent, testable set of gateway capabilities inside the existing NexaRoute architecture. The desired outcome is **behavioral capability parity**, not a superficial list of checkboxes and not a literal clone of every vendor's service.

The 57 named projects are a discovery inventory, not 57 independent products to embed. Extract the underlying capabilities, verify claims against primary sources, consolidate duplicates, and implement the best coherent design natively in NexaRoute. Never claim exact parity with a hosted service where its internals or behavior are not publicly verifiable.

## 2. Mandatory instructions for coding agents

1. Read `AGENTS.md`, `ARCHITECTURE.md`, `CONTRIBUTING.md`, `docs/KNOWN_GAPS.md`, `docs/CONFIGURATION.md`, `docs/COMPATIBILITY.md`, `docs/SECURITY.md`, `docs/OBSERVABILITY.md`, and relevant package tests before planning code changes.
2. Inspect the current `main`, open PRs, and relevant issues before every implementation slice. Avoid duplicate or conflicting work. In particular, account for existing credential-pool editing work and the existing routing, control-plane, security, and media paths.
3. Build on current components. Do not create parallel routers, credential stores, policy engines, caches, metrics systems, or provider registries if a suitable implementation already exists.
4. Create a separate issue/PR-sized implementation slice for each phase below. Do not attempt a giant rewrite or mark every feature complete in one PR.
5. Write an implementation plan and acceptance tests before changing architecture. Record code paths and the evidence used to determine whether a capability already exists.
6. Use Go and existing repository conventions. Preserve compatibility by default; new configuration fields must have safe defaults, validation, documentation, and migration behavior where applicable.
7. Never expose secrets in API responses, logs, traces, metrics, events, errors, test snapshots, or generated reports. Do not weaken authorization, CSRF, audit, encryption, or fail-closed behavior.
8. Routing and protocol translation remain separate. Do not promise transparent failover after bytes have been committed to a streaming client.
9. All new behavior requires deterministic regression tests. Concurrency changes require race tests; routing/latency claims require reproducible benchmarks; reliability claims require repeatable stress/soak evidence.
10. Run `./scripts/verify.sh` when feasible. If blocked, run the narrowest meaningful tests and state exactly what was not run. Never fabricate verification.
11. Work on a unique branch and open a PR targeting `main`. Do not merge, release, close issues, or rewrite published history without explicit authorization.
12. Keep `docs/KNOWN_GAPS.md` accurate. A feature is not “implemented” merely because a config field, stub, UI control, interface, or documentation paragraph exists.

## 3. Existing baseline and design constraints

The current README describes an existing Go gateway with OpenAI/Anthropic-compatible ingress, OpenAI Responses, Gemini and other provider adapters, credential pools, per-deployment health, routing strategies, retries/failover, circuit breakers, request hedging, quota/cost-aware pressure handling, protocol translation, media passthrough, encrypted secret storage, local admin security, a web UI, and optional shared control-plane components. Confirm every statement against current code and tests before relying on it.

Preserve these architectural invariants:

- One validated runtime configuration and coherent provider/router snapshots.
- Deployment-level eligibility and health; a single broken model must not disable an entire provider.
- Provider selection must not be hidden inside protocol translators.
- Secret fields are write-only. Editing must not erase saved secrets that the user did not explicitly replace.
- Explicit cancellation, terminal-state fencing, ledger settlement, and reconciliation invariants must remain intact.
- Local/single-host behavior remains usable without mandatory Redis, PostgreSQL, Kubernetes, or hosted services.
- Optional distributed components must fail according to explicit documented policy; no silent unsafe fallback.
- OpenAI/Anthropic compatibility claims must be backed by contract tests and an explicit compatibility matrix.

## 4. Capability requirements

Each item is a requirement to **audit first** and then implement only the missing portion. Status vocabulary: `EXISTS`, `PARTIAL`, `MISSING`, `NOT_APPLICABLE`, `UNVERIFIED`. Do not pre-mark any item as complete.

### A. Routing, model selection, resilience

- **CAP-ROUTE-01 — Policy-based smart routing:** selectable policies for cost, latency/TTFT, quality proxy, availability, capability match, provider diversity, and weighted combinations. Explain score inputs; avoid pretending subjective “quality” is known without evaluations.
- **CAP-ROUTE-02 — Quality/cost router learning:** optional evaluation dataset and offline training/calibration workflow; versioned router policy; safe rollback; no online model updates from untrusted traffic by default.
- **CAP-ROUTE-03 — Tiered fallback:** explicit ordered tiers such as preferred/subscription, paid/low-cost, and free/local, with user-configurable eligibility, spend ceilings, provider allow/deny lists, and per-request opt-out. Never assume subscription APIs are legally or technically interchangeable with official provider APIs.
- **CAP-ROUTE-04 — Failover and retry:** classify retryable failures, honor `Retry-After`, bound attempts and total time, avoid retrying non-idempotent operations unless safe, and stop failover once a stream is committed.
- **CAP-ROUTE-05 — Routing safety:** capability-aware selection for tools, vision, reasoning, JSON/schema constraints, context size, streaming, and media; explicit behavior when no candidate meets requirements.
- **CAP-ROUTE-06 — Adaptive controls:** circuit breakers, health recovery, latency/failure EWMA, load/capacity pressure, session affinity, provider-diverse ordering, optional hedging, and deterministic tests for each transition.
- **CAP-ROUTE-07 — Virtual model endpoints:** stable public aliases and virtual endpoints independent of provider/model identifiers, with validated candidate pools, profiles, and fallback chains.
- **CAP-ROUTE-08 — Request repair:** opt-in, bounded preflight normalization/repair for well-defined malformed inputs. Preserve user intent, never silently alter prompts in ways that change semantics, and record redacted audit metadata.

### B. Credentials, identity, tenancy, budgets and billing

- **CAP-KEY-01 — Credential pools:** add/remove/reorder/index-update credentials; preserve unchanged encrypted secrets; explicit null/omitted/replace semantics; rotation, cooldown, per-key quota/health and safe selection. Regression-test partial edits and secret preservation.
- **CAP-KEY-02 — Virtual client keys:** scoped keys, digest-only storage where feasible, revocation, expiry, rotation, per-key RPM/concurrency/model/route restrictions, and last-used metadata without revealing raw keys.
- **CAP-IAM-01 — Multi-user access:** RBAC, organization/team boundaries, role-checked admin routes, audit trails, and least privilege. Treat SSO/OIDC interoperability and distributed identity as separate verifiable requirements.
- **CAP-BUDGET-01 — Budgets and hard limits:** hierarchical organization/team/user/key/project budgets, spend windows, alert thresholds, hard-stop vs soft-limit policies, reservations for in-flight work, and atomic concurrent accounting.
- **CAP-BILL-01 — Usage ledger and settlement:** provider-reported usage, estimated-usage flags, pricing snapshots, idempotent ledger events, reconciliation, adjustments, and explicit settlement-before-terminal-state ordering where required. Do not invent a provider price or exact token count.
- **CAP-BILL-02 — Billing integrations:** optional payment/provider integrations behind adapters; never make a payment processor mandatory for self-hosted core.
- **CAP-QUOTA-01 — Provider quota awareness:** per-credential/model quota observations, reset windows, cooldowns, and quota-aware routing with safe handling of missing/stale quota data.

### C. Token efficiency, caching and inference optimization

- **CAP-TOKEN-01 — Token counting:** native provider count endpoint when available; clearly marked estimate otherwise; per-model/tokenizer accuracy metadata.
- **CAP-COMP-01 — Safe context compression:** pluggable compression pipeline for tool output, logs, RAG chunks, and oversized context; support conservative and aggressive modes, allowlists/denylists, and before/after token estimates.
- **CAP-COMP-02 — Semantics protection:** compression must not alter code blocks, structured arguments, security instructions, citations, identifiers, or user-designated verbatim spans without explicit policy. Include adversarial and semantic-regression tests.
- **CAP-CACHE-01 — Response/prompt cache:** opt-in exact cache first, then semantic cache only as a separately gated feature; tenant/key isolation, TTL, invalidation, model/prompt/tool/schema-aware keys, privacy controls, and metrics.
- **CAP-CACHE-02 — Cache correctness:** never serve cross-tenant/private data; do not cache non-deterministic/tool-side-effect requests by default; test streaming behavior, errors, and stale invalidation.
- **CAP-INFER-01 — Local and hybrid inference:** support local endpoints and mixed hosted/BYOK/local routing through existing provider abstraction; expose locality and data-handling policies in route selection.

### D. Protocols, providers, tools and agents

- **CAP-PROTO-01 — Protocol compatibility:** maintain the documented matrix for OpenAI Chat Completions, Responses, Anthropic Messages, streaming, tool calls/results, reasoning fields, JSON/schema constraints, and cancellation. Explicitly document lossy cross-protocol translations.
- **CAP-PROTO-02 — Media APIs:** preserve bytes and provider semantics for embeddings, image, audio, rerank, file/batch, and video endpoints; model asynchronous jobs, polling, cancellation, terminal-state fencing, and cost settlement where applicable.
- **CAP-PROVIDER-01 — Provider adapters:** use a common adapter contract for authentication, request/response mapping, capability discovery, health checks, errors, usage/pricing metadata, and provider-specific extensions. Add providers through focused adapters rather than router conditionals.
- **CAP-PROVIDER-02 — Cloud providers:** audit AWS Bedrock, Google Vertex AI, Azure OpenAI, Gemini, and other existing provider support; prove auth/signing/region/project semantics in tests or mark interoperability unverified.
- **CAP-AGENT-01 — MCP gateway:** authenticated MCP endpoint/proxy, tool allowlists, per-tool authorization, input/output size limits, audit, rate limits, tenant isolation, timeout/cancellation, and redaction.
- **CAP-AGENT-02 — A2A/agent traffic:** investigate Agent-to-Agent protocol support as a separate phase; define identity, authorization, trust boundaries, replay protections, and resource limits before implementing.
- **CAP-AGENT-03 — Coding-agent support:** document tested integrations for Claude Code, Cursor, Codex, and OpenAI-compatible clients. Configuration examples must not imply an integration is tested if it is not.

### E. Observability, policy, governance and security

- **CAP-OBS-01 — End-to-end observability:** structured logs, request/trace correlation, Prometheus metrics, latency and TTFT histograms, provider/model/route labels, dashboards, alerts, and optional OTLP export.
- **CAP-OBS-02 — Privacy-safe telemetry:** no prompts, authorization headers, raw keys, secrets, or sensitive payloads by default; bounded cardinality; configurable redaction and retention.
- **CAP-GUARD-01 — Guardrails:** composable pre/post request policies for size, content, tool use, schema, PII/secrets, and provider allowlists. Define enforcement vs observe-only modes and audit outcomes.
- **CAP-GOV-01 — Rate/concurrency governance:** global, tenant, key, route, provider, and model limits; bounded queues; overload rejection; starvation and fairness tests.
- **CAP-SEC-01 — Admin/API security:** preserve server-side route permissions, CSRF/session controls, OIDC boundaries, audit durability, encrypted-at-rest secrets, write-only credentials, and fail-closed authorization.
- **CAP-SEC-02 — Data residency:** route constraints for region/provider locality, explicit user-visible policy, and tests that prove disallowed providers are never selected. Do not claim GDPR or residency compliance solely from a routing flag.
- **CAP-SEC-03 — Supply chain:** maintain dependency scanning, reproducible release verification, SBOM/signature checks, and least-privilege CI.

### F. Deployment, developer experience and integrations

- **CAP-OPS-01 — Deployment modes:** preserve a simple single-binary/self-hosted mode; optionally support Docker, systemd, Helm/Kubernetes, and air-gapped operation with documented secrets and upgrade/rollback steps.
- **CAP-OPS-02 — Distributed control plane:** audit file/memory/PostgreSQL/Redis support, multi-instance consistency, leases/locks, optimistic concurrency, migrations, backups, restore, and fail-closed modes. Do not advertise HA until chaos/recovery tests prove it.
- **CAP-OPS-03 — Performance:** reproducible baseline and load tests for throughput, p50/p95/p99 latency, TTFT, memory, connection reuse, and contention; compare changes to baseline and publish commands/results.
- **CAP-UI-01 — Admin UX:** provider/credential pool management, model discovery, route policies, budgets, usage ledger, health/quota views, audit, and guardrail configuration; all controls must map to enforced server-side behavior.
- **CAP-SDK-01 — Client compatibility:** document OpenAI-compatible base URL and auth, Anthropic base URL/auth, and supported SDKs. Avoid provider-specific claims not covered by integration tests.
- **CAP-INTEGRATION-01 — External platforms:** evaluate connectors/integrations (e.g. tracing, orchestration, model marketplaces) through optional adapters and official documented APIs; no hard dependency on a third-party hosted control plane.

## 5. Inventory mapping: all 57 reference entries

This is a source-discovery map, not a claim of verified feature parity. Validate each reference against official docs/source at implementation time, pin links to relevant files/versions, and label stale, archived, forked, or unverified claims.

| # | Reference | Capability family to evaluate |
|---:|---|---|
| 1 | LiteLLM | virtual keys, team budgets, auth and spend limits |
| 2 | 9Router | token reduction, tiered fallback |
| 3 | OmniRoute | context compression, model aggregation |
| 4 | kRouter | consolidated router features; validate fork-specific claims |
| 5 | Bifrost | Go gateway performance and benchmarks |
| 6 | Portkey | guardrails, hooks, observability |
| 7 | OpenRouter | hosted model/provider catalog and routing metadata |
| 8 | Cloudflare AI Gateway | edge caching and cache controls |
| 9 | Vercel AI Gateway | pricing transparency and BYOK |
| 10 | Envoy AI Gateway / Agent Router | Kubernetes CRDs and gateway routing |
| 11 | Kong AI Gateway | MCP traffic governance |
| 12 | TrueFoundry AI Gateway | VPC/air-gapped deployment |
| 13 | Requesty | custom routing policies |
| 14 | mnfst/llm-gateway | self-hosting and BYOK |
| 15 | Helicone | low-friction observability |
| 16 | Not Diamond | trained/custom model router |
| 17 | AMRouter | fork architecture; verify independent value |
| 18 | 9router-go | Go implementation/performance |
| 19 | Routerly | hierarchical budgets and limits |
| 20 | YoloRouter | OpenAI-compatible gateway, failover, key pool |
| 21 | Manifest | request autofix |
| 22 | Orq.ai | LLMOps evaluation/optimization workflows |
| 23 | ShareAI | hosted marketplace/distributed inference |
| 24 | OrcaRouter | automatic model picker and strategy policies |
| 25 | Concentrate | identity and source must be verified first |
| 26 | Arch / ArchGW | agent routing, guardrails, observability |
| 27 | Lynkr | coding-tool compatibility and tier routing |
| 28 | Claude Code Router | Claude Code-specific configurable routing |
| 29 | Weave Router | coding-agent cost/latency routing |
| 30 | ApiFlux | unified endpoint and failover |
| 31 | FastRouter.ai | hosted multi-model routing |
| 32 | Experiential Labs / Experiential | hybrid hosted/BYOK/local inference |
| 33 | Martian / RouterBench | learned routing and reproducible evaluation |
| 34 | Ramp Router | cost optimization under quality constraints |
| 35 | Baseten | open-weight inference deployment integration |
| 36 | Darkbloom | private/distributed inference; verify maturity and trust model |
| 37 | Inference Gateway | cloud-native proxy |
| 38 | agentgateway | MCP/A2A, RBAC and agent traffic |
| 39 | New API | billing, usage and payments |
| 40 | one-api | API-key distribution and account/billing management |
| 41 | Higress | extensible AI proxy/cache plugins |
| 42 | Apache APISIX | AI proxy, balancing and fallback |
| 43 | WSO2 AI Gateway | governance and content policies |
| 44 | Tyk AI Studio Proxy | centralized proxy and observability |
| 45 | Azure API Management AI Gateway | Azure policies, semantic cache, token rate limits |
| 46 | Amazon Bedrock Gateway | Bedrock integration and OpenAI-compatible facade |
| 47 | Alibaba Cloud AI Gateway / Higress | AI gateway and provider integration |
| 48 | Eden AI | multimodal API aggregation |
| 49 | Cortecs | EU residency/sovereign routing |
| 50 | EUrouter | EU-hosted model routing |
| 51 | Polyrouter | fallback and hard cost ceilings |
| 52 | UniterRouter (verify exact project identity) | unified endpoint and MCP integration |
| 53 | ZenMux | multi-provider multiplexing |
| 54 | TensorZero | gateway/observability/optimization; verify archived status before relying on it |
| 55 | freellmapi | free-tier aggregation and quota policy; review provider terms |
| 56 | CLIProxyAPI | CLI/subscription-to-API bridge; check provider terms and authorization |
| 57 | Headroom | context/tool-output compression |

## 6. Delivery phases and exit gates

### Phase 0 — Evidence-based baseline (required before feature work)
- Create a capability audit table with columns: ID, status, source code paths, tests, API/config/UI surfaces, gaps, upstream evidence, risks, dependencies, proposed PR.
- Inspect current open PRs/issues and assign ownership to avoid overlap.
- Validate the 57 inventory links and claims. Use official docs/repositories first; distinguish public source from hosted-only behavior. Mark missing/ambiguous/archived projects `UNVERIFIED`, not factual.
- Produce a dependency graph and a prioritized sequence. No production feature code in this phase unless needed to fix the audit tooling.

**Exit gate:** every CAP item has an evidence-based status and a bounded next action.

### Phase 1 — Correctness foundations
- Credential-pool edit semantics and secret preservation.
- Routing policy contracts, capability filtering, bounded retries/failover.
- Usage accounting, idempotent ledger/settlement, budgets and quotas.
- Tests for authorization, tenant boundaries, cancellation, terminal states, and error paths.

**Exit gate:** deterministic regression tests pass; no secret exposure; existing public contracts remain compatible.

### Phase 2 — Routing and cost controls
- Explicit policy interface and cost/latency/availability strategies.
- Hierarchical budgets, hard ceilings, reservations and quota-aware fallback.
- Transparent routing decisions with redacted explainability.
- Benchmarks for routing overhead and correctness under concurrency.

**Exit gate:** tests prove hard budget limits cannot be exceeded by concurrent requests beyond documented reservation semantics; route decisions are explainable and reproducible.

### Phase 3 — Efficiency
- Exact cache and safe compression pipeline first.
- Semantic cache only after tenant isolation, embedding/model versioning, invalidation and privacy review.
- Token estimation metadata and before/after savings metrics.
- Quality-preservation corpus and adversarial tests.

**Exit gate:** measurable token/cache benefit with no cross-tenant leak and documented quality trade-offs.

### Phase 4 — Guardrails and agent protocols
- Server-enforced policy engine and MCP proxy with tool authorization.
- A2A only after protocol scope and threat model are approved.
- Rate limits, payload bounds, audit, cancellation, timeout and replay protection.

**Exit gate:** negative authorization tests prove unauthorized tools/actions are rejected on the server, not merely hidden in UI.

### Phase 5 — Observability and multi-instance operations
- OTLP export, dashboards, alerts, safe cardinality and redaction.
- Validate distributed state backends, migrations, failover, recovery and restore.
- Stress/soak/chaos tests and deployment documentation.

**Exit gate:** reproducible telemetry and recovery evidence; no HA/residency claim without tests.

### Phase 6 — UX, integrations and release hardening
- Expose stable server-backed policies and usage/budget views in the UI.
- Add optional provider/platform adapters only where documented demand and maintenance ownership exist.
- Run full verification, security review, release checks, and update docs/known gaps.

**Exit gate:** full verification is green (or blockers precisely documented), API/config docs match behavior, and all claims have evidence.

## 7. Prioritization model

Score each missing capability from 1–5 for user value, security/correctness impact, demand evidence, and reuse across features; score implementation cost and operational risk from 1–5. Prioritize high value/reuse/security with manageable cost. Security and accounting correctness are gates, not optional polish. Do not prioritize a feature merely because a competitor advertises it.

Recommended default order:
1. correctness, secrets, authorization, usage/settlement and hard budgets;
2. routing policy transparency, quota-aware fallback and key-pool correctness;
3. observability and reproducible performance;
4. exact caching and safe compression;
5. guardrails and MCP;
6. advanced learned routing, semantic caching, A2A and specialized integrations.

## 8. Required artifacts for each implementation PR

Every feature PR must include:
- Problem statement and scope, with capability IDs.
- Existing implementation evidence and exact changed code paths.
- Design decisions, rejected alternatives and compatibility impact.
- Config/API/UI changes and migration/default behavior.
- Unit, regression, integration, race, fuzz, or benchmark tests as applicable.
- Exact verification commands and actual results.
- Security/privacy review and failure-mode behavior.
- Documentation updates and `docs/KNOWN_GAPS.md` updates.
- Follow-up issues for intentionally deferred work.

## 9. Completion tracking template

Copy this table into the audit report and fill it with evidence; do not infer status from README prose alone.

| Capability ID | Status | Code paths | Tests/evidence | Gap and next PR | Risk/notes |
|---|---|---|---|---|---|
| CAP-ROUTE-01 | UNVERIFIED | TBD | TBD | Audit then plan | Do not claim smart quality without evaluation |
| CAP-KEY-01 | UNVERIFIED | TBD | TBD | Audit current pool-edit work first | Secret preservation is mandatory |
| CAP-BUDGET-01 | UNVERIFIED | TBD | TBD | Audit control-plane accounting | Concurrent hard-limit correctness |
| CAP-BILL-01 | UNVERIFIED | TBD | TBD | Audit ledger/settlement | Idempotency and ordering |
| CAP-COMP-01 | UNVERIFIED | TBD | TBD | Audit before implementing | Preserve semantics |
| CAP-CACHE-01 | UNVERIFIED | TBD | TBD | Exact cache first | Tenant isolation |
| CAP-AGENT-01 | UNVERIFIED | TBD | TBD | Threat model + implementation slice | Server-side tool authorization |
| CAP-OBS-01 | UNVERIFIED | TBD | TBD | Audit current metrics/OTLP | Redaction/cardinality |
| CAP-SEC-02 | UNVERIFIED | TBD | TBD | Define enforceable locality rules | Routing flag is not compliance proof |
| CAP-OPS-02 | UNVERIFIED | TBD | TBD | Audit shared control plane | HA requires failure testing |

## 10. Definition of done

The overall initiative is complete only when:
- every capability requirement is implemented, explicitly deferred with a reason, or marked not applicable with evidence;
- every implemented claim is backed by code and automated tests;
- security, privacy, compatibility, and accounting invariants remain intact;
- performance and reliability claims have reproducible measurements;
- UI, API, config, docs, and `docs/KNOWN_GAPS.md` agree;
- all 57 references have a recorded validation status and source link or an explicit “identity/source not verified” note;
- no hosted-only, archived, fork-specific, subscription-based, or proprietary behavior is represented as guaranteed without evidence.

**Important:** This specification does not authorize copying third-party source code or bypassing provider terms. Reimplement capabilities from documented behavior and original NexaRoute code. It is a plan for staged engineering, not proof that the work is already done.
