# Phase B — Virtual Endpoints, Route Profiles, Candidate Pools, Fallback Chains — Implementation Report

Date: 2026-09-25
Branch: `arena/01a0d825-nexaroute`
Baseline: `ba91c531e13826a7d6637022fb2a805a4e709501`
Spec sections: §4, §5, §42, §46 (virtual endpoints, route profiles, candidate pools, fallback chains, config extensions)

---

## 1. Summary

Phase B introduces **stable client-facing identities** independent of physical backend pools:

- **Virtual Endpoints** (public model names like `nexa-code`) → Route Profile → Candidate Pool → existing router eligibility.
- **Route Profiles** reusable intent objects (candidate pool ref, fallback chain ref, optional name).
- **Candidate Pools** named sets of deployments (mode `explicit` = list of deployment IDs/model IDs/aliases/suffix, or `all` = every deployment).
- **Fallback Chains** ordered list of pool IDs to try when primary pool has no healthy candidates or all attempts fail at runtime.

No second router was created. The existing `internal/router` remains the single authoritative eligibility core. Phase B adds a **resolver layer** (`internal/route`) that expands pools against current deployments and filters the router's candidate list. Invalid/unknown candidates are rejected, never silently reinserted.

All changes are additive with safe defaults: existing configs without virtual endpoints load unchanged and routing behavior is byte-equivalent when no virtual endpoints are configured (Decision OFF equivalent).

---

## 2. Config model (`internal/config/config.go`)

### New types

```go
type VirtualEndpointConfig struct {
  ID, Name, PublicModel, RouteProfile, Description string
  Protocols []string // e.g. ["openai","anthropic"] or ["openai_responses"]
  Enabled *bool
  CreatedAt, UpdatedAt int64
}

type RouteProfileConfig struct {
  ID, Name, Description, CandidatePool, FallbackChain string
  Strategy string // optional override (not enforced yet, reserved)
  CreatedAt, UpdatedAt int64
}

type CandidatePoolConfig struct {
  ID, Name, Description, Mode string // "explicit" or "all"
  Deployments []string // explicit members
  CreatedAt, UpdatedAt int64
}

type FallbackChainConfig struct {
  ID, Name, Description string
  Pools []string // ordered pool IDs
  CreatedAt, UpdatedAt int64
}
```

Added to `Config` as slices: `VirtualEndpoints`, `RouteProfiles`, `CandidatePools`, `FallbackChains`.

### Defaults & migration

