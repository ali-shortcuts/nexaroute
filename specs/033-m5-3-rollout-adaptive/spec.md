# M5-3 — Quality routing, capped hedging, rollout, adaptive behavior

Existing substrate (verified by inventory, not rebuilt): measured
quality scoring in the router (health status + EWMA latency/failure
+ capacity pressure, tunable via routing weights and decision
policies), structurally capped hedging (first attempt only, one
partner, in internal/httpapi/hedging.go), retry/backoff with a
Retry-After cap, and three breaker scopes (deployment / provider /
capability) with probe- and trial-driven recovery (M4-8).

This unit adds the missing piece and pins the rest with tests:

1. **Rollout (new)**: route profiles gain `canary_deployment_id` +
   `canary_percent` (0-100). For requests with a stable key
   (session key, else selection key), a deterministic SHA-256
   bucket assigns the session to the canary arm: the canary
   deployment — when it is ALREADY an eligible candidate for the
   request — is promoted to the front of the candidate list (the
   rest remain as fallback). No key -> no rollout (stable arm).
   A decision policy on the profile, when configured, still
   governs final selection (documented precedence).
2. **Hedging cap invariants (tests)**: with 3+ candidates and a
   slow primary, total upstream calls for one request <= 2; the
   hedge loser records NO health failure.
3. **Quality/adaptive invariants (tests)**: measured EWMA failure/
   latency deprioritizes a deployment in candidate order; the
   Retry-After cap bounds the client-visible value on 429s.
4. **Privacy gate + leak battery (tests)**: a no_training profile
   serves only from trains_on_data="no" providers (fail closed on
   unknown/yes — the gate already exists; pinned at HTTP level),
   and a canary prompt string must appear in NO observable sink —
   event bus, usage records, response headers, error bodies —
   across the chat, messages and responses paths. The media paths
   (M5-1c) join the same battery when PR #324 lands; the sink
   assertions are path-agnostic by construction.
