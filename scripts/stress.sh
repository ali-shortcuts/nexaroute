#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

export NEXAROUTE_STRESS=1

echo '== router scale stress =='
go test -timeout=2m -count=1 ./internal/router -run='^TestStress'

echo '== probe/recovery stress =='
go test -timeout=2m -count=1 ./internal/probe -run='^TestStress'

echo '== event-state stress =='
go test -timeout=2m -count=1 ./internal/events -run='^TestStress'

echo 'STRESS PASS'
