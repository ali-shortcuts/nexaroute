# Phase F — External Decision Providers

Date: 2026-09-25. Checkout: branch `arena/01a0d9e5-nexaroute` (from `ba91c53`).

Phase F adds a **secure external DecisionProvider infrastructure** plus **one
real adapter** (Jev `model-route`). The pre-implementation audit
(`docs/PHASE_F_CURRENT_STATE_NOTE.md`) found no decision plane at all, so
Phase F also establishes the minimal **generic DecisionProvider foundation
owned by NexaRoute** — contract, registry, orchestrator, validator,
primary-selection guardrails, and the `local`/`policy` built-ins. Nothing in
the generic layer names, imports, or depends on Jev (or TypeSafe, OpenAI,
Anthropic, Gemini, or any vendor): the Jev code lives in exactly one package,
`internal/decision/jev`, behind the generic `DecisionProvider` interface.

This document is the normative contract for the decision plane. The build
record, strict-check matrix, and verdict live in
`docs/PHASE_F_IMPLEMENTATION_REPORT.md`.

## 1. Architecture

```text
ingress (openai.go / anthropic.go / canonical_path.go)
  │ router.Resolve → eligible band (capability + health filtered)
  ▼
cache-serve early return (opt-in; cache hits never consult the plane)
  │ miss (or uncacheable)
  ▼
applyDecision (decision_wiring.go)              ← fail-open seam; len(band) ≥ 2 only
  │ 1. session-affinity pin check (short-circuit, no provider call)
  │ 2. orchestrator.Decide (exactly one provider call)
  │ 3. validator.Validate (membership + mapping-only enforcement)
  │ 4. reorder band (selected first, survivors keep order)
  ▼
attempts → failover → health (unchanged)
```

The decision plane **reorders** the router's eligible band; it never adds,
removes, or rewrites deployments. Execution, retry, failover, and health
semantics are untouched: a decision only changes which deployment is
attempted *first*.

### 1.1 Generic contract (`internal/decision`)

| File | Contents |
|---|---|
| `provider.go` | `DecisionProvider` interface: `ID()`, `Type()`, `Decide(ctx, DecisionRequest) (DecisionResult, error)` |
| `request.go` | `DecisionRequest` (candidates, allowed IDs, features, 512-candidate bound), `Candidate` (metadata only), `RequestFeatures` (externalized booleans) |
| `result.go` | `DecisionResult` (selected ID or abstain + bounded reason) |
| `registry.go` | `Registry`: register/get by ID, duplicate-ID rejected |
| `orchestrator.go` | Single-provider flow: affinity short-circuit → one `Decide` call with timeout → validate → outcome |
| `validator.go` | Membership proof: selection must be in `AllowedPrimaryIDs`, no invented IDs |
| `constraints.go` | Hard pre/post-call guards (single call, affinity, band preservation) |
| `reason.go` | Closed `ReasonCode` set (`LOCAL_*`, `POLICY_*`, `AFFINITY_*`, `PRIMARY_*`, `EXTERNAL_*`) |
| `local/provider.go` | Built-in `local` provider: deterministic earliest-PoolOrdinal/priority pick |
| `policy/provider.go` | Built-in `policy` provider: deterministic policy pick (no network) |
| `remote/` | Shared external transport: `client.go` (POST JSON, no retry/redirect), `errors.go` (typed secret-safe errors), `limits.go` (32 KiB request / 64 KiB response / 4 KiB key caps) |
| `jev/` | The single external adapter: `provider.go`, `request.go`, `response.go`, `mapper.go` |

### 1.2 Modes (`decision.mode`)

- `off` (default): no provider runs. Local deterministic behavior, zero
  external calls. Configured external providers stay idle.
- `local`: only the built-in `local` or `policy` providers may run.
  External providers are rejected by config validation.
- `assisted`: exactly one configured external provider runs
  (fail-open on any error); the built-ins are rejected as the
  `assisted` provider so the mode always means what it says.

