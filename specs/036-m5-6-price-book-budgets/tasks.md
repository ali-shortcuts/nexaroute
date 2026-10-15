# M5-6 tasks

- [x] T1 config: pricing.versions + VirtualKeyConfig.budget_usd + validation; internal/pricing (Book, Resolve, Cost, Estimate)
- [x] T2 usage: Tracker.RecordEx + cache-token totals + cost-by-version; BudgetLedger (reserve/settle/expire/snapshot)
- [x] T3 router: book prices feed Deployment costs at build
- [x] T4 httpapi: record-time costing in recordUsage(+Ex), budget reserve in clientAuthAllowed, settle in recordUsage, canonical/anthropic cache tokens plumbed
- [ ] T5 tests: pricing/ledger/tracker properties, router pins, httpapi e2e (budget admit/settle/reject, version pinning)
- [ ] T6 docs (CONFIGURATION pricing + budget, UNRELEASED), PR green; stop for owner
