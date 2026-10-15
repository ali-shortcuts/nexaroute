# M2-6 — Audit log page: searchable, filterable, exportable, tamper-evident

The tamper-evident audit chain exists server-side (M3-6:
hash-chained records via GET /admin/api/audit, `nexaroute audit
verify` CLI). This unit adds its page:

- Audit nav page loading up to 1000 records (limit selectable),
  newest first.
- Search across actor/action/target/path/outcome/request id;
  outcome filter (all / succeeded / failed-or-denied).
- Tamper evidence in the browser: per-record seq + short hash,
  and an in-page CHAIN LINKAGE check over the loaded window
  (each chained record's prev_hash must equal the previous
  record's hash; legacy pre-chain records are marked as the
  unverified legacy segment). Full cryptographic verification
  remains `nexaroute audit verify` — the page says so and never
  claims more than linkage.
- Export of the loaded (filtered) records as JSON or CSV.
- Role failures render an explicit requires-admin state.
