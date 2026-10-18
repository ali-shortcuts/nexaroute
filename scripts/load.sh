#!/usr/bin/env bash
# Issue #437: reproducible local load-profile runner.
#
# Usage: scripts/load.sh [label]
#
# Runs the deterministic mixed-workload profile in
# internal/httpapi/loadprofile_test.go against loopback httptest
# upstreams only (no network, no secrets) and appends the environment
# capture plus the full test log to benchmarks/load/<label>.txt.
#
# Knobs (all optional, all recorded in the output):
#   NEXAROUTE_LOAD_POOL         candidate-pool size           (default 100)
#   NEXAROUTE_LOAD_OPS          total operations              (default 1000)
#   NEXAROUTE_LOAD_WORKERS      concurrent workers            (default 16)
#   NEXAROUTE_LOAD_SEED         deterministic workload seed   (default 1)
#   NEXAROUTE_LOAD_SUBSCRIBERS  slow dashboard subscribers   (default 2)
#
# The profile measures local gateway overhead separately from the fixed
# upstream delay and asserts only bounded-state/liveness invariants;
# it never asserts a wall-clock threshold. See
# docs/reports/ISSUE437_LOAD_PROFILE.md.
set -euo pipefail
cd "$(dirname "$0")/.."

LABEL="${1:-$(date +%Y%m%d-%H%M%S)}"
OUT_DIR="benchmarks/load"
mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/$LABEL.txt"

pool="${NEXAROUTE_LOAD_POOL:-100}"
ops="${NEXAROUTE_LOAD_OPS:-1000}"
workers="${NEXAROUTE_LOAD_WORKERS:-16}"
seed="${NEXAROUTE_LOAD_SEED:-1}"
subs="${NEXAROUTE_LOAD_SUBSCRIBERS:-2}"

{
  echo "# issue437 load profile — label=$LABEL"
  echo "# date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# go: $(go version)"
  echo "# cpu: $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || echo unknown)"
  echo "# nproc: $(nproc 2>/dev/null || echo unknown)"
  echo "# args: pool=$pool ops=$ops workers=$workers seed=$seed subscribers=$subs"
} | tee "$OUT"

NEXAROUTE_LOAD=1 \
NEXAROUTE_LOAD_POOL="$pool" \
NEXAROUTE_LOAD_OPS="$ops" \
NEXAROUTE_LOAD_WORKERS="$workers" \
NEXAROUTE_LOAD_SEED="$seed" \
NEXAROUTE_LOAD_SUBSCRIBERS="$subs" \
  go test -count=1 -run '^TestLoadProfileMixedWorkload$' -v ./internal/httpapi/ 2>&1 | tee -a "$OUT"

echo "load profile written to $OUT"
