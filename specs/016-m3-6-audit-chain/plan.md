# Plan

- `internal/securitystore/store.go`
  - `AuditEvent`: add `Seq`, `PrevHash`, `Hash` (all `omitempty`).
  - `canonicalAuditHash`: SHA-256 over `nexaroute-audit-chain-v1` domain
    separator and length-prefixed fields in fixed order (seq, prev hash,
    timestamp RFC3339Nano UTC, request id, actor, action, target, method,
    path, outcome, status, permission, authn). The `Hash` field itself is
    excluded from its own computation.
  - Meta keys `audit-chain-head` / `audit-chain-anchor`: JSON `{seq, hash}`.
  - `appendAuditTx`: assign seq (existing meta sequence), resolve prev
    hash (meta head; else newest surviving record — stored hash, or the
    canonical hash of a legacy record using its key sequence; else
    genesis empty), compute hash, persist head. Pruning updates the
    anchor to the newest pruned record and keeps head equal to the
    newest surviving record (or the anchor when the bucket is empty).
  - `VerifyAuditChain(ctx)`: full scan, per-record hash recomputation,
    linkage, sequence monotonicity/duplicates, anchor and head checks.
    Returns a structured report; legacy records are counted separately
    and never reported as verified.
  - `OpenReadOnly(path)`: same file/directory permission enforcement as
    `Open`, bbolt read-only mode, no bucket creation, file must exist.
- `cmd/gateway/audit.go`: `audit verify` subcommand; config via
  `config.LoadBase`; store path resolution mirrors
  `httpapi.adminSecurityStorePath` semantics (configured path or
  `<config-dir>/security/nexaroute-security.db`).
- Tests: `internal/securitystore/chain_test.go`,
  `cmd/gateway/audit_test.go`.
- Docs: `docs/OPERATIONS.md` audit-verification section;
  `docs/reports/COMPLETION_CHECKLIST.md` M3-6 row.
