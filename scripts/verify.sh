#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

echo '== go version =='
go version

echo '== shell syntax =='
bash -n install-user.sh run-local.sh scripts/*.sh

echo '== formatting =='
if out=$(gofmt -l .) && [[ -n "$out" ]]; then
  echo 'gofmt check failed for:' >&2
  echo "$out" >&2
  while IFS= read -r file; do
    [[ -z "$file" ]] && continue
    echo "--- gofmt diff: $file ---" >&2
    gofmt -d "$file" >&2 || true
  done <<< "$out"
  exit 1
fi

echo '== unit/integration tests =='
go test -timeout=3m -shuffle=on -count=10 ./...

echo '== go vet =='
go vet ./...

echo '== race detector =='
go test -race -timeout=3m -shuffle=on -count=3 ./...

if command -v node >/dev/null 2>&1; then
  echo '== web ui javascript syntax =='
  node --check internal/httpapi/web/app.js
else
  echo 'WARN: node not installed; skipping JavaScript syntax check' >&2
fi

echo '== short fuzz checks =='
GOMAXPROCS=2 go test ./internal/httpapi -run='^$' -fuzz=FuzzPatchJSONModel -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/core -run='^$' -fuzz=FuzzParseAnthContent -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/eval -run='^$' -fuzz=FuzzResolve_Verdicts -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/eval -run='^$' -fuzz=FuzzRunner_Artifacts -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/scorecards -run='^$' -fuzz=FuzzImportJSON -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/scorecards -run='^$' -fuzz=FuzzValueValidation -fuzztime=2s -parallel=2

echo '== linux amd64 build =='
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-amd64 ./cmd/gateway

echo '== linux arm64 build =='
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-arm64 ./cmd/gateway

echo '== sha256sums =='
(cd bin && sha256sum nexaroute-linux-amd64 nexaroute-linux-arm64 > SHA256SUMS)
cp bin/SHA256SUMS SHA256SUMS
test -s SHA256SUMS

echo '== installer test =='
./scripts/test-installer.sh

echo 'VERIFY PASS'
