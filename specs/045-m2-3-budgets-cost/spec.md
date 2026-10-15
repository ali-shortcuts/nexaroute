# M2-3 — Budgets and Cost page, exports (webhook alerts: documented remainder)

Live cost data exists today: the admin snapshot carries the
usage snapshot (total estimated cost + per-deployment totals),
and the identities endpoint carries per-identity token totals
with a CSV form. Since PR #329 (this PR's base), usage
with a recorded cost is priced WHEN RECORDED under the
price-book version active at that time; such recorded costs
are NOT repriced when price-book versions or configured
prices change. Records with no applicable price are labelled
unpriced, and legacy/unpriced records are estimated from
model-config prices — those fallback estimates MAY change
when the fallback rates change. This unit adds the Cost
page:

- Totals strip (estimated cost, requests, tokens in/out).
- Per-deployment cost table with share bars; per-identity token
  table (identity cost is not tracked separately on this backend
  and is never fabricated from tokens). The identity table is
  capped at 100 rows and discloses the cap in place ("Showing
  the first 100 of N identities").
- Exports: identity CSV (existing endpoint), per-deployment CSV
  and full cost JSON generated client-side from snapshot data.
- Budgets section: virtual-key records carry budget_usd (the
  budget backend in PR #329 enforces the limits), exposed via
  GET /admin/api/client-keys, and the configured limits are
  listed. The panel distinguishes its states — configured
  budgets, none configured, budget data not exposed by the
  backend, key records not visible to the caller's role, and
  key records unavailable — and never presents an empty list
  as proof that no budget exists.

REMAINDER (explicit, not silently dropped): webhook budget
alerts need a server-side alerter watching spend against
limits. PR #329 enforces budgets but ships no alerter, so
alerts remain a separate follow-up beyond both this page and
the budget backend.