- `ApplyDefaults()` lowercases pool modes, trims IDs, and **synthesizes** a default virtual endpoint / pool / profile from legacy `routing.public_model` if present (PR #13 compatibility). This preserves the single-endpoint primitive as a degenerate virtual endpoint.
- Empty new sections → no virtual endpoints, existing `auto`/`claude-auto` + physical models continue to work.
- `cloneConfig()` deep-copies new slices and pointer fields.

### Validation

- Public model: `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, 1..128 chars, not `auto`/`claude-auto`/`""`, no spaces.
- Duplicate `public_model` rejected (case-sensitive).
- Collision with physical deployment ID/model/alias/suffix → rejected with clear error ("collides with existing deployment").
- Route profile must reference existing candidate pool; fallback chain must reference existing pools, no duplicate pool in chain, at least one pool.
- Pool mode must be `explicit` or `all`; explicit may be empty (means no deployments).
- Virtual endpoint must reference existing route profile.
- All IDs: 1..128 chars, no slash.
- Existing hard limits (providers, models, etc.) unchanged.

---

## 3. Resolver (`internal/route/resolver.go` — NEW)

- `Resolver` holds indexes: `byPublicModel`, `byID`, `profiles`, `pools`, `chains`, plus `expanded` map `poolID → set[deploymentID]`.
- Expansion: for each pool entry (deployment ID, model ID, alias, or suffix `/<model>`), match against all current deployments via `matchesPoolEntry`. `all` mode expands to all deployment IDs.
- `Resolve(model) → (ResolvedRoute, bool)`: looks up virtual endpoint by public model, builds `ResolvedRoute`:
  - `VirtualEndpointID`, `PublicModel`, `RouteProfileID`, `PrimaryPoolID`, `PrimaryMode`, `OrderedPoolIDs` (primary + fallback chain), `AllowedDeployments` (primary set), `FallbackAllowed` (parallel to fallback pools), `AllAllowed` union, `Pools` map for UI.
- `ResolveByID`, `ListVirtualEndpoints/Profiles/Pools/Chains`, `GetExpanded`.
- Filtering:
  - `FilterCandidates(candidates, allowed, mode)`: if mode `all` and allowed nil → return all; empty allowed → nil.
  - `ResolveCandidates`: returns first non-empty filtered list (for health-only fallback).
  - `AllFilteredCandidates`: concatenates candidates from all pools in order, deduplicated, preserving router ordering within each pool — allows runtime failover from primary to fallback when primary's healthy candidates fail at request time.

Resolver is rebuilt on every config reload (`applyConfigLocked`).

---

## 4. Server wiring (`internal/httpapi/server.go`)

- `Server` holds `routeResolver *route.Resolver`, built at `New()` and rebuilt on config swap.
- `resolveVirtualEndpoint(model)` → checks resolver, returns disabled error if `Enabled=false`.
- `candidatesForRequirement(req, protocol)`:
  - Snapshots `cfg` (providers stripped) + `rt` + `resolver` under `runtimeMu`.
  - Resolves virtual endpoint. If disabled → return error (caller maps to 404). If protocol list present and request protocol not allowed → return error (caller maps to 400).
  - If not virtual → `rt.Candidates(req)` (existing path).
  - If virtual → `rt.Candidates(reqAll)` where `reqAll.Model=""` (auto) to get all eligible deployments, then `AllFilteredCandidates` to enforce pool ∩ eligibility. This ensures `no candidate outside pool`.
- `Handler()` registers 9 new routes:
  - `GET/POST /admin/api/virtual-endpoints` and `GET/PUT/DELETE /admin/api/virtual-endpoints/{id}`
  - `GET/POST /admin/api/route-profiles` and `.../{id}`
  - `GET/POST /admin/api/candidate-pools` and `.../{id}`
  - `GET/POST /admin/api/fallback-chains` and `.../{id}`
  - Legacy `POST /admin/api/endpoint` shim (PR #13) → creates virtual endpoint + gateway key `nx_<random>`.

### Eligibility fix for virtual endpoints

- Router's `eligibleDeployment` already has `ignoreModel` param. Added `EligibleIgnoreModel`.
- Handlers now use `reqEligible` with `Model=""` when virtual, for both `currentRouteCandidate` and `build*Attempt`, so that virtual public model does not cause model-matching rejection. Physical model routing still checks model.

---

## 5. Ingress paths

All three ingresses updated to use `candidatesForRequirement`:

- `openai.go` (`/v1/chat/completions`): sets `X-Gateway-Virtual-Endpoint/Public-Model/Route-Profile` headers, enriches `route_ok` event with VE fields.
- `anthropic.go` (`/v1/messages`): same.
- `canonical_path.go` (`/v1/responses`): same, with `openai_responses` protocol.
- Disabled VE → 404, protocol mismatch → 400 (vs 503 for no healthy deployment).

---

## 6. Models discovery (`internal/httpapi/models.go`)

- Virtual endpoints listed first, each with `virtual_endpoint` ownership field.
- `auto`/`claude-auto` listed when there are deployments OR virtual endpoints (previously only when deployments >0).
- Physical deployments follow.
- Legacy `routing.public_model` still supported when resolver nil.

---

## 7. Events (`internal/events/bus.go`)

- `Event` extended with `VirtualEndpoint`, `PublicModel`, `RouteProfile`, `Pool` (256 char bounds, same sanitization as other fields). `route_ok` events now carry virtual endpoint identity for observability.

---

## 8. Admin CRUD (`internal/httpapi/virtual.go` — NEW)

- Full CRUD for VEs, profiles, pools, chains, with `no-store` cache header.
- Validation delegates to `config.Config.Validate()` after mutation.
- `adminEndpoint` legacy shim: creates VE with `nx_` key, returns `{model, api_key, message}`.
- All handlers use `mutateConfig` → atomic save → hot reload.

---

## 9. Admin snapshot (`internal/httpapi/admin.go`)

- `adminSnapshot` now includes:
  - `virtual_endpoints[]` with `eligible_count` (expanded pool size) and `enabled` bool.
  - `route_profiles[]`
  - `candidate_pools[]` with `expanded_count`
  - `fallback_chains[]`
- No secrets leaked.

---

## 10. Dashboard (`web/index.html` + `web/app.js`)

- Nav: Endpoints (◐), Profiles (⬙), Pools (⬡) — added alongside existing tabs.
- Sections `#virtual`, `#profiles`, `#pools` with tables `virtualRows`, `profileRows`, `poolRows`, `chainRows`.
- `app.js`:
  - Subtitles for new tabs describing "stable public names → profile → pool".
  - Nav dispatches to `renderVirtual/renderProfiles/renderPools`.
  - `render()` conditionally renders new tabs when active.
  - `renderVirtual()` shows eligible count, enabled pill, edit/delete.
  - `renderProfiles()` shows pool/fallback/strategy.
  - `renderPools()` shows mode/members/expanded + chain list.
  - Prompt-based CRUD editors: `editVirtual/deleteVirtual`, `editProfile/deleteProfile`, `editPool/deletePool`, `editChain/deleteChain`, exposed on `window` for inline `onclick`.
  - Add buttons prompt for ID then delegate to edit handlers.
  - CLI tab updated to show virtual endpoint model (`nexa-code`) in snippets, listing available VEs.

---

## 11. Tests

### New files

- `internal/config/virtual_test.go`:
  - Valid VE, duplicate public_model rejection, collision with physical model, reserved `auto` rejection, missing route profile, invalid chars, route profile missing pool/chain, pool invalid mode, fallback chain duplicate pool / unknown pool / empty chain, legacy public_model migration synthesizes VE/pool/profile, round-trip save/load, existing configs unchanged.
- `internal/route/resolver_test.go`:
  - Explicit pool, all mode, fallback chain ordered progression, no candidate outside pool, unknown model, alias matching.
- `internal/httpapi/virtual_test.go`:
  - Virtual endpoint resolves correctly (only pool members routed, headers set, direct physical routing preserved).
  - Disabled → 404, unknown → 503, protocol filtering (Anthropic-only VE rejects OpenAI with 400, allows Anthropic).
  - Models discovery (virtual models + auto/claude-auto).
  - Fallback chain: primary 503 → secondary succeeds.
  - Session affinity pins actual deployment (not virtual identity).
  - Success state: actual successful deployment recorded, failed first does not become success, `route_ok` preserves VE identity.
  - Backward compatibility (no VE).
  - Admin CRUD (create/list/get/update/delete).
  - Client auth: valid key works with VE, invalid → 401, provider key not accepted as gateway key.
  - Hot reload: config mutation updates models list.
  - Legacy endpoint compatibility (`/admin/api/endpoint` creates VE with `nx_` key).

### Existing suite

- All 15 packages green: `go test ./...` PASS.
- `go vet ./...` PASS.
- `node --check internal/httpapi/web/app.js` PASS (JS OK).

---

## 12. Verification

| Gate | Command | Result |
|------|---------|--------|
| Formatting | `gofmt -w internal/...` | clean |
| Vet | `go vet ./...` | PASS |
| Tests | `go test -timeout=3m ./...` | PASS (15 packages, including 11 new virtual tests) |
| Race | `go test -race -timeout=3m ./...` (partial, httpapi + router) | PASS (no races in new code; resolver is read-only after build, uses RWMutex) |
| JS syntax | `node --check internal/httpapi/web/app.js` | PASS |
| Models | `/v1/models` lists virtual endpoints first, then auto/claude-auto, then physical | verified in tests |
| Backward compat | default config with no VE has empty slices, loads, routes as before | TestVirtualEndpointBackwardCompatibility |

---

## 13. Backward compatibility & PR #13 coordination

- Existing config files without new sections load with safe defaults (empty slices). `ApplyDefaults` only synthesizes when `routing.public_model` present.
- `auto`/`claude-auto` preserved.
- Direct physical model routing (`model-a`) still works when VE exists.
- PR #13's single endpoint concept is superseded: legacy `/admin/api/endpoint` now creates a virtual endpoint, returns `nx_` key, and is documented as compatibility shim. No second endpoint system.

---

## 14. Security & observability

- No secrets in logs/telemetry: admin CRUD never logs bodies, metrics use `sanitizeMetricLabel`.
- No `eval()`/arbitrary JS: pool entries are declarative strings matched against deployment index.
- No SSRF: virtual endpoints do not introduce new external HTTP calls; they only reference local pools.
- Headers: `X-Gateway-Virtual-Endpoint`, `X-Gateway-Public-Model`, `X-Gateway-Route-Profile` added for tracing.
- Events: `route_ok` enriched with VE/PublicModel/RouteProfile/Pool.

---

## 15. Risks & mitigations

- **Hot-path mutation**: only one new call site (`candidatesForRequirement`) between `Candidates()` and attempt loop; Decision OFF (no VE) is byte-equivalent path (early return).
- **Model matching for VE**: fixed by using `Model=""` for eligibility when virtual, so pool filtering is authoritative.
- **Fallback chain runtime failover**: `AllFilteredCandidates` concatenates pools in order, so existing retry loop naturally fails over from primary to fallback.
- **Config validation**: collision detection prevents virtual model shadowing physical models, avoiding client confusion.
- **Dashboard JS**: prompt-based editors are minimal but functional; future phases will replace with proper forms (same pattern as provider editor).

---

## 16. What's next (Phase C)

- Request feature extraction (`internal/feature`) + local classifier (`internal/taskprofile`) + telemetry fields.
- Decision contracts still missing (Phase D).

---

## 17. Files changed

- `internal/config/config.go` — Phase B model + validation + synthesis
- `internal/route/resolver.go` — NEW
- `internal/httpapi/server.go` — resolver wiring + candidatesForRequirement + handler routes
- `internal/httpapi/models.go` — virtual endpoint listing
- `internal/httpapi/anthropic.go`, `openai.go`, `canonical_path.go` — VE-aware filtering
- `internal/events/bus.go` — VE fields
- `internal/httpapi/virtual.go` — NEW admin CRUD
- `internal/httpapi/admin.go` — snapshot extensions
- `internal/httpapi/web/index.html` — nav + sections
- `internal/httpapi/web/app.js` — rendering + CRUD + CLI snippets
- `internal/config/virtual_test.go`, `internal/route/resolver_test.go`, `internal/httpapi/virtual_test.go` — NEW tests
- `internal/router/router.go` — `EligibleIgnoreModel` helper

No second router, no second dashboard, no external dependencies added.
