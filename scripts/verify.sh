#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:$PATH"
echo '== go version =='
go version
echo '== shell syntax =='
bash -n scripts/*.sh
echo '== final release gate regression tests =='
bash .github/scripts/test-final-release-gate.sh
echo '== HA documentation consistency =='
./scripts/check-ha-docs.sh
echo '== flake-hunt harness regression checks =='
bash scripts/test-flake-hunt.sh
echo '== formatting =='
if out=$(gofmt -l .) && [[ -n "$out" ]]; then
  echo 'gofmt check failed for:' >&2
  echo "$out" >&2
  while IFS= read -r file; do
    [[ -n "$file" ]] && gofmt -d "$file" >&2
  done <<< "$out"
  exit 1
fi
echo '== mandatory clean unit/integration pass =='
go test -timeout=3m -count=1 ./...
echo '== go vet =='
go vet ./...
echo '== mandatory clean race pass =='
go test -race -timeout=3m -count=1 ./...
echo '== release artifact manifest checks =='
./scripts/test-release-artifacts.sh
echo '== SBOM generator trust-chain checks =='
./scripts/test-generate-sbom.sh
echo '== coverage gate regression checks =='
./scripts/test-coverage-gate.sh
echo '== web ui javascript syntax =='
node --check internal/httpapi/web/app.js
node --check internal/httpapi/web/visual-agent.js
echo '== real browser control-plane acceptance =='
# The runner reports PASS, FAIL or SKIPPED per suite and exits non-zero
# on FAIL (1) or SKIPPED (2). A local skip is NOT a pass: it is printed
# as SKIPPED and the mandatory CI gate (which provisions the pinned
# browser first) fails on it. This local path therefore never claims
# browser acceptance passed when the suites did not run.
browser_status=0
python3 scripts/run-browser-acceptance.py || browser_status=$?
if [[ "$browser_status" -eq 2 ]]; then
  echo 'BROWSER ACCEPTANCE: SKIPPED locally (the mandatory CI gate runs separately and fails on a skip; see docs/browser-acceptance.md)' >&2
elif [[ "$browser_status" -ne 0 ]]; then
  echo 'BROWSER ACCEPTANCE: FAIL (see the runner diagnostics above)' >&2
  exit 1
fi
echo '== short fuzz checks =='
GOMAXPROCS=2 go test ./internal/httpapi -run='^$' -fuzz=FuzzPatchJSONModel -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/core -run='^$' -fuzz=FuzzParseAnthContent -fuzztime=2s -parallel=2
echo '== linux amd64 build =='
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-amd64 ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-videogen-linux-amd64 ./cmd/videogen
echo '== linux arm64 build =='
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-arm64 ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-videogen-linux-arm64 ./cmd/videogen
printf 'VERIFY PASS\n'
