#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:$PATH"
echo '== go version =='
go version
echo '== shell syntax =='
bash -n scripts/*.sh
echo '== formatting =='
if out=$(gofmt -l .) && [[ -n "$out" ]]; then
  echo 'gofmt check failed for:' >&2
  echo "$out" >&2
  exit 1
fi
echo '== mandatory clean unit/integration pass =='
go test -timeout=3m -count=1 ./...
echo '== go vet =='
go vet ./...
echo '== mandatory clean race pass =='
go test -race -timeout=3m -count=1 ./...
echo '== web ui javascript syntax =='
node --check internal/httpapi/web/app.js
node --check internal/httpapi/web/visual-agent.js
echo '== real browser control-plane acceptance =='
if command -v chromium >/dev/null 2>&1 && python3 -c 'import playwright' >/dev/null 2>&1; then
  python3 scripts/test-browser-e2e.py
  echo '== real browser Live Visual Agent acceptance =='
  python3 scripts/test-live-visual-agent.py
else
  echo 'WARN: Chromium + Python Playwright unavailable; skipping browser acceptance' >&2
fi
echo '== short fuzz checks =='
GOMAXPROCS=2 go test ./internal/httpapi -run='^$' -fuzz=FuzzPatchJSONModel -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/core -run='^$' -fuzz=FuzzParseAnthContent -fuzztime=2s -parallel=2
echo '== linux amd64 build =='
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-amd64 ./cmd/gateway
echo '== linux arm64 build =='
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-arm64 ./cmd/gateway
printf 'VERIFY PASS\n'
