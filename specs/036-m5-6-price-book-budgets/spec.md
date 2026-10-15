# M5-6 — Versioned price book, token accounting, atomic budgets, cost routing

Today: prices live per-model in config and are applied at SNAPSHOT
time (a price edit reprices all history); cache tokens never reach
accounting; no spend caps exist; cost_aware routing reads model
prices. This unit adds:

- **Versioned price book** (`pricing.versions[]`: {version,
  effective_from, prices: {"provider/model": {input, output,
  cache_read?, cache_write? per MTok}}}). The active version at a
  moment is the latest effective_from <= that moment. Resolution
  order: book entry -> model config prices (version label
  "config"). Validated (unique versions, RFC3339 dates, prices in
  range, deployments exist).
- **Record-time costing**: usage is priced WHEN RECORDED with the
  version active then, and the tracker accumulates cost per price
  version (Totals.cost_by_version_usd). Recorded costs no longer
  reprice when prices change; usage without a recorded cost is
  estimated from model-config prices, and those estimates can
  change when the fallback rates change. Cache read/write tokens join accounting
  where the path reports them (canonical + Anthropic native).
  Cost conventions per dialect (published billing semantics):
  Anthropic input EXCLUDES cache tokens; OpenAI/Gemini/Responses
  input INCLUDES cached tokens (the cached share is priced at the
  cache-read rate instead of the input rate, never double-charged).
  Cache rates default to the input rate when the book omits them.
- **Atomic budgets**: virtual keys gain `budget_usd` (0 =
  unlimited). Admission (clientAuthAllowed, next to the TPM
  reserve) reserves the conservative estimate (most expensive
  deployment serving the model; input estimate + max output) in a
  mutex-guarded ledger; over-budget -> 402 budget_exceeded. The
  first usage report settles the reservation to the ACTUAL cost;
  unsettled reservations expire into committed-at-estimate after
  15 min (mirrors the TPM rule that unreported requests keep their
  estimate). Budgets are runtime state like the usage tracker
  itself: they reset on restart (documented).
- **Cost routing**: router deployments take input/output prices
  from the ACTIVE book version when an entry exists (resolved at
  router build / config swap), so cost_aware follows the book.
- **Property tests**: randomized invariants for pricing (version
  resolution, monotonic non-negative cost), the budget ledger
  under concurrency (committed+reserved never exceeds the limit
  for admitted reservations; conservation), and the tracker
  (concurrent RecordEx sums exactly; per-version sums reconcile).
