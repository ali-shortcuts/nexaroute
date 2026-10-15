# Failure semantics

What a client or operator can rely on when something in or around
the gateway fails. Every row names the test(s) that pin the behavior;
`docs/OPERATIONS.md` covers the operational responses.

## Upstream failures

| Fault | Observable behavior |
|---|---|
| Upstream refuses the connection | The attempt fails promptly; with another eligible deployment the request fails over (F2), otherwise the client receives a 5xx with the standard error envelope and the failure is exported as `nexaroute_errors_total{error_type="provider_connection_failed"}` (F1). |
| Upstream accepts but never responds | The attempt is bounded by `routing.request_timeout_ms`; the client receives **504** when the bound is hit (F3 measured the configured 1.2s bound at ~1.20s). |
| Upstream cuts the connection mid-stream | The client keeps the partial content that was actually produced; the stream then terminates. The gateway does **not** fabricate the remainder, a `finish_reason`, or a `[DONE]` it never received (F4). Usage accounting records only what the upstream reported. |
| Repeated upstream failures | The deployment breaker trips after `routing.failure_threshold` consecutive failures: the gateway sheds load at the edge with 503 `no compatible healthy deployment` instead of queueing doomed attempts (flapping soak: ~105k edge rejections vs ~11.5k upstream successes during alternating phases). |
| Recovery — default `ready_mesh` strategy | Eligibility requires the deployment to be **Healthy**, and health is restored by the **probe engine**, not by live traffic (a stopped deployment receives no trial requests). With probes enabled, recovery probing re-admits the deployment shortly after the upstream heals (flapping soak: ~0.26s after stabilization with fast probe settings). **Consequence: disabling probes under a ready strategy means a tripped deployment stays down until probes run again or the config changes** — probes are on by default; keep them on. |
| Recovery — non-ready strategies (e.g. `priority`) | Half-open trial: after the cooldown (`routing.cooldown_seconds`) expires, the next live request is admitted as a trial; its success restores the deployment and traffic flows (F6). Requests inside the cooldown are refused without an upstream attempt. |

## Platform failures

| Fault | Observable behavior |
|---|---|
| Control-plane store outage | Per-namespace failure mode decides: `fail_closed` surfaces the error and refuses to serve the namespace; `fail_open` proceeds without the stored snapshot; `last_known_good` serves the last loaded snapshot (pinned by the control-plane manager tests). The zero-service local default (file store) keeps working with no external service. |
| Admin security store locked by another process | Startup does not proceed silently: opening is bounded by a ~2s bbolt lock wait, then the gateway reports `SecurityStoreError` and admin authentication fails closed (F5) rather than running without sessions/audit. |
| Admin security store unavailable mid-run | Admin auth fails closed (no session creation/validation without the store); the data plane is unaffected. Audit append failures surface as errors to the caller, never as silent drops (securitystore contract suite). |
| Audit chain tampering (any backend) | Full-chain verification runs inside every append transaction/commit; a tampered record, rewound head, or lowered allocator makes both verification AND further appends fail until an operator restores a good backup (securitystore tamper suites; `nexaroute audit verify` for offline checks). |
| Backup/restore faults | Restore verifies the whole bundle (manifest SHA-256 per file) before writing anything; a tampered or truncated bundle changes nothing on disk. Interrupted restores are recoverable via the durable journal and `nexaroute restore --recover` (M4-3 suites). |
| IdP unavailable or JWKS endpoint down | Admin login callbacks fail closed — no session is issued on an unverifiable token. Signing keys are fetched fresh per callback, so a retired IdP key stops working as soon as the IdP unpublishes it (M4-7 rotation tests). |
| Key custody provider unavailable/misconfigured | Startup fails closed with the provider error; the gateway never falls back to a different key source (secrets provider tests). |
| Config file invalid at startup | Startup fails with the validation error; a running gateway that receives an invalid hot-reload keeps serving the last good config. |

## Load-shedding semantics

- Global/provider concurrency limits reject excess requests rather
  than queueing them without bound; overload rejections are exported
  in metrics and alerted on (`deploy/observability/`).
- The soak suite (`.github/workflows/soak.yml`, `scripts/soak.sh`)
  re-runs the traffic, hot-reload, recovery-cycle and flapping
  scenarios, including a `-race` pass: no leaked in-flight slots,
  no lost requests, goroutines return to baseline after drain.

## Evidence map

- F1–F6: `internal/httpapi/failure_injection_test.go`.
- Flapping soak: `TestSoakFlappingUpstreamFailureSemantics`
  (`internal/httpapi/soak_test.go`, env-gated `NEXAROUTE_SOAK=1`).
- Hot-reload soak: `TestSoakConcurrentTrafficAndHotReload`;
  recovery cycles: `TestSoakRepeatedRecoveryCyclesReleaseState`
  (`internal/probe`).
- Failure modes: `internal/controlplane` manager tests
  (`TestManagerLastKnownGood`,
  `TestManagerFailClosedDoesNotUseStaleSnapshot`).
