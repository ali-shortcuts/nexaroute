# CCR-parity control plane: status and handoff

This file is the living handoff for the single PR on branch
`feat/ccr-parity-control-plane`. Any builder agent (or human) can pick the work
up from here. Update the checklist in the same commit as the code it describes.

- Spec (source of truth): [SPEC.md](SPEC.md)
- Rule of thumb: ONE PR, English only, no heavy dependencies, UI stays embedded
  in the Go binary, do not break routing, failover, hedging, cache, hashed
  client keys, admin auth/CSRF, privacy fields or the video page.

## Honest state

Nothing from SPEC.md section 2 is implemented on this branch yet. The branch
contains only this handoff and the spec. An earlier attempt lived in a private
sandbox that no longer exists; none of its code or test results were recovered,
so nothing from it may be claimed as done or verified.

## Checklist (update as work lands)

Status values: `todo`, `in-progress`, `code-done (unverified)`, `verified`.
Only mark `verified` after the commands in "Verification" pass on a machine
that can build the module.

| Spec | Item | Status |
|------|------|--------|
| 2.1 | Global Endpoint pill (readyz dot, URL copy, 5s poll) | todo |
| 2.2 | Providers table (search, Models pill, usage, toggle, edit, delete, expand) | todo |
| 2.3 | Add/Edit provider modal (preset, tiles, pool editor, pick models, test, loading) | todo |
| 2.4 | Controlled secret reveal (`reveal-secret`, `admin.allow_secret_reveal`, audit, rate limit) | todo |
| 2.5 | Account usage column (quota headers, optional `usage_endpoint`) | todo |
| 2.6 | API Keys page, "Local Gateway" seed, expiry and limits in client-auth middleware | todo |
| 2.7 | Agent profiles API + page (Claude Code, Codex), per-profile client key | todo |
| 3.8 | Secret-leak canary test (snapshot, SSE, metrics, logs) | todo |
| 3.10 | README refresh, docs, CHANGELOG, `docs/SECURITY.md` trade-off note | todo |

## Suggested order

1. 2.4 backend first (reveal endpoint, config switch, audit, rate limit), since
   the modal and the API Keys page depend on it.
2. Client-key changes (2.6 backend): `key_prefix`, expiry, limits, seeded key,
   protocol-shaped 401 for expired keys. Keep keys hashed at rest.
3. Agent profiles backend (2.7): CRUD, validation, RBAC via `admin_authz`.
4. Account usage backend (2.5).
5. UI: endpoint pill, providers table, modal, API Keys page, agent profiles
   page. Put new UI code in a new embedded file rather than growing `app.js`.
6. Leak canary test, round-trip tests (Go and JS), docs, README, CHANGELOG.

## Known design decisions and caveats

- Client keys are hashed at rest, so the full key cannot be shown later unless
  an encrypted revealable copy is stored (only when `admin.allow_secret_reveal`
  is true). Otherwise show it once at creation and say so in the UI.
- Creating the first client key turns on `/v1` authentication. Warn about it
  in the UI.
- A web page cannot launch local processes, so the profile "play" button is
  not possible from the browser. Offer a copyable launch command instead.
- Env var names containing KEY, TOKEN or SECRET should be rejected in custom
  profile env lists, except the generated auth token.
- The Codex `config.toml` snippet and the `ANTHROPIC_DEFAULT_FABLE_MODEL`
  variable must be checked against the real clients before being called done.

## Environment notes for the next builder

- The module requires Go >= 1.25.0 (`go.mod`).
- In the cloud sandbox used so far, Go 1.25.1 was preinstalled at
  `/usr/local/go1.25.1/bin`, but module downloads from `proxy.golang.org` were
  denied by the session egress proxy (HTTP 403), so `go build ./...` could not
  run there. Do not work around the proxy; run the build where modules are
  reachable, or vendor them.
- No local verification of any kind has been run for this feature yet.

## Verification (run all before marking anything `verified`)

```
go vet ./...
go test -race ./...
node --check internal/httpapi/web/app.js
```

Plus the new tests required by SPEC.md section 3 and the check that the UI
works at 1280px and 390px widths.