Mode and provider are hot-reloadable: the running server swaps the whole
decision runtime atomically (see §6) and rejects invalid reloads without
touching the live plane.

## 2. Primary-selection guardrails

These hold for **every** provider, built-in or external — the external
service advises, NexaRoute decides:

1. **Affinity short-circuit.** If the request carries a session pin that is
   still in the eligible band, the pinned deployment is kept and **no
   provider is called** (`AFFINITY_PRESERVED`).
2. **Single-choice skip.** With fewer than two eligible deployments the
   provider is not called (`PRIMARY_SINGLE_CHOICE`).
3. **Earliest-first default.** `local` selects the earliest
   PoolOrdinal/priority candidate, so fail-open output equals the
   pre-decision router order.
4. **Membership validation.** A selection outside `AllowedPrimaryIDs` is
   discarded and the request fails open
   (`PRIMARY_CONSTRAINT_VIOLATION` / `EXTERNAL_UNKNOWN_CANDIDATE`).
5. **Band preservation.** The outcome reorders the input band: the selected
   deployment moves first, survivors keep relative order, length and
   membership are unchanged (property-tested over 2000 adversarial cases).
6. **Single call.** At most one provider call per request; external calls
   never retry (`ProviderCalls ≤ 1` is asserted by every external test).

## 3. Privacy: metadata only

The external service receives **routing metadata, never user content**:

- Opaque candidate IDs (`c0`, `c1`, …) — physical deployment IDs
  (`p1/model-a`) never leave the gateway.
- Per-candidate metadata: non-identifying description, capability flags,
  context window, health snapshot (EWMA latency, consecutive successes,
  sample counts). No prompts, no parameters, no message text.
- Request features as externalized booleans (`streaming`, `tools`,
  `vision`, `reasoning`, `stakes`), never raw requirement structs.
- API key travels in the `Authorization: Bearer` header only.
- The unmapped `guidance`/`message`/`probabilities` fields of a Jev
  response are parsed and dropped; unknown JSON keys are ignored.

Canary tests (`decision_privacy_test.go`) plant unique markers in the
prompt, the API key, and a mocked remote error body, then prove none of
them appear in the Jev payload, events, metrics, admin snapshot, or the
client response. Only bounded classes (`EXTERNAL_HTTP_ERROR`, …) are
recorded; remote bodies are never stored.

## 4. The Jev adapter (`internal/decision/jev`)

One real external provider, against the official Jev API
(`POST https://www.jevai.org/api/v1/decisions/model-route`):

- **Endpoint**: `DefaultEndpoint =
  https://www.jevai.org/api/v1/decisions/model-route`; path constant
  `ModelRoutePath`. The host is allow-listed (`www.jevai.org` only);
  anything else is rejected before dialing (SSRF guard).
- **Request**: `task` + `candidates[{id, description, cost?, latency?}]`
  (+ `priorities`/`constraints`/`stakes`), measured after marshaling;
  bodies over 32 KiB fail open **without sending a byte**.
- **Response**: Jev envelope `{code, message, data}`; `code == 0` means
  success and `data.decision` carries the chosen opaque ID (mapped back
  through the request-local lookup; anything else is
  `EXTERNAL_UNKNOWN_CANDIDATE`). Confidence is clamped to [0,1];
  non-finite values are rejected.
- **Transport** (`remote.Client`): exactly one POST, context timeout
  honoured mid-flight, redirects refused, HTTP/2 + compression disabled
  for a minimal fingerprint, `User-Agent: NexaRoute-Decision/1.0`,
  responses capped at 64 KiB. No retries on any status (400/401/403/429/
  5xx all fail open after one attempt).
- **TLS**: default Go verifier, TLS 1.2 minimum; no custom roots, no
  insecure skips.
- **Errors**: typed and secret-safe — timeout, HTTP status class,
  invalid response, unknown candidate, request/response too large,
  unavailable. The API key and remote bodies never appear in errors.

