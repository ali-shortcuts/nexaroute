# A9 — SSE sequence and resume contract evidence (#79)

Branch: `opencode/issue79-20260929181708` (from current `main`, no history rewrite).
Scope: added `internal/httpapi/sse_resume_test.go` ONLY. No production files
touched; `/admin/api/snapshot` (`internal/httpapi/admin.go:adminSnapshot`)
unchanged. Broad PR #68 (`origin/fix/sse-resume-and-benchmarks-v2`,
440-line `admin_events_resume_test.go` + unrelated deletions) was NOT
modified — this is the separate small PR/report for #79.

## Contract (implementation: `internal/httpapi/admin_events.go`, `internal/events/bus.go`)

- Frame wire format: `id: <seq>\nevent: event\ndata: <event JSON>\n\n`,
  with `id == payload seq`, seq monotonic from `Bus.Add`.
- Preamble: `: connected\n\n` commits headers on idle streams.
- Resume cursor: `?since=<seq>` takes precedence, else `Last-Event-ID`
  header; replay = events with `Seq > cursor` via `SubscribeSnapshotSince`
  (atomic snapshot + live subscribe, no gap).
- Errors: bad `since` / `Last-Event-ID` → 400; missing/wrong admin key → 401
  (resume cursors included).

## Wire-format evidence (from `TestSSEWireFormatResume -v`)

Raw preamble: `": connected\n\n"`

Raw data frame:

```text
id: 1
event: event
data: {"seq":1,"time":"2026-09-29T18:19:01.342384599Z","kind":"wire-1","message":"hello"}

```

Assertions: exactly 3 lines (`id:` / `event: event` / `data:`), blank-line
terminator, `id == data.seq == 1`, `Content-Type: text/event-stream`.

## Test results (exact)

- `go test ./internal/httpapi/ -run 'SSE.*Resume' -count=1 -v` → 8/8 PASS
  (WireFormat, SinceQuery, LastEventID, SincePrecedence, NoDuplicateReorder,
  AuthEnforced incl. 4 unauth subcases + 2 auth subcases, InvalidCursor,
  SnapshotUnchanged).
- `go test ./internal/httpapi/ -run 'SSE.*Resume' -count=1 -race` → ok.
- `go build ./...` → clean.
- `go vet ./...` → clean.
- `go test ./... -count=1` → all packages ok (httpapi 6.154s).
- `scripts/verify.sh` → see gate log below (full suite + race + fuzz +
  benchmarks + linux builds).

Final gate (this branch, commit `517236c`): `scripts/verify.sh` → `VERIFY PASS`
(full clean + shuffle unit/integration, vet, full race + shuffle race,
fuzz smoke, benchmark smoke, linux amd64 + arm64 builds).

## Overlap note vs broad PR #68

`origin/fix/sse-resume-and-benchmarks-v2` overlaps in intent (resume tests)
but is broad: deletes `ProductionEventKinds`, health views, production
events, dashboard ring, rewrites web UI. This A9 change does not touch any
of that; no merge conflict is introduced by this file (new path
`internal/httpapi/sse_resume_test.go` vs their
`internal/httpapi/admin_events_resume_test.go`).

## Compatibility note

`go.mod` (`go 1.23`) untouched. No new dependencies. Runner `go1.24.13`
builds cleanly; no old-branch CI compatibility error reproduced on current
main, so no compat fix was required beyond keeping the change additive.
