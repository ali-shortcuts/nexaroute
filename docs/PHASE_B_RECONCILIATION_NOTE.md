# PHASE B RECONCILIATION NOTE

## Current main state (ba91c531)

- Branch `arena/01a0d825-nexaroute` off `main` at `ba91c531e13826a7d6637022fb2a805a4e709501`.
- No Virtual Endpoint concept in main.
- Routing: `router.Requirement{Model}` → `Router.Candidates()` → attempt loop.
- `auto` and `claude-auto` are hard-coded gateway aliases matching all deployments.
- Client auth: global `client_auth` with static keys, constant-time compare, per-key RPM.
- `/v1/models` lists physical deployment IDs + aliases + auto/claude-auto.
- Config: `routing` has strategy, attempts, health thresholds, etc., but no `public_model`.
- No candidate pools, route profiles, fallback chains.

## PR #13 overlap inspection

PR #13 `fix/beta-compatibility-install` (Beta 0.6.1-beta.3) introduces a primitive unified endpoint:

**What it implements:**
- `RoutingConfig.PublicModel string` (default "nexaroute") – single public model name.
- `internal/httpapi/endpoint.go`: admin endpoint `POST /admin/api/endpoint` with `{model, rotate_key}` creates/rotates gateway key (`nx_` + 32 random bytes hex = 67 chars) stored as first entry in `ClientAuth.Keys`, enables `ClientAuth`.
- `router/router.go`: `eligibleDeployment` and `Candidates()` treat `PublicModel` as matching all deployments (bypass model match).
- `models.go`: exposes `PublicModel` + auto + claude-auto as gateway-owned models.
- `server.go`: clones `ClientAuth.Keys` correctly, adds `/admin/api/endpoint` route.
- Web UI: endpoint tab with Base URL, Public model name, Gateway API key (password field), Save/Create, Copy key, Replace key, connection snippets for Claude Code / OpenAI / env / health.
- Tests: `endpoint_test.go` covers shared-model Anthropic ingress with cross-provider failover, key separation, persistence, future models auto-joining, invalid mutation, rotation, remote rejection.
- `clientauth.go`: bucket capacity guard.

**Conceptual similarity to Phase B:**
- One public client-facing model name abstracting pool.
- Generated gateway key.
- Eligible enabled deployments behind public name.
- Future providers/models joining shared pool.
- Preservation of existing health/capability/routing rules.
- Key separation: provider keys server-side only.

**Reusable pieces:**
- Key generation logic (`crypto/rand` 32 bytes hex with `nx_` prefix).
- Secret handling: `no-store` cache control, snapshot redaction, clone of keys.
- UI pattern: endpoint panel with copy/rotate/save.
- Router bypass for public model (generalize to multiple VEs).
- Models endpoint including virtual models.
- Validation of public model name (no whitespace/quotes, max 128).

**Conflicts with Phase B architecture:**
- Single global `PublicModel` vs first-class multiple Virtual Endpoints.
- No Route Profiles, no Candidate Pools, no Fallback Chains – cannot express `nexa-code` vs `nexa-fast` vs `coding-smart`.
- Auto-join all enabled deployments globally is implicit and surprising; Phase B requires explicit pool membership with optional `all` mode.
- Config schema: `routing.public_model` vs new top-level `virtual_endpoints`, `route_profiles`, `candidate_pools`, `fallback_chains`.
- Admin API: singular `/admin/api/endpoint` vs CRUD for multiple VEs.
- No deterministic precedence rules for collisions (physical model ID vs alias vs auto vs virtual).
- No route resolution abstraction shared across Anthropic/OpenAI/Responses ingress.
- No observability for virtual endpoint identity.

**Chosen integration approach:**

1. Implement full Phase B model as specified: `virtual_endpoints`, `route_profiles`, `candidate_pools`, `fallback_chains` as first-class config sections.

2. Preserve backward compatibility for PR #13 configs:
   - Keep `RoutingConfig.PublicModel` field (deprecated) in config for loading old files.
   - In `ApplyDefaults`, if `PublicModel` is set and `virtual_endpoints` is empty, synthesize:
     - CandidatePool `default` mode `all`
     - RouteProfile `default` referencing pool `default`
     - VirtualEndpoint `default` with `public_model = PublicModel`, `route_profile = default`, enabled=true
   - This makes PR #13's single-endpoint workflow continue to work while new configs use full model.
   - When `virtual_endpoints` non-empty, `PublicModel` is ignored (logged as deprecated) – no ambiguity.

3. Generalize key generation:
   - Extract `generateGatewayKey()` helper (same `nx_` + 32 bytes) reused for both legacy endpoint API and new Virtual Endpoint APIs.
   - Keep existing global `client_auth` as authoritative; Virtual Endpoints reference existing keys via optional allowed-keys? For Phase B minimal, VEs use global auth (any valid key) – no second key system. Future phases can add per-VE key_refs.

4. Router:
   - Do NOT keep PR #13's `PublicModel` bypass in router core. Instead, remove that special case and implement pool-based filtering in httpapi layer via new `internal/route` resolver.
   - Resolver expands pool entries (deployment ID / model / alias / suffix) to deployment ID sets, handles `all` mode, fallback chain ordering, validation.
   - Single shared resolver used by all ingress paths (Anthropic, OpenAI Chat, Responses).

5. Admin API:
   - Keep legacy `/admin/api/endpoint` for backward compat, but implement it on top of new VE model (creates/updates default VE).
   - Add new CRUD endpoints:
     - `/admin/api/virtual-endpoints` and `/admin/api/virtual-endpoints/{id}`
     - `/admin/api/route-profiles` and `/{id}`
     - `/admin/api/candidate-pools` and `/{id}`
     - `/admin/api/fallback-chains` and `/{id}`
   - Use same auth, validation, atomic config update flow.

6. Dashboard:
   - Generalize PR #13 endpoint panel into Virtual Endpoints management UI.
   - Show Virtual Endpoint → Route Profile → Candidate Pool → Currently Eligible.
   - Keep connection snippets per VE.

7. Do NOT overwrite unrelated fixes in PR #13:
   - `clientauth.go` bucket guard – preserve.
   - `server.go` clone of keys – preserve.
   - Canonical path fixes – not touched by Phase B (avoid hot-loop changes).

8. Other open PRs:
   - #20 hedge attempt accounting – avoid modifying `anthropic.go`/`openai.go`/`hedging.go` hot loop beyond minimal resolver hook (filter candidates, not loop rewrite).
   - #21/#26 Responses routing – keep canonical_path.go unchanged except for using resolver.
   - #31 compat tests – low conflict.

Result: Phase B builds on PR #13's validated single-endpoint concept as degenerate Virtual Endpoint, generalizes it, and establishes the stable abstraction layer required for all later intelligence phases without creating a second router or second auth system.
