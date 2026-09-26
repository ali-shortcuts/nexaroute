# Phase B — Virtual Endpoints, Route Profiles, Candidate Pools, Fallback Chains — Implementation Report (Converged)

Date: 2026-09-25 (convergence pass)
Branch: `arena/01a0d825-nexaroute`
Baseline: `ba91c531e13826a7d6637022fb2a805a4e709501`
Spec sections: §4, §5, §42, §46

---

## A. Final Architecture (verified)

```text
Client (Claude Code / OpenAI SDK / any HTTP)
  ↓
Public Virtual Model (e.g. nexa-code) — stable client identity
  ↓
Virtual Endpoint (ID, public_model, route_profile, protocols, enabled)
  ↓
Route Profile (ID, candidate_pool, fallback_chain) — inherits global routing strategy
  ↓
Primary Candidate Pool (ID, mode explicit|all, deployments[] list of deployment IDs/model IDs/aliases)
  ↓
Fallback Pools (ordered via FallbackChain)
  ↓
Existing NexaRoute Router — single routing core (router.Candidates)
  → Compatibility / Health / Provider Circuit / Context Fit / Capability Eligibility (authoritative)
  ↓
AllFilteredCandidates = pool ∩ eligible, ordered primary→fallback, deduplicated
  ↓
Existing Attempt / Hedging / Failover Loop (max_attempts hard bound)
  ↓
Actual Physical Deployment (provider/model) — upstream receives physical model, not virtual
```

No second router exists. Resolver is an index, not a router.

---

## B. Exact Structs Actually Present (from `internal/config/config.go`)

```go
type VirtualEndpointConfig struct {
  ID           string   `json:"id"`
  Name         string   `json:"name,omitempty"`
  Enabled      *bool    `json:"enabled,omitempty"` // nil = true
  PublicModel  string   `json:"public_model"`
  RouteProfile string   `json:"route_profile"`
  Protocols    []string `json:"protocols,omitempty"`
}

type RouteProfileConfig struct {
  ID            string `json:"id"`
  Name          string `json:"name,omitempty"`
  CandidatePool string `json:"candidate_pool"`
  FallbackChain string `json:"fallback_chain,omitempty"`
  Strategy      string `json:"strategy,omitempty"` // Phase B: only "" or "inherit" allowed; inherits global
}

type CandidatePoolConfig struct {
  ID          string   `json:"id"`
  Name        string   `json:"name,omitempty"`
  Mode        string   `json:"mode,omitempty"` // "explicit" (default) or "all"
  Deployments []string `json:"deployments,omitempty"`
}

type FallbackChainConfig struct {
  ID    string   `json:"id"`
  Name  string   `json:"name,omitempty"`
  Pools []string `json:"pools"`
}
```

Added to `Config` as `VirtualEndpoints`, `RouteProfiles`, `CandidatePools`, `FallbackChains` (omitempty). No `Description`, `CreatedAt`, `UpdatedAt` fields — those were removed as fabricated claims. Only fields above exist.

---

## C. Exact Config Semantics (Phase B)

### Defaults & Migration

