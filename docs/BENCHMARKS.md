# NexaRoute benchmarks (M6-4)

Reproducible hot-path benchmarks for the gateway's per-request
work, a default-vs-tuned profile comparison, and one measured
hotspot fix. The harness is the deliverable; the numbers are one
machine's.

## Reproduce

```sh
scripts/bench.sh all            # default + tuned profiles
scripts/bench.sh default myrun  # labeled results files
```

Fixed settings: `-benchtime 1s -count 5 -benchmem`, curated set
(router candidates, canonical round trips, cache, guardrail scan,
decision/policy scoring, httpapi gateway end-to-end). Results +
environment capture land in `benchmarks/results/<label>-<profile>.txt`
(the runs cited below are committed there as `before-default.txt`,
`after-default.txt`, `after-tuned.txt`).

**Profiles.** `default` = stock Go runtime. `tuned` =
`GOGC=200 GOMEMLIMIT=1GiB GOMAXPROCS=<nproc>` — the knobs an
operator would set on a dedicated host.

**Environment of record.** AMD EPYC 9D25 (2 vCPUs visible), Go
1.27.0, linux/amd64, shared CI-class VM.

## Reading the numbers — noise caveat

This VM is shared and throttled: across two sessions an hour
apart, CPU times for benchmarks the fix could not touch drifted
25–45%. Treat `ns/op` as indicative (same-session comparisons
only) and `B/op` / `allocs/op` as exact — Go allocation counts
are deterministic. The hotspot claim below rests on allocations
and on the memory profile, not on wall-clock deltas.

## Results (medians of 5, default profile)

| Benchmark | before | after fix | tuned (after) |
|---|---|---|---|
| Routing_Gateway (end-to-end chat) | 197,640 ns · 41,072 B · 385 allocs | 185,823 ns · **32,989 B · 355 allocs** | 158,573 ns |
| Candidates10 | 15,294 ns · 5,389 B | 9,301 ns · 5,389 B | 8,012 ns |
| Candidates100 | 194,203 ns · 49,676 B | 146,740 ns · 49,676 B | 84,380 ns |
| AnthropicRoundTrip | 63,762 ns · 10,727 B | 37,534 ns · 10,729 B | 39,811 ns |
| OpenAIChatRoundTrip | 46,609 ns · 7,949 B | 24,675 ns · 7,951 B | 23,865 ns |
| CacheKeyWithContext | 739 ns · 160 B | 475 ns · 160 B | 469 ns |
| CachePutGet | — (bench fixed during M6-4) | ~97 ns · 0 B · 0 allocs | — |
| ScanCleanBody (~1.9 KB) | 591,692 ns | 441,358 ns | 441,543 ns |
| ScanBodyWithFindings | 28,812 ns | 21,441 ns | 21,396 ns |

## Hotspot fix: the dialect registry was rebuilt on every request

CPU/memory profiling of `BenchmarkRouting_Gateway`
(`-cpuprofile`/`-memprofile`) put `internal/compat.Dialects` at
**19.9% of all allocated bytes** (40 MB of 201 MB): `Dialects()`
constructed the entire ~15-profile registry map — each profile
carrying a full capability matrix — on EVERY call, and the one
production caller (`DetectDialect`, on the capability path of
every routed request) needed a single entry.

Fix: the registry is built once at package init
(`dialectTable`); `Dialects()` returns the shared table and is
documented read-only (no code path mutates the map or the
profiles' alias maps; profile values are copied on lookup —
audited before the change). Measured effect on the gateway
benchmark: **−8,083 B/op (−19.7%) and −30 allocs/op**, matching
the profile attribution.

## Known remaining costs (measured, not yet optimized)

- `cloneConfig` (~9% of gateway allocations): the server clones
  the config per request for swap safety. Reducing it means
  fewer `currentConfig()` calls per handler, a deliberate
  follow-up, not a drive-by.
- Guardrail `Scan` costs scale with body size × detector count
  (a ~1.9 KB clean body scans in ~0.44–0.59 ms on THIS throttled
  VM; per-detector timings were uniform, i.e. machine-bound, not
  pattern pathology). Operators enabling guardrails on large
  bodies should benchmark on their own hardware with the harness.
- Router candidate scoring is linear in pool size (~1.5–1.9 µs
  per candidate on this VM): a 100-deployment pool costs
  ~0.15–0.19 ms per request before any upstream work.
