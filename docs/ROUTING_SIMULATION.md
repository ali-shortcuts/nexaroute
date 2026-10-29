# Routing simulation (dry run)

NexaRoute has one routing engine, not two. The simulation surface reuses
it: `GET /admin/api/route-preview` calls `Router.Explain`, which runs the
exact candidate funnel the data plane runs (`evaluateDeployment` for
eligibility, then the configured strategy ordering), and reports the
verdict for every deployment considered — included with its score, or
excluded with a stable reason code.

There is no separate simulation router to drift from real routing. What
differs between a simulation and a real request is only the side effects
around the funnel, and those differences are deliberate and labelled.

## Endpoint

`GET /admin/api/route-preview` — existing endpoint, extended, not a new
surface. No new endpoint was added for simulation.

Query parameters (all optional except as noted):

| Parameter | Meaning |
| --- | --- |
| `model` | Requested model, alias, virtual-endpoint public model, `auto`, or unknown model (unknown-model fallback is reported, not hidden). |
| `protocol` | `openai`, `anthropic`, or `openai_responses`. Virtual endpoints enforce their protocol list exactly; a mismatch is the answer, not an error. |
| `tools`, `vision`, `stream`, `reasoning`, `structured_output`, `parallel_tools` | Capability requirements (`1`/`true`/`yes` = required). |
| `require_no_training` | Fail-closed privacy requirement: only providers declaring `trains_on_data="no"` survive. |
| `min_context_window` | Minimum usable context window (non-negative integer). |
| `provider_type` | Restrict to one provider type. |
| `session_key`, `selection_key` | Bounded (≤512 chars) anonymous keys used only to evaluate session affinity and deterministic rotation. Consumed internally, never echoed, never persisted by the preview. |
| `compare_profile` | One alternative route profile id. The same requirement is simulated against that profile's pools and privacy setting; see Comparison below. Unknown id → `400`. |

Response fields that matter for operators:

- `preview: true`, `simulated: true`, `no_provider_call: true` on every
  successful simulation, and `selected_is_simulated: true` whenever a
  selection is reported. A simulated selection is never a record of a
  real request.
- `explanation.simulated`, `explanation.deterministic`,
  `explanation.rotation_predictive` (see Rotating strategies).
- `selected`: the simulated lead candidate, with score and score
  breakdown. `eligible_candidates`: the ordered simulated list.
- `explanation.decisions`: one verdict per considered deployment —
  `included` with priority, weight, score breakdown, health status,
  EWMA latency/failure, capacity pressure and cost estimate, or
  `excluded_reason` with one stable code (`model_mismatch`,
  `provider_type`, `privacy_no_training`, `provider_circuit_open`,
  `context_window_too_small`, `capability_*_unsupported`,
  `conformance_*_unsupported`, `retired`, `health_not_ready`,
  `capability_scope_not_ready`, `health_cooldown`,
  `fallback_superseded`, `not_in_virtual_endpoint_pools`,
  `not_in_route_profile_pools`).
- `excluded_count`: number of excluded decisions.

## What a simulation never does

- Calls no upstream provider (proven by tests with a counting upstream
  that assert zero hits, including under `round_robin`).
- Advances no routing rotation counter. Rotating strategies are ordered
  from the requirement's own keys (or offset 0 with no key) instead of
  the live counter, so a preview can never shift the next real pick.
- Creates no session pin, starts no probe, touches no admission counter,
  quota, budget, or usage ledger, and sends no traffic.
- Persists nothing: no prompt text is accepted at all (only capability
  flags and a context bound), no credential is accepted, and session /
  selection keys are never echoed back or stored by the preview.
- Leaks no tenant policy or secret: the response carries routing
  metadata only — deployment/provider identity, scores, health
  status/latency, strategy. No upstream error text, no prompt, no key,
  no client-key or tenant information.

The only shared-state housekeeping a simulation can trigger is reaping
an already-expired session pin on a read, exactly as any other read
does. That is not a routing decision and creates no pin.

## Access and bounds

- Loopback-only admin surface by default, plus `admin.api_key` when
  configured, plus the admin authorization matrix: `route-preview`
  requires the routing-read permission, and non-GET methods are denied
  (403) before the handler runs.
