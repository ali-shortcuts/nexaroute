#!/usr/bin/env bash
# Deterministic repository-wide statement-coverage gate for issue #663.
#
# Canonical measurement (see .github/workflows/coverage-report.yml):
#   go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...
# This gate reads ONLY the total line of `go tool cover -func` over that
# profile. No package, file, or statement is excluded, and the denominator
# is whatever the profile contains: excluding a package, lowering the
# threshold, or changing the denominator to make the gate pass is not
# acceptable. The threshold is inclusive: a total exactly equal to the
# threshold passes.
#
# The gate is deterministic: the same profile always yields the same
# verdict, with no network, no clock, and no sleep. Any failure to read
# or parse the profile fails closed (exit 1).
#
# Toolchain caveat: statement counts differ between Go releases (local
# Go 1.27.0 denominators are larger than CI Go 1.27.3 denominators), so
# the CI artifact produced by coverage-report.yml is authoritative and
# a local total must never be quoted as a CI total.
#
# Run from the module root whose packages the profile covers: `go tool
# cover` resolves the profile's import paths through that module (the
# workflow runs from the repository root).
#
# Usage: ./scripts/coverage-gate.sh [profile] [threshold]
#   profile   coverage.out path (default: .artifacts/coverage/coverage.out)
#   threshold minimum total percentage, inclusive (default: 95.0)
set -euo pipefail

profile="${1:-.artifacts/coverage/coverage.out}"
threshold="${2:-95.0}"

if [[ ! -r "$profile" ]]; then
  echo "::error::coverage gate: profile not readable: $profile" >&2
  exit 1
fi

if [[ ! "$threshold" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
  echo "::error::coverage gate: threshold is not a number: $threshold" >&2
  exit 1
fi

func_out=""
if ! func_out="$(go tool cover -func="$profile" 2>&1)"; then
  echo "::error::coverage gate: go tool cover failed on $profile" >&2
  printf '%s\n' "$func_out" >&2
  exit 1
fi

total_field="$(printf '%s\n' "$func_out" | tail -n 1 | awk '{print $NF}')"
total="${total_field%\%}"

if [[ ! "$total" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
  echo "::error::coverage gate: could not parse total coverage from $profile (last line: $(printf '%s\n' "$func_out" | tail -n 1))" >&2
  exit 1
fi

if awk -v t="$total" -v th="$threshold" 'BEGIN { exit !(t + 0 >= th + 0) }'; then
  echo "coverage gate: PASS total=${total}% (threshold >= ${threshold}%) profile=$profile"
  exit 0
fi

echo "::error::coverage gate: FAIL total=${total}% is below the ${threshold}% threshold profile=$profile" >&2
exit 1
