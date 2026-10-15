# M4-5 — Admin API OpenAPI, generated from and contract-tested against the implementation

## Sources of truth (all in-repo, verified)

- Route registrations: `internal/httpapi/server.go` — every `/admin`
  route is registered there and nowhere else.
- Method + permission matrix: `adminPermissionForRequest` in
  `internal/httpapi/admin_authz.go` (fail-closed; PATCH/OPTIONS/TRACE
  are never allowed), with permissions from `internal/authz`
  (config.read/write, providers.read/write, routing.read/write,
  usage.read, audit.read, evaluation.run, keys.manage).
- Error envelope: `errorJSON` — `{"error":{"type":"gateway_error",
  "message":"..."}}`.
- Auth: session cookie `nexaroute_admin_session`; mutations require
  the double-submit CSRF pair (cookie + `X-NexaRoute-CSRF` header),
  obtained from `GET /admin/api/csrf-token` → `{"token": "..."}`.
- Pagination: the audit endpoint takes `limit` (1..1000, default 100)
  and returns `{"events": [...]}` newest-first; there is no offset or
  cursor pagination anywhere in the Admin API. There are no
  idempotency keys; the spec states this rather than inventing them.

## Deliverable

`docs/api/admin-openapi.yaml` (OpenAPI 3.1) covering every registered
admin route with its methods, required permission (as a machine
readable `x-permission` extension), the session/CSRF security schemes,
the standard error responses, and concrete response schemas only
where verified from handler code (csrf-token, audit). All other
responses are documented at envelope level — no invented fields.

## Contract tests (`internal/httpapi/admin_openapi_contract_test.go`)

1. Spec paths == the route patterns registered in server.go
   (normalized), so a route added without a spec update fails CI.
2. For every documented operation, `adminPermissionForRequest` allows
   exactly that method on that path with exactly the documented
   permission — and every matrix-allowed operation is documented.
3. Live probes against a real server with an owner session: every
   documented operation answers (never a mux 404, never 405); an
   unauthenticated request gets the documented 401 error envelope;
   the audit `limit` contract (default, limit=1, limit=0 → 400) is
   exercised for real. Where a probe disagrees with the matrix (a
   handler rejecting a matrix-allowed method), the SPEC is trimmed to
   the handler's behavior — behavior wins.