- Every text parameter is bounded (≤512 chars; model ≤256,
  provider type ≤64). Decisions are capped at 1000 entries
  (`decisionLimit`); a comparison adds one more bounded explanation,
  never an unbounded one.
- Cardinality is therefore bounded by the configured deployment count
  (itself config-capped), not by request volume: the preview keeps no
  per-request state and emits no metric or event per simulation.

## Rotating strategies and determinism

The simulation is deterministic: repeating the same simulation against
unchanged runtime state returns the same order. That is not the same as
predicting live traffic:

- `round_robin` and `adaptive_round_robin` rotate on every real
  request, so `rotation_predictive` is `false` and the simulated order
  (seeded deterministically from the requirement's own keys and model)
  must not be read as "the next real pick".
- `ready_mesh` power-of-two sampling is predictive when a session or
  selection key is supplied (the same key samples the same pair), and
  not predictive without one (live traffic consumes the counter).
- For every other strategy the simulated lead is the live lead from
  the same snapshot — assuming no health, config, quota, or pin change
  between the simulation and the real request. A simulation is a
  snapshot verdict, not a reservation: it does not hold a deployment,
  a credential, or a quota slot for a later request.

## Comparison: what changes under an alternative profile

`?compare_profile=<id>` runs the identical requirement through the
same funnel, then layers the alternative profile's membership over it:

- Candidate pool = the profile's primary pool plus every pool in its
  fallback chain, expanded through the live resolver.
- Privacy = the profile's privacy setting; a `no_training` profile
  tightens the requirement (a request-level `require_no_training`
  already set stays set).
- The comparison reports `selected`, `selected_changed`,
  `eligible_count`, `excluded_count`, `added_deployments`,
  `removed_deployments`, and the full alternative explanation with
  `not_in_route_profile_pools` for deployments the funnel included but
  the profile's pools do not admit.

Deliberately not simulated for the alternative profile: canary rollout
promotion and session-pin promotion. Both are session-specific
promotions (a keyed bucket, a recorded pin), and the comparison must
show what the profile itself changes, not what one session's history
would promote. The baseline selection, by contrast, does include the
baseline virtual endpoint's rollout and pin promotion, exactly as the
data plane does — so a `selected_changed` result can also reflect
those baseline promotions; the comparison's added/removed sets are the
profile-membership signal.

## Unsupported cases (explicit)

- No real traffic, no load test, no failover rehearsal: the simulation
  does not attempt providers, so it cannot show failover behaviour,
  retry outcomes, hedging, or what happens after a streamed response
  has started.
- No quota, budget, or admission prediction: provider quota evidence,
  budget ledgers, and the global admission ceiling are not inputs to
  the router, so a simulated selection says nothing about whether a
  real request would be admitted or charged.
- No task-intelligence simulation: task classification, task pools,
  and eval gates are request-classification features, not routing
  inputs to this funnel; a profile's task settings do not change the
  simulated order.
- No per-profile strategy simulation: route profiles currently inherit
  the global routing strategy (per-profile strategies are deferred;
  config validation rejects a non-`inherit` profile strategy), so the
  comparison varies pools and privacy, not strategy.
- No credential-pool simulation: credential selection happens after a
  deployment is deemed eligible and is not part of the routing
  decision; the simulation names deployments, never credentials.
- No multi-request or time-series simulation: health, latency, and
  capacity are read from the current snapshot only. A deployment in
  cooldown today may be healthy at the next request, and the
  simulation cannot fast-forward recovery.
- No guarantee across a config reload or health change: if config or
  health changes between simulation and request, the real decision
  follows the new state, not the simulated one.

## Operator UI

The Routes page in the admin UI has a "Routing simulation (dry run)"
panel: model/public route, protocol, capability flags, context bound,
and an alternative-profile selector, all calling the same endpoint.
It renders the simulated selection, the rotation-predictive warning
when applicable, the comparison outcome, and the per-deployment
verdict/score-breakdown table. The panel sends only capability flags
and a context bound — no prompt, session key, or credential.
