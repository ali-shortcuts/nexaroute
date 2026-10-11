#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:$PATH"

required_go="1.24"
actual_go="$(go env GOVERSION | sed 's/^go//')"
if [[ "$(printf '%s\n%s\n' "$required_go" "$actual_go" | sort -V | head -n1)" != "$required_go" ]]; then
  echo "dependency audit requires Go ${required_go} or newer; found ${actual_go}" >&2
  exit 1
fi

echo "Go toolchain: ${actual_go} (module minimum: ${required_go})"
echo "Checking declared module graph..."
go list -m all >/dev/null
go list -deps ./... >/dev/null

echo "Verifying downloaded module checksums..."
go mod verify

if command -v govulncheck >/dev/null 2>&1; then
  echo "Running govulncheck..."
  govulncheck ./...
else
  echo "WARN: govulncheck is not installed; local vulnerability analysis was not run." >&2
  echo "      Install golang.org/x/vuln/cmd/govulncheck; CI Security scan is still required." >&2
fi

echo "DEPENDENCY AUDIT PASS"
