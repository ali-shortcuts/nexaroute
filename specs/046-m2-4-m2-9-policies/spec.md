# M2-4 + M2-9 — Policies/Guardrails page with simulator; route-profile privacy editing

- Policies page: guardrail posture from the live config
  (mode, per-class reject flags, body cap) and a LOCAL simulator
  that mirrors the server's current scan classes exactly
  (pii_email, pii_phone, secret_api_key, prompt_injection — the
  regexes in internal/guardrail/scan.go) and reports the verdict
  the configured mode + flags would produce. Text never leaves
  the browser; findings are kinds only, like the server. The
  page states the server remains authoritative (its class set
  grows with the guardrail hardening in PR #332).
- Decision plane summary + policy list (weights, task overrides,
  min score delta) from the live config; lifecycle operations
  (simulate/promote/rollback) point at the `nexaroute policy`
  CLI (PR #331) — display only here.
- M2-9: Settings gains a Route-profile privacy card: each
  profile's privacy (any / no_training) is editable via the
  existing PUT /admin/api/route-profiles/{id}, sending the full
  record with only privacy changed.
