#!/usr/bin/env bash
# Reproducible model-quality regression gate check.
#
# Runs the gate unit tests, then exercises the built CLI against the
# checked-in offline examples in docs/quality-gate/examples/. No network
# call, no live provider call and no routing change happen here: all
# three comparisons are replay evidence, and the expected exit codes
# are 0 (no regression), 1 (regression) and 3 (insufficient evidence).
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/go/bin:/usr/local/go/bin:$PATH"

echo '== gate unit tests =='
go test -count=1 ./internal/eval/ ./internal/qualitygate/ ./cmd/gateway/ -run 'TestQualityGate|TestCompare|TestSummarize|TestCheckLive|TestParseBaseline|TestThresholds|TestReport_'

echo '== build quality-gate binary =='
tmpbin="$(mktemp -d)/nexaroute"
trap 'rm -rf "$(dirname "$tmpbin")"' EXIT
CGO_ENABLED=0 go build -trimpath -o "$tmpbin" ./cmd/gateway

check() { # <candidate> <expected-exit> <label>
  local cand="$1" want="$2" label="$3"
  set +e
  "$tmpbin" quality-gate compare \
    --baseline docs/quality-gate/examples/baseline.json \
    --candidate "docs/quality-gate/examples/$cand" \
    --out "$(mktemp -d)/report.json" >/dev/null
  local got=$?
  set -e
  if [[ "$got" != "$want" ]]; then
    echo "quality-gate $label: exit $got, want $want" >&2
    exit 1
  fi
  echo "quality-gate $label: exit $got (expected)"
}

echo '== offline example comparisons =='
check candidate-same.json 0 "same candidate"
check candidate-regressed.json 1 "regressed candidate"
check candidate-insufficient.json 3 "insufficient candidate"

echo 'QUALITY GATE CHECK PASS'
