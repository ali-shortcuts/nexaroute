#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

rounds="${NEXAROUTE_SOAK_ROUNDS:-3}"
if ! [[ "$rounds" =~ ^[1-9][0-9]*$ ]]; then
  echo "NEXAROUTE_SOAK_ROUNDS must be a positive integer" >&2
  exit 2
fi
if (( rounds > 20 )); then
  echo "NEXAROUTE_SOAK_ROUNDS must be <= 20" >&2
  exit 2
fi

echo "== repeated bounded stress: ${rounds} rounds =="
for i in $(seq 1 "$rounds"); do
  echo "-- stress round $i/$rounds --"
  NEXAROUTE_STRESS=1 go test -timeout=3m -count=1 ./internal/router ./internal/probe ./internal/events ./internal/httpapi ./internal/logging -run='^TestStress'
done

echo '== same-process HTTP hot-reload soak =='
NEXAROUTE_SOAK=1 go test -timeout=10m -count=1 ./internal/httpapi -run='^TestSoak'

echo '== same-process recovery soak =='
NEXAROUTE_SOAK=1 go test -timeout=10m -count=1 ./internal/probe -run='^TestSoak'

echo '== race-enabled soak =='
GOMAXPROCS=4 NEXAROUTE_SOAK=1 go test -race -timeout=15m -count=1 ./internal/httpapi ./internal/probe -run='^TestSoak'

echo 'SOAK PASS'