- `ApplyDefaults()`:
  - Trims IDs, names, models.
  - Pool mode lowercased, default `explicit`.
  - VE `Enabled` nil → true.
  - RouteProfile `Strategy` lowercased, `"inherit"` normalized to `""` (empty = inherit global).
  - Legacy `routing.public_model` (PR #13) → if set and no VEs defined, synthesizes default pool (`mode=all`), default profile, default VE with that public model. This preserves single-endpoint primitive as degenerate VE.

### Validation (authoritative)

- Public model: 1..128 bytes, no spaces/tabs/quotes/`$`/`\`, not `auto`/`claude-auto`, unique, must not collide with any physical deployment ID/model/alias (including suffix match via physicalIDs map).
- IDs: 1..256 bytes, `validLocalID` = letters/digits/dot/underscore/hyphen, unique per type.
- Pool: mode must be `explicit` or `all`, deployments each 1..256 bytes, no invalid chars, max 4096 entries, max 256 pools.
- Fallback chain: at least 1 pool, max 32 pools, no duplicate pool, each pool must exist, max 256 chains.
- Route profile: ID required, candidate_pool required and must exist, fallback_chain if set must exist, **strategy must be empty or "inherit" in Phase B** — any other value rejected with message "strategy must be empty or inherit in Phase B (per-profile strategies deferred to Phase E)".
- Virtual endpoint: ID, public_model, route_profile required, protocols max 16, each protocol 1..64 bytes letters/digits/hyphen/underscore, public_model unique.
- Limits: maxVirtualEndpoints 256, maxRouteProfiles 256, maxCandidatePools 256, maxFallbackChains 256.

### Reference Integrity on CRUD

- `DELETE /candidate-pools/{id}` → 409 if any RouteProfile references it as candidate_pool OR any FallbackChain references it.
- `DELETE /fallback-chains/{id}` → 409 if any RouteProfile references it.
- `DELETE /route-profiles/{id}` → 409 if any VirtualEndpoint references it.
- Failed mutation does not save: `mutateConfig` clones, applies fn, then `applyConfigLocked` validates before `SaveAtomic`. If validation fails or fn returns error, file not written, in-memory config unchanged, resolver not rebuilt.

### Protocol Restrictions (exact, documented)

- Empty protocols list → allow all ingress protocols.
- `"anthropic"` → only `/v1/messages`
- `"openai"` → only `/v1/chat/completions` (exact, does NOT implicitly allow responses)
- `"openai_responses"` or alias `"responses"` → only `/v1/responses`
- Matching is case-insensitive, normalized: `"responses"` alias → `"openai_responses"`.
- If operator wants both chat and responses, they must list both `["openai","openai_responses"]`.
- Disabled VE → 404, protocol not allowed → 400 (vs 503 for no healthy deployment).

---

## D. Exact API Routes (from `server.go` Handler)

```
GET  /admin/api/virtual-endpoints
POST /admin/api/virtual-endpoints
GET  /admin/api/virtual-endpoints/{id}
PUT  /admin/api/virtual-endpoints/{id}
DELETE /admin/api/virtual-endpoints/{id}

GET  /admin/api/route-profiles
POST /admin/api/route-profiles
GET  /admin/api/route-profiles/{id}
PUT  /admin/api/route-profiles/{id}
DELETE /admin/api/route-profiles/{id}

GET  /admin/api/candidate-pools
POST /admin/api/candidate-pools
GET  /admin/api/candidate-pools/{id}
PUT  /admin/api/candidate-pools/{id}
DELETE /admin/api/candidate-pools/{id}

GET  /admin/api/fallback-chains
POST /admin/api/fallback-chains
GET  /admin/api/fallback-chains/{id}
PUT  /admin/api/fallback-chains/{id}
DELETE /admin/api/fallback-chains/{id}

GET  /admin/api/endpoint   (legacy PR #13 shim, no-store)
POST /admin/api/endpoint   (legacy, creates/updates VE + gateway key nx_*)
```

All admin routes have `Cache-Control: no-store` + `Pragma: no-cache` via middleware. Legacy endpoint also sets no-store explicitly.

---

## E. Exact `/v1/models` Behavior

- Virtual endpoints listed first, each as `{"id": public_model, "object":"model", "created": now, "owned_by":"gateway", "virtual_endpoint": ve.ID}`.
- `auto` and `claude-auto` listed when there are deployments OR virtual endpoints (previously only when deployments>0).
- Physical deployments follow: IDs = deployment ID, upstream model, aliases (deduped).
- Legacy `routing.public_model` still listed when resolver nil.
- Tests: `TestVirtualEndpointModelsDiscovery` verifies virtual models + auto present.

---

## F. Fallback + Max-Attempt Semantics (verified)

- `Resolver.AllFilteredCandidates(candidates, resolved)` concatenates:
  1. Filter primary pool: `FilterCandidates(allCandidates, primaryAllowed, primaryMode)`
  2. For each fallback pool in chain order: filter and append if not already seen (dedup).
  - Preserves router ordering inside each pool.
  - Duplicate deployment in two pools attempted once (TestFallbackChainDeduplication).
- `candidatesForRequirement` fetches `rt.Candidates(reqAll)` where `reqAll.Model=""` (auto) to get all eligible deployments, then `AllFilteredCandidates`.
- Attempt loop in `openai.go`/`anthropic.go`/`canonical_path.go` respects `routing.max_attempts` hard bound: `max = min(cfg.MaxAttempts, len(candidates))`. Even with 3 pools and MaxAttempts=2, only 2 attempts (TestFallbackChainMaxAttemptsSemantics).
- Fallback chain cannot create infinite attempts: bounded by candidate list length and max_attempts.
- Hedging: `doAttemptWithHedge` races primary vs next eligible candidate; hedge counts as attempt budget (existing behavior), and candidate list is already pool-union, so hedging cannot jump outside configured pool union.
- Documented: max_attempts remains hard upper bound; fallback chain completion is best-effort within budget.

---

## G. Session-Affinity Semantics

- Session affinity pins **physical deployment ID** (`provider-a/model-a`), not virtual public model.
- Pin key = hash(session_id) + model + providerType + scopes (existing `affinityBucket`).
- For virtual endpoints, eligibility uses `reqEligible.Model=""` so pin lookup works with physical deployment.
- Test: `TestVirtualEndpointSessionAffinityPinsActualDeployment` verifies 3 requests with same session_id pin to same physical deployment (or at least >=2 to same).
- Failed first deployment is NOT recorded as success: `recordRouteSuccess` only called after actual execution success. Test `TestVirtualEndpointSuccessState` verifies first 500 fails, second succeeds, success state is second deployment.

---

## H. Actual Verification Commands and Outputs (Phase B convergence)

### Full gate

```bash
./scripts/verify.sh
```

Output (truncated, final lines):

```
== go version ==
go version go1.23.9 linux/amd64
== shell syntax ==
== formatting ==
== unit/integration tests ==
ok  github.com/ali-shortcuts/nexaroute/cmd/gateway 0.009s
ok  github.com/ali-shortcuts/nexaroute/internal/cache 0.106s
ok  github.com/ali-shortcuts/nexaroute/internal/compat 0.041s
ok  github.com/ali-shortcuts/nexaroute/internal/config 0.109s
ok  github.com/ali-shortcuts/nexaroute/internal/core 0.004s
ok  github.com/ali-shortcuts/nexaroute/internal/events 0.637s
ok  github.com/ali-shortcuts/nexaroute/internal/health 1.199s
ok  github.com/ali-shortcuts/nexaroute/internal/httpapi 25.010s
ok  github.com/ali-shortcuts/nexaroute/internal/logging 0.366s
ok  github.com/ali-shortcuts/nexaroute/internal/probe 12.666s
ok  github.com/ali-shortcuts/nexaroute/internal/protocol/canonical 0.018s
ok  github.com/ali-shortcuts/nexaroute/internal/providers 2.562s
ok  github.com/ali-shortcuts/nexaroute/internal/route 0.005s
ok  github.com/ali-shortcuts/nexaroute/internal/router 0.884s
ok  github.com/ali-shortcuts/nexaroute/internal/translate 0.012s
ok  github.com/ali-shortcuts/nexaroute/internal/usage 0.003s
== go vet ==
== race detector ==
ok  github.com/ali-shortcuts/nexaroute/cmd/gateway 1.016s
ok  github.com/ali-shortcuts/nexaroute/internal/cache 1.045s
...
ok  github.com/ali-shortcuts/nexaroute/internal/httpapi 10.490s
...
== web ui javascript syntax ==
== short fuzz checks ==
...
== linux amd64 build ==
== linux arm64 build ==
VERIFY PASS
```

Result: **PASS**

### Stress gate

```bash
./scripts/stress.sh
```

Output:

```
== router scale stress ==
ok  github.com/ali-shortcuts/nexaroute/internal/router 0.313s
== probe/recovery stress ==
ok  github.com/ali-shortcuts/nexaroute/internal/probe 0.169s
== event-state stress ==
ok  github.com/ali-shortcuts/nexaroute/internal/events 0.184s
== HTTP admission stress ==
ok  github.com/ali-shortcuts/nexaroute/internal/httpapi 0.050s
== concurrent log rotation stress ==
ok  github.com/ali-shortcuts/nexaroute/internal/logging 0.072s
STRESS PASS
```

Result: **PASS**

### Smoke gate

```bash
./scripts/smoke-local.sh
```

Output:

```
PASS embedded Web UI (200)
PASS runtime hello (200)
PASS model list (200)
PASS admin snapshot (200)
PASS count_tokens fallback (200)
PASS provider create (201)
PASS provider reveal (200)
PASS provider edit (200)
PASS provider re-open (200)
PASS provider redacted read (200)
PASS provider delete (200)
PASS backup-free atomic config persistence
SMOKE PASS — local runtime, UI, admin persistence and token-count fallback are operational.
```

Result: **PASS**

### Targeted verification

```
go test -run "TestVirtual|TestRouteProfile|TestEligible|TestProtocol|TestReference|TestLegacy|TestHotReload|TestFallback" -v ./internal/httpapi
```

- TestVirtualEndpointResolvesCorrectly PASS
- TestVirtualEndpointDisabled PASS
- TestVirtualEndpointUnknown PASS
- TestVirtualEndpointModelsDiscovery PASS
- TestVirtualEndpointFallbackChain PASS
- TestVirtualEndpointSessionAffinityPinsActualDeployment PASS
- TestVirtualEndpointSuccessState PASS
- TestVirtualEndpointProtocol PASS
- TestVirtualEndpointBackwardCompatibility PASS
- TestVirtualEndpointAdminCRUD PASS
- TestVirtualEndpointClientAuth PASS
- TestVirtualEndpointHotReload PASS
- TestVirtualEndpointUpstreamModelIsPhysical PASS
- TestRouteProfileStrategyValidation PASS
- TestEligibleDeploymentsSemanticsCorrection PASS
- TestProtocolExactMatching PASS (openai exact, responses alias, both)
- TestReferenceIntegrity PASS (pool→profile, chain→profile, profile→VE, collision atomicity)
- TestLegacyEndpointSecurity PASS (no-store, no leak, rotation preserves unrelated, client auth alone 401)
- TestHotReloadCoherence PASS (resolver/router coherent generation)
- TestFallbackChainMaxAttemptsSemantics PASS (max_attempts hard bound)
- TestFallbackChainDeduplication PASS

`go test ./internal/config -run TestRouteProfileStrategyMustNotLie` PASS

---

## I. Full-Gate Results Summary

- `gofmt -l` clean
- `go test -shuffle=on -count=10 ./...` PASS
- `go vet ./...` PASS
- `go test -race -shuffle=on -count=3 ./...` PASS
- `node --check internal/httpapi/web/app.js` PASS
- fuzz checks PASS (2s each)
- linux amd64 + arm64 builds PASS
- stress PASS
- smoke-local PASS
- working tree: intended files only (see git status)

---

## J. Known Limitations (intentional Phase B)

- Route Profiles inherit global routing strategy; per-profile strategy override deferred to Phase E.
- Pool member counts are configured counts, not runtime eligible counts (runtime eligibility depends on health, capabilities, provider circuits, etc.). True eligibility Dry Run deferred.
- No Task Analyzer, DecisionProvider, Jev, evaluation, shadow, canary, learning, supervision — all deferred per spec.
- Protocol list empty = allow all; otherwise exact matching required (no hidden family expansion).
- Dashboard editors are prompt-based, not full forms (same as minimal Phase B).
- No persistence beyond config.json (runtime/eval in-memory).

---

## K. Deferred Phase C Functionality

Per task: DO NOT create `internal/feature`, TaskProfile, TaskAnalyzer, DecisionProvider, DecisionOrchestrator, Jev, LLM Judge, Scorecards, Evaluation Engine, Shadow, Canary, Learned Router, Supervisor. This report confirms none exist.

---

## L. Files Changed (final)

- `internal/config/config.go` — Phase B types, ApplyDefaults synthesis, Validate with strategy restriction to empty/inherit, reference integrity
- `internal/route/resolver.go` — NEW, pool expansion, ResolvedRoute, FilterCandidates, AllFilteredCandidates
- `internal/httpapi/server.go` — resolver wiring, candidatesForRequirement exact protocol matching, Handler routes, removal of dead EligibleIgnoreModel helpers
- `internal/httpapi/models.go` — virtual first, auto when VE or deployments
- `internal/httpapi/openai.go`, `anthropic.go`, `canonical_path.go` — VE-aware, reqEligible Model=""
- `internal/events/bus.go` — VE fields
- `internal/httpapi/virtual.go` — NEW CRUD + reference integrity 409 + pool_member_count naming + legacy shim secure
- `internal/httpapi/admin.go` — snapshot uses pool_member_count, not eligible
- `internal/httpapi/web/index.html` — headers Pool Members (configured), strategy inherits global note
- `internal/httpapi/web/app.js` — render uses pool_member_count, strategy shows inherit(global), editProfile no longer prompts for strategy, subtitles corrected
- `internal/router/router.go` — EligibleIgnoreModel removed (dead)
- `internal/config/virtual_test.go`, `internal/route/resolver_test.go`, `internal/httpapi/virtual_test.go` — comprehensive tests including strategy lie, eligible semantics, protocol exact, reference integrity, security, hot-reload coherence, max_attempts, dedup, upstream physical model

No second router, no second dashboard, no external deps.
