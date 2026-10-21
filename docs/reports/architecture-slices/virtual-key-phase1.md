# Virtual Key mutation architecture — phase 1 (revoke)

- Parent audit: [#593](https://github.com/ali-shortcuts/nexaroute/issues/593) (stays open)
- Slice issue: [#607](https://github.com/ali-shortcuts/nexaroute/issues/607)
- Branch: `opencode/issue607-20261021154037`
- Base `main` SHA: `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d`
- Code/test SHA: `c43a5960079f3d22148eb6e3c1c4962d285b7c45`
- This report is the immediately following docs-only commit on the same branch; CI
  and Security must be evaluated on the PR head, not on the base SHA.
- Release/tag impact: none. `v0.23.0` was not modified, moved, or re-released.

## Selected slice

The smallest cohesive mutation path is **revoke**:

```text
DELETE /admin/api/client-keys/{id}
  -> Server.adminClientKeyByID
  -> virtualkeys.Service.Revoke
  -> Server.mutateConfig (atomic clone, validation, persistence, runtime swap)
```

Create and rotate remain in the HTTP package for later slices. Revoke was chosen
first because it has no key generation, no response secret, no replace/append
semantics, and no partial-conflict state; it still exercises the same config
mutation, not-found, idempotency, authorization, audit, and persistence
boundaries that the other mutations need.

## Domain contract

`internal/virtualkeys/service.go`:

```go
type MutateFunc func(func(*config.Config) error) (config.Config, error)

type Service struct { mutate MutateFunc }

func NewService(mutate MutateFunc) *Service
func (s *Service) Revoke(id string) error
```

Contract:

1. An empty ID returns `ErrIDRequired` without touching persistence.
2. A missing mutator (including a nil `*Service`) returns
   `ErrMutationUnavailable`; it never silently succeeds.
3. Only the key whose `ID` exactly matches is changed. Every other key and every
   metadata field on the target (tenant/project/team scope, role, allowed
   models/routes, expiry, RPM/TPM, privacy, budget, hash) is preserved.
4. Revoking an already revoked key succeeds without appending, replacing, or
   removing an entry. The operation is idempotent.
5. An unknown ID returns `*NotFoundError`, whose text is exactly
   `client key "<id>" not found`, matching the pre-extraction handler.
6. Persistence/validation errors from the injected `MutateFunc` are returned
   unchanged. The HTTP handler keeps its existing error mapping, so the public
   status/body contract does not change.

The service deliberately does **not** authenticate, authorize, emit audit
events, or parse HTTP. Those concerns stay in the existing middleware and handler
so the authorization decision and the `attempt -> completed/denied` audit ordering
remain identical to every other admin route.

## Exact route and function map

| Concern | Location |
| --- | --- |
| Route registration | `internal/httpapi/server.go:1088` (`/admin/api/client-keys/`) |
| HTTP dispatch, rotate path still local | `internal/httpapi/client_keys.go:131` (`adminClientKeyByID`) |
| Revoke handler now delegates | `internal/httpapi/client_keys.go:174` |
| Domain service | `internal/virtualkeys/service.go:33` (`Service.Revoke`) |
| Authorization matrix | `internal/httpapi/admin_authz.go:77` (`authz.ManageKeys`) |
| Pre/post audit | `internal/httpapi/server.go:1329-1344` |
| Denied audit | `internal/httpapi/admin_auth.go:241` (`admin.access.denied`) |
| Atomic mutation | `internal/httpapi/server.go:843` (`mutateConfig`) |

## Behavior preservation

| Behavior | Before extraction | After extraction |
| --- | --- | --- |
| Allowed role | `admin`/`owner` with `keys.manage` -> 204 | Same; `admin` regression-tested through a real OIDC session |
| Denied roles | `operator`/`viewer` (and any role without `keys.manage`) -> 403 | Same; both tested, target key stays active |
| Empty body response | 204 with no body | Same, asserted |
| Unknown ID | 404, `client key "<id>" not found` | Same text and status, asserted |
| Repeat revoke | 204 and no duplicate/mutation | Same, asserted twice |
| Scope metadata | Untouched | Deep-equality assertion over all fields |
| Secret handling | No key material in revoke response | No body and explicit hash-leak assertion |
| Persistence failure | `mutateConfig` error -> existing handler 404 mapping; config unchanged | Same mapping and runtime state, tested with an unwritable config directory |
| Audit ordering | `admin.operation.authorized` before `admin.operation.completed`; denials use `admin.access.denied` | Same events/order, asserted from the security store |
| Failure to authorize | Denied before handler/service mutation | Same; no authorized/completed event for denied roles |

The persistence-failure response remains the pre-existing 404 mapping. This slice
intentionally preserves it rather than changing the public API. It is documented
here as a separate API-contract decision for a later slice, not silently changed.

## Regression tests

Unit tests (`internal/virtualkeys/service_test.go`):

- `TestRevokeMarksOnlyTargetAndPreservesMetadata`
- `TestRevokeIsIdempotent`
- `TestRevokeUnknownKeyFailsClosed`
- `TestRevokeRequiresID`
- `TestRevokePropagatesPersistenceFailure`
- `TestRevokeUnavailableWithoutMutator`

HTTP/authz/persistence tests (`internal/httpapi/client_keys_test.go`):

- `TestAdminClientKeyRevokeContractAndIdempotency` — 204/no body, no hash leak,
  metadata deep equality, repeat revoke, unknown-ID 404 text.
- `TestAdminClientKeyRevokeRequiresManageKeysRole` — `admin` allowed;
  `operator` and `viewer` denied with no mutation; audit pair/order and denied
  event verified.
- `TestAdminClientKeyRevokePersistenceFailureLeavesKeyActive` — unwritable
  config directory forces a persistence failure; existing 404 mapping is kept,
  no secret leaks, and the key remains active.

The role test uses real OIDC sessions because the session authenticator accepts
`viewer`, `operator`, and `admin` claims. `owner` remains covered by the existing
authorization matrix tests; it is not an accepted OIDC session role.

## Verification evidence

Baseline before changes:

```text
go test ./internal/httpapi ./internal/authz ./internal/config \
  -run 'TestAdminClientKey|TestVirtualKey|TestViewer|TestOIDCRole|TestAdminOpenAPIViewer|TestRolePermission' \
  -count=1
EXIT 0
```

After changes, on the final working tree:

```text
go test ./internal/virtualkeys ./internal/httpapi -run 'TestRevoke|TestAdminClientKeyRevoke' -count=1
EXIT 0

go test ./internal/virtualkeys ./internal/httpapi -count=1
EXIT 0

gofmt -l .
(empty)

go test -timeout=30m -count=1 ./...
EXIT 0 (all packages ok, including internal/virtualkeys)

go vet ./...
EXIT 0

go test -race -timeout=30m -count=1 ./...
EXIT 0 (all packages ok)

./scripts/verify.sh
VERIFY PASS
EXIT 0
```

`verify.sh` also reported `WARN: Chromium + Python Playwright unavailable;
skipping browser acceptance`. That is a skip, not a pass; this slice has no UI
change. Raw local logs were kept in the gitignored
`.artifacts/architecture-slices/` directory. Fresh CI and Security results must be
read from the PR head SHA; they are not claimed here.

## Remaining duplication and next endpoint

Still local to `internal/httpapi/client_keys.go`:

1. **Next: rotate** (`POST /admin/api/client-keys/{id}/rotate`,
   `client_keys.go:138`). It shares lookup/revoke with this slice and adds key
   generation plus append/replace semantics. Migrate it second, preserving the
   one-time plaintext response and its current conflict behavior.
2. **Then: create** (`POST /admin/api/client-keys`,
   `client_keys.go:74`). It needs validated input, expiry/role defaults, key
   generation, budget-alert fields, and first-persist failure mapping. Migrate
   it after rotate.
3. The existing GET/list handler and budget live-state decoration are read-only
   and out of scope.

The eventual service can own one ID/key generator and one lookup helper, but this
slice does not introduce unused create/rotate methods. That avoids a parallel
implementation and keeps the reviewed contract limited to revoke.

## Boundaries honored

- No public API schema, status/body, role policy, or secret-handling change.
- No edits to `Config.Validate` or scanner slices.
- No generic anonymous helper: the contract is expressed as a Virtual Key
  mutation service with typed domain errors.
- No changes to tenant/project/team scope intersection logic; revoke cannot widen
  or rewrite scope, and tests prove metadata is byte-for-byte preserved.
- Parent issue #593 remains open.

## Remaining blockers / unverified

- Fresh CI and Security checks for the PR head SHA must be observed after the PR
  is created.
- The pre-existing persistence-failure 404 mapping is preserved for compatibility
  and remains a candidate for a separately approved API-contract change.
- Create and rotate duplication remains until their focused slices land.
- No live PostgreSQL/Redis/OIDC/provider verification was required or performed
  for this process-local config mutation. OIDC session tests use the existing
  in-process test provider.
