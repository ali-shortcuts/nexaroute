#!/usr/bin/env bash
# Reproducible benchmark harness (M6-4).
#
# Usage: scripts/bench.sh [default|tuned|all] [label]
#
# Runs the curated hot-path benchmark set with fixed settings
# (-benchtime=1s -count=5 -benchmem) and writes results plus an
# environment capture to benchmarks/results/<label>-<profile>.txt.
# Profiles: default = stock Go runtime settings; tuned =
# GOGC=200, GOMEMLIMIT=1GiB, explicit GOMAXPROCS (operator-tuned
# runtime). Compare medians across runs; shared machines are
# noisy, so the harness fixes everything it controls.
set -euo pipefail
cd "$(dirname "$0")/.."

PROFILE="${1:-all}"
LABEL="${2:-$(date +%Y%m%d-%H%M%S)}"
OUT_DIR="benchmarks/results"
mkdir -p "$OUT_DIR"

BENCH_PKGS=(
  ./internal/router/
  ./internal/protocol/canonical/
  ./internal/cache/
  ./internal/guardrail/
  ./internal/decision/
  ./internal/decision/policy/
  ./internal/httpapi/
)
BENCH_RE='Benchmark(Candidates|AnthropicRoundTrip|OpenAIChatRoundTrip|Cache|Scan|Routing_Gateway|Score|Apply|Orchestrator)'

run_profile() {
  local profile="$1"
  local out="$OUT_DIR/$LABEL-$profile.txt"
  {
    echo "# nexaroute benchmarks — profile=$profile label=$LABEL"
    echo "# date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "# go: $(go version)"
    echo "# cpu: $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || echo unknown)"
    echo "# nproc: $(nproc 2>/dev/null || echo unknown)"
    if [ "$profile" = "tuned" ]; then
      echo "# env: GOGC=200 GOMEMLIMIT=1GiB GOMAXPROCS=$(nproc 2>/dev/null || echo 2)"
      GOGC=200 GOMEMLIMIT=1GiB GOMAXPROCS="$(nproc 2>/dev/null || echo 2)" \
        go test -run xxx -bench "$BENCH_RE" -benchtime 1s -count 5 -benchmem "${BENCH_PKGS[@]}"
    else
      echo "# env: stock (GOMAXPROCS=$(go env GOMAXPROCS 2>/dev/null || echo default))"
      go test -run xxx -bench "$BENCH_RE" -benchtime 1s -count 5 -benchmem "${BENCH_PKGS[@]}"
    fi
  } | tee "$out"
}

case "$PROFILE" in
  default) run_profile default ;;
  tuned) run_profile tuned ;;
  all) run_profile default; run_profile tuned ;;
  *) echo "usage: scripts/bench.sh [default|tuned|all] [label]" >&2; exit 2 ;;
esac
