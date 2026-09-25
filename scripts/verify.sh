#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

GO="$(command -v go || true)"
if [[ -z "$GO" ]]; then
  echo 'ERROR: Go 1.23+ is required; install Go and retry.' >&2
  exit 1
fi
GO_VERSION="$(go env GOVERSION)"
if [[ ! "$GO_VERSION" =~ ^go1\.(2[3-9]|[3-9][0-9])([.]|$) ]]; then
  echo "ERROR: Go 1.23+ is required; found ${GO_VERSION}." >&2
  exit 1
fi

echo '== go version =='
go version

echo '== shell syntax =='
bash -n install.sh install-user.sh run-local.sh scripts/*.sh

echo '== formatting =='
if out=$(gofmt -l cmd internal) && [[ -n "$out" ]]; then
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
CC_BIN="$(go env CC)"
if [[ "$(go env CGO_ENABLED)" == "1" ]] && command -v "$CC_BIN" >/dev/null 2>&1; then
  go test -race -timeout=3m -shuffle=on -count=3 ./...
else
  echo "WARN: skipping race detector (requires CGO_ENABLED=1 and C compiler '${CC_BIN}')." >&2
fi

if command -v node >/dev/null 2>&1; then
  echo '== web ui javascript syntax =='
  node --check internal/httpapi/web/app.js
else
  echo 'WARN: node not installed; skipping JavaScript syntax check' >&2
fi

echo '== short fuzz checks =='
GOMAXPROCS=2 go test ./internal/httpapi -run='^$' -fuzz=FuzzPatchJSONModel -fuzztime=2s -parallel=2
GOMAXPROCS=2 go test ./internal/core -run='^$' -fuzz=FuzzParseAnthContent -fuzztime=2s -parallel=2

mkdir -p bin
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  echo "== ${os} ${arch} build =="
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "bin/nexaroute-${os}-${arch}" ./cmd/gateway
done

echo 'VERIFY PASS'
