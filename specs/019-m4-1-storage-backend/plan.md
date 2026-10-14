# M4-1A plan

1. `internal/securitystore/backend.go`: `Backend` interface (the exact
   public method set consumers use), `var _ Backend = (*Store)(nil)`,
   and `OpenBackend(backend, url, path string, maxAuditItems,
   retentionDays int) (Backend, error)` implementing the fail-closed
   selection semantics from the spec.
2. `internal/config`: `AdminConfig.SecurityStoreBackend` /
   `SecurityStoreURL` + validation (known values; URL required and
   bounded for redis/postgres; rejected for bbolt).
3. `internal/httpapi/server.go`: `securityDB` field becomes
   `securitystore.Backend`; construction goes through `OpenBackend`.
   No other consumer changes (all calls are interface methods already).
4. Tests: `backend_contract_test.go` — `runBackendContract` executed
   against bbolt (session lifecycle incl. rotate/revoke/touch expiry,
   audit append/order, chain verify valid); factory tests (default,
   explicit bbolt, redis/postgres fail-closed, unknown); config
   validation tests.
5. Docs: CONFIGURATION admin section (new fields + fail-closed note),
   OPERATIONS state-classification note, UNRELEASED entry, checklist
   M4-1 → IN PROGRESS with evidence; M4-2 row notes the contract suite.
