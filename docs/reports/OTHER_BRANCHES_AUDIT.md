# Other branches audit

Date: 2026-10-10
Base used for comparison: `origin/phase1b-on-1a` (`c986bda`)
Coverage branch: `phase1-coverage-on-1b`

## `chore/video-gateway-audit-hardening`

- Tip: `376cd10` (`release: assemble integrated gateway and video pipeline`).
- Scope: adds the video gateway/runtime, queue/storage/composer/orchestrator/provider layers, `cmd/videogen`, release wiring, UI changes, and client-auth hardening.
- Full `go test -count=1 ./...`: **PASS**.
- Trial merge into the coverage branch with `git merge --no-commit --no-ff`: **CONFLICT** in `cmd/gateway/main.go` and `internal/httpapi/web/app.js`; merge was aborted and no changes were retained.
- Relationship: not a coverage-only change; it is a large release/integration branch and must be merged only after the phase-1 stack is intentionally integrated and the conflicts are resolved by the owner.

## `fix/virtual-key-policy-intersection`

- Tip: `f5f415f` (`test: keep legacy tenant metadata usage fixture`).
- Scope: client-auth hardening for virtual-key tenant metadata/policy scope intersection, duplicate metadata rejection, large-payload inspection, and associated tests.
- Full `go test -count=1 ./...`: **PASS**.
- Trial merge into the coverage branch with `git merge --no-commit --no-ff`: **clean** (stopped before commit); it was aborted and no changes were retained.
- Relationship: overlaps `internal/httpapi/clientauth.go` and related tests, so it should not be combined casually with other client-auth changes. It is not included in this coverage branch because the requested coverage work does not authorize selecting or merging it.

## Conclusion

No other branch was merged, and no source changes from either audit branch were copied into this branch. The video branch has a real textual conflict with the phase-1 gateway/UI state; the virtual-key branch is mergeable at this snapshot but remains a separate policy/security change.
