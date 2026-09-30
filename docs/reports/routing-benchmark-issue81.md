# Routing benchmark evidence — issue #81 (parent #51)

Implementation: `internal/httpapi/routing_bench_test.go`.
Compares the real gateway routing hot path
(`POST /v1/chat/completions` → router → local deterministic upstream)
against a direct upstream `POST` to the same deterministic server.

Local-only: `httptest` loopback servers, `AuthMode: none`, no secrets,
no external provider calls. No competitor numbers, no production SLO claims.

## Environment

- Machine: `Linux runnervmtr4k5 6.17.0-1022-azure #22-Ubuntu SMP x86_64 GNU/Linux`, 4 vCPU, ~16 GB RAM
- CPU reported by Go benchmark: `AMD EPYC 7763 64-Core Processor`
- Go: `go version go1.24.13 linux/amd64`
- Base commit at work start: `d57940efd3733da204654c30f2b9b295c6d3346d` (main tip, `d57940e`)

## Command (exact, per issue)

```
go test -run=^$ -bench=Routing -benchmem ./...
```

## Gates

```
gofmt -l .        → (empty)
go build ./...    → ok, exit 0
go vet ./...      → ok, exit 0
```

## Raw output (real, 2026-09-30, `internal/httpapi` excerpt)

```
goos: linux
goarch: amd64
pkg: github.com/ali-shortcuts/nexaroute/internal/httpapi
cpu: AMD EPYC 7763 64-Core Processor
BenchmarkRouting_Gateway-4              4363      234509 ns/op     40612 B/op       428 allocs/op
BenchmarkRouting_DirectUpstream-4      10000      105854 ns/op      7224 B/op        84 allocs/op
PASS
ok    github.com/ali-shortcuts/nexaroute/internal/httpapi  2.134s
```

All other packages report `ok` with no matching benchmarks; full run exit 0.

## Sample caveats

- Sample run only: numbers reflect one shared CI VM; loopback `httptest`
  servers include HTTP + JSON overhead, not production network/providers.
- Gateway leg includes routing, handler, translation, and provider forward;
  direct leg is the same upstream bypassing the gateway.
- Do not compare across machines or infer SLOs; re-run the exact command
  above to reproduce.
