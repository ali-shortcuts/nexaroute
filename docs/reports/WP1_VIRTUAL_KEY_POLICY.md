# WP1 — virtual-key policy intersection

## Scope

Integrated `fix/virtual-key-policy-intersection` onto `main` as branch
`wp1-virtual-key-policy`. The change is security-sensitive client-auth policy
hardening and duplicate-field inspection.

## Security review

- Key-level route/model allow-lists are checked first.
- With a configured tenant hierarchy, project and team policies are ceilings;
  a child wildcard cannot widen a parent allow-list.
- Unknown tenant, project, or team references deny access.
- A team reference without a project reference denies access.
- Duplicate top-level `model` values deny model authorization rather than
  depending on the JSON parser's first/last-value behavior.
- Duplicate `max_tokens` and `max_output_tokens` values use the maximum value;
  malformed or negative values fail closed for token budgets.
- Request bodies are restored after bounded inspection.
- No credentials or secret values are logged or returned.

## Tests

Added/retained meaningful negative tests for allowed intersection, project/team
widening attempts, unresolved scopes, duplicate model fields, duplicate token
limits, large-body inspection, and request-body preservation.

## Gate result

Full gate output: [`wp1-virtual-key-policy-gates.raw.txt`](wp1-virtual-key-policy-gates.raw.txt)

- `gofmt -l .` — empty
- `go vet ./...` — PASS
- `go test -race -count=1 ./...` — PASS
- `./scripts/verify.sh` — `VERIFY PASS`, browser and Live Visual Agent E2E PASS
- `./scripts/smoke-local.sh` — `SMOKE PASS`
- `./scripts/build-release.sh v0.7.0` — amd64/arm64 artifacts valid
- `./scripts/test-install.sh` — `INSTALL PASS`, installer E2E PASS
- coverage — **79.6%** repository-wide
- `git diff --check` — PASS

No release, tag, repository setting, secret, or unrelated branch was changed.
