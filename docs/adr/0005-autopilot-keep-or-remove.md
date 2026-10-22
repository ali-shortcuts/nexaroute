# ADR 0005: Autopilot — keep and test, or remove

- Status: **Proposed — decision required from the repository owner.**
  The placeholder until the decision is Option C (keep, frozen); no
  investment and no removal happens on a default.
- Date: 2026-10-22
- Parent audit: #593 (architecture & quality audit finding)

## Context

The autopilot (`internal/autopilot`, Phase 18) is a routing-policy
canary controller: it leases a configurable share of traffic to a
candidate routing policy, observes error/latency signals per stratum,
evaluates a promotion gate (`EvaluateGate`), and can promote the
candidate or roll back to the incumbent, recording incidents.

It is **live, not dead code** — verified on main:

- `internal/httpapi/autopilot.go` builds the controller from validated
  config (`newAutopilotController`), selects policies in the data plane
  (`autopilotSelectPolicy` + per-request leases on the server), records
  TTFT/failover/finish signals, and the admin API drives
  `ctrl.Evaluate()` / `ctrl.Promote()`.
- `internal/httpapi/autopilot_e2e_test.go` exercises the lifecycle
  end-to-end, including a rollback incident with gate reasons.
- `internal/config` validates the whole `autopilot.*` surface
  (policy references, canary bounds) at load time.

The finding that forces this decision is coverage asymmetry. In the
package's own test profile (baseline run on 2026-10-22), package
coverage is 80.4%, and five public `Controller` methods show **0.0%**:
`TargetPolicyID`, `TargetMatches`, `Release`, `Evaluate`, `Rollback`.
Some of them are driven through the httpapi end-to-end tests, but no
test calls them directly, and their edge semantics (lease release,
rollback state restoration, target matching) are exactly the paths a
canary controller must not get wrong silently. A feature that can
shift production traffic deserves either direct proof or removal —
the current middle state is the risk.

## Options

### Option A — Keep and close the proof gap

Add direct unit tests in `internal/autopilot` for the five uncovered
public methods (state transitions, gate verdicts, lease release,
rollback restoration), keep the httpapi end-to-end suite, and record
the feature as supported in the routing documentation.

- Pro: keeps a working, config-validated canary capability that
  already has data-plane and admin wiring; the investment is bounded
  (tests for five methods, no redesign).
- Con: ongoing maintenance surface for a feature whose real-world use
  is unproven; every routing change must keep the lease/signal model
  coherent.

### Option B — Remove the feature

Delete `internal/autopilot`, its httpapi wiring (controller, leases,
admin handlers, e2e tests) and the `autopilot.*` config surface.

- Pro: removes an unproven traffic-shifting mechanism and its whole
  failure class; shrinks the routing surface to deterministic,
  operator-explicit policy changes.
- Con: a breaking change — existing configurations using
  `autopilot.*` keys would fail validation/loading semantics, and the
  removal must be staged (deprecate-then-remove) to be safe for
  operators. Loses the canary capability entirely; re-adding it later
  costs more than keeping it.

### Option C — Keep, frozen (placeholder only)

No new tests, no removal. The feature stays as-is and unadvertised.

- Pro: zero immediate work; preserves the owner's optionality.
- Con: the coverage asymmetry — the stated risk — remains by design.
  Acceptable as a placeholder, not as a resolution.

## Recommendation

Option A if the canary capability is wanted at all: the code is
written, wired and e2e-tested, so the marginal cost of making it
trustworthy is small next to the sunk cost. Option B if it is not —
an unproven traffic-shifting feature should not ride along untested.
Option C is explicitly only the placeholder until this decision is
made.

## Exact decision required from the owner

**Keep autopilot (Option A: invest in direct tests and support it),
or remove it (Option B: staged deprecation and deletion)?**