`NewWithTransport` (custom endpoint/transport) exists for tests only; the
production wiring (`buildDecisionRuntime`) always uses the allow-listed
production endpoint. Optional manual smoke test:
`JEV_API_KEY=... bash scripts/smoke-jev.sh` (network-gated, never in CI).

## 5. Configuration

```json
{
  "decision": {"mode": "assisted", "provider": "jev-main", "timeout_ms": 400},
  "decision_providers": [
    {"id": "jev-main", "type": "jev", "enabled": true,
     "api_key_env": "JEV_API_KEY", "privacy_mode": "metadata_only"}
  ]
}
```

- `decision.mode`: `off` (default) | `local` | `assisted`.
- `decision.provider`: `local` (default) | `policy` | an external entry ID.
- `decision.timeout_ms`: per-call bound, default 400 ms (50–30000
  enforced for every mode).
- `decision_providers[]`: at most 16 entries, unique IDs, `type: "jev"`
  (only external type in Phase F), `privacy_mode: "metadata_only"`
  (only mode in Phase F). `assisted` requires the referenced entry to
  exist and be enabled; `local` accepts only built-ins; `off` allows an
  idle entry. Env credential wins over a literal key; a literal key is
  length-bounded.
- Timeouts and mode/provider changes apply on hot reload; a bad reload is
  rejected with the previous runtime untouched.

`configs/config.example.json` carries an annotated `decision` block plus a
disabled `jev-main` entry.

## 6. Hot reload and concurrency

- The live `*DecisionRuntime` sits behind an `atomic.Pointer`; every
  request loads it once. Reload builds the replacement **before** touching
  disk state and swaps it in one store — no torn reads, no request ever
  sees a half-built plane.
- Metrics counters carry across the swap (no reset on reload).
- The Jev provider is stateless per call (request-local mapping,
  clone-per-request `http.Client`); it is safe for concurrent use and the
  race detector covers the E2E suites.
- Decision state is request-scoped; there is no cross-request cache to
  invalidate.

## 7. Observability (bounded)

- **Events**: one `decision` event per assisted attempt with the bounded
  outcome class only (`EXTERNAL_SELECTED`, `EXTERNAL_TIMEOUT`, …).
- **Metrics** (`/metrics`, Prometheus text):
  `nexaroute_external_decision_requests_total{type,outcome}` and
  `nexaroute_external_decision_latency_seconds_{count,sum}{type}` —
  labels are `type × outcome` only (no IDs, no status codes, no bodies).
- **Admin** (`/admin/api/snapshot`): `decision: {mode, provider}` plus
  `external_decision_providers[]` safe rows (`id`, `type`, `enabled`,
  `privacy_mode`, credential *presence* — never the key).
- External failures never touch model health: no failure counters, no
  quarantine, no cooldown from the decision plane. Health changes only
  when an *executed* deployment fails (tested both directions).

## 8. Security summary

| Threat | Mitigation |
|---|---|
| Prompt / PII exfiltration | Metadata-only payload; opaque IDs; canary-tested |
| Credential leak | Env-first keys; header-only use; redacted admin; secret-safe errors; key-length bound |
| SSRF / endpoint confusion | Host allow-list; fixed path; redirects refused |
| TLS interception | System roots, TLS ≥ 1.2, no skips |
| Remote overload / retry storm | Single call, no retry, context timeout, 32 KiB request cap |
| Malicious response | 64 KiB cap, strict envelope parse, closed reason codes, mapping-only acceptance, confidence clamping |
| Config injection via reload | Full validation pre-swap; bad reload rejected; previous runtime kept |
| Health corruption | Decision plane is read-only w.r.t. health state |

## 9. Non-goals (Phase F stops here)

No additional external providers, no provider chains or fallthrough
between decision providers (Phase G explicitly out of scope), no
decision-aware caching, no streaming decisions, no prompt-aware routing.
The `Type()` discriminator and the `remote` transport package exist so a
second adapter can be added later without touching the generic layer.
