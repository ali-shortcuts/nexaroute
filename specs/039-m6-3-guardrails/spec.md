# M6-3 — Guardrails: broader detection, deterministic output, >=90% coverage

The guardrail layer exists (internal/guardrail.Scan +
Server.applyGuardrails inside clientAuthAllowed: off/audit/block
modes, per-class reject flags, body cap -> 413, audit event).
This unit hardens and proves it:

- Detection breadth: credit cards validated by Luhn (a bare
  16-digit run is NOT enough), US SSNs validated by area/group/
  serial rules, plus JWTs, PEM private-key blocks, DB/AMQP
  connection strings with embedded passwords, Bearer tokens,
  Google API keys, Slack tokens, and GitHub fine-grained PATs in
  the secret class; more injection phrasings (disregard previous,
  developer mode, print your instructions, override safety,
  do anything now, ...). Patterns stay multi-token/high-precision
  shapes.
- Determinism: Scan returns findings in a fixed class order
  (the current map iteration is random).
- Auditability: the audit-mode event names the matched kinds
  (never content).
- Coverage: internal/guardrail at 100% statements; a corpus
  battery (positives AND negatives per class, no-content-leak
  assertions for every positive) and an httpapi matrix battery
  over mode x class x reject-flag x error-style, the body cap,
  audit-mode upstream body preservation, and every data-plane
  ingress that passes through clientAuthAllowed. The >=90%
  target is reported as measured coverage of the guardrail
  package plus full branch coverage of applyGuardrails.

Known posture (documented, unchanged): guardrails run inside
client authentication, so they are active when client auth is
enabled; body scanning is bounded by guardrails.max_body_bytes.
