# Phase 2 Coverage Expansion Report

**Branch:** `phase1-coverage-on-1b`
**Base:** `phase1b-on-1a`
**Baseline:** `77.4%` (the Phase 1b-on-1a baseline)
**Final measured coverage:** `79.5%`
**Target:** `85.0%`

## Scope

This pass added behavior-focused tests only; no assertion-free padding was used. The exercised areas were:

- guardrail fail-closed and boundary behavior;
- control-plane manager and Redis-lease validation/error behavior;
- provider HTTP adapter CountTokens, error taxonomy, headers, quota identity and streaming close/cancel wrappers;
- usage saturation, negative pricing safety and accounting invariants;
- gateway config/secrets CLI exit contracts;
- compat Responses and Anthropic protocol adapters, probe verdicts and conversion errors;
- HTTP API routing/policy/retry/request-inspection pure behavior;
- decision reason-code bounded contract.

## Coverage by package

| Package | Baseline/reference | Final | Notes |
|---|---:|---:|---|
| `internal/compat` | 70.2% reference | 81.0% | Responses/Anthropic suites and probe conversion branches added |
| `internal/config` | 70.7% reference | 75.7% | Validation matrix and flag semantics added |
| `internal/controlplane` | n/a | 86.8% | Manager/lease error behavior added |
| `internal/decision` | 68.0% reference | 77.0% | Reason-code bounded contract added |
| `internal/httpapi` | 73.7% reference | 74.4% | Routing, retry and request inspection behavior added |
| `internal/probe` | n/a | 72.1% | Existing recovery suite remains the limiting area |
| `internal/providers` | n/a | 83.9% | Adapter transport and streaming lifecycle behavior added |
| `internal/usage` | n/a | 98.9% | Saturation and negative-price safety covered |
| `cmd/gateway` | n/a | 75.5% | Config/secrets CLI contracts added |

## Gate evidence

Raw output is in [`phase2-coverage-gates.raw.txt`](phase2-coverage-gates.raw.txt). The final profile and function-level output are:

- [`phase2-final.cover`](phase2-final.cover)
- [`phase2-final-cover-func.raw.txt`](phase2-final-cover-func.raw.txt)

All requested gates passed:

- `gofmt -l .`
- `go vet ./...`
- `go test -race -count=1 ./...`
- `./scripts/verify.sh` — including browser acceptance and short fuzz checks
- `./scripts/smoke-local.sh`
- `./scripts/build-release.sh v0.7.0`
- `./scripts/test-install.sh`
- full coverage profile generation
- `git diff --check`

## Exact remaining gap

The target is **not reached**: `85.0% - 79.5% = 5.5 percentage points` remain.

The remaining gap is concentrated in broad, integration-heavy packages rather than untested small utilities. The largest measured package gaps are:

- `internal/probe`: **72.1%** — recovery worker and capability-store integration paths;
- `internal/httpapi`: **74.4%** — admin provider discovery/test/evaluation paths and protocol data-plane branches;
- `internal/config`: **75.7%** — large validation/persistence surface;
- `cmd/gateway`: **75.5%** — lifecycle and secret rotation branches;
- `internal/decision`: **77.0%** — orchestrator decision/fallback branches.

Reaching 85% would require another focused pass over those integration-heavy paths. No merge or history rewrite was performed.
