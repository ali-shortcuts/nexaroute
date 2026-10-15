# M6-2 — Deterministic policy-as-code lifecycle

Decision policies already live as code in config
(`decision_policies[]` + the `decision.policy` pointer) and are
evaluated by the deterministic policy provider. This unit adds
the lifecycle tooling around them, as a CLI (`nexaroute policy`),
mirroring the existing privacy/audit subcommands:

- **simulate**: evaluate one policy against a fixture file of
  recorded-shape DecisionRequests (privacy-safe: candidates and
  signals only, never prompts) WITHOUT serving traffic. Output
  per case: winner, ordered candidates, score breakdown
  (policy.Explain), plus a SHA-256 digest of the canonical report
  — the determinism proof. The same inputs always produce the
  same digest (asserted by tests across repeated runs).
- **replay**: re-evaluate recorded decision requests (JSONL of
  {request, recorded_winner}) under a candidate policy and diff
  against what production actually chose: agreement count and
  per-case divergences. This is how a policy change is reviewed
  against history before promotion.
- **promote**: point `decision.policy` at another defined policy
  in the config file (validated first; the file is rewritten
  atomically via config.SaveAtomic only when the whole config
  validates). Every promotion appends a record to an append-only
  audit log (`<config>.policy-audit.jsonl` by default): timestamp,
  action, from/to policy, and the config file's SHA-256 before
  and after.
- **rollback**: restore the previous policy pointer from the
  audit log's latest record. Rollback refuses when the config
  file's current digest differs from the last record's
  after-digest (out-of-band change) unless --force is given, and
  appends its own audit record.

Exit codes mirror the other CLIs: 0 ok, 1 operational error,
2 usage. Server-side config history (admin API) remains the
whole-config rollback layer; this CLI is the policy-pointer
lifecycle on the config-as-code file.
