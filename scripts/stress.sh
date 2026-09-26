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

echo '== HTTP admission stress =='
go test -timeout=2m -count=1 ./internal/httpapi -run='^TestStress'

echo '== concurrent log rotation stress =='
go test -timeout=2m -count=1 ./internal/logging -run='^TestStress'

echo '== evaluation plane stress =='
go test -timeout=2m -count=1 ./internal/eval -run='^TestStress'

echo 'STRESS PASS'
