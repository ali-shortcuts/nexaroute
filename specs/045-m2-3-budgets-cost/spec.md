# M2-3 — Budgets and Cost page, exports (webhook alerts: documented remainder)

Live cost data exists today: the admin snapshot carries the
usage snapshot (total estimated cost + per-deployment totals
priced from model config), and the identities endpoint carries
per-identity token totals with a CSV form. This unit adds the
Cost page:

- Totals strip (estimated cost, requests, tokens in/out).
- Per-deployment cost table with share bars; per-identity token
  table (identity cost is not tracked separately on this backend
  and is never fabricated from tokens).
- Exports: identity CSV (existing endpoint), per-deployment CSV
  and full cost JSON generated client-side from snapshot data.
- Budgets section: when virtual-key records carry a budget_usd
  limit (the budget backend in PR #329), the configured limits
  are listed. Until that backend lands, the page says so plainly.

REMAINDER (explicit, not silently dropped): webhook budget
alerts need a server-side alerter watching spend against limits;
they belong with the budget backend (#329 follow-up), not this
UI unit.
