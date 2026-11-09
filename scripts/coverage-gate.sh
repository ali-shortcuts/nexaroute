#!/usr/bin/env bash
# Deterministic repository-wide statement-coverage gate for issue #663.
#
# Canonical measurement (see .github/workflows/coverage-report.yml):
#   go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...
# This gate validates the profile with `go tool cover -func` and compares
# exact covered/total statement counts, not its rounded display percentage.
# No package or Go source file is excluded: the denominator is every
# instrumented statement block inside a declared function, matching Go's
# standard tool. Blocks outside functions are ignored by `go tool cover` and
# by this gate; excluding a package/file, lowering the threshold, or otherwise
# changing the denominator to make the gate pass is not acceptable. The
# threshold is inclusive: a total exactly equal to it passes.
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
# Optional environment (reported verbatim, never used for the verdict):
#   COVERAGE_GATE_COMMIT     tested commit SHA (workflow passes GITHUB_SHA)
#   COVERAGE_GATE_GO_VERSION Go toolchain version (workflow passes go version)
# The report prints commit, Go version, covered statements, total
# statements, percentage, and threshold, so a PASS/FAIL verdict is always
# attributable to an exact measurement.
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

# Count only coverage blocks that overlap declared function extents, matching
# `go tool cover -func`; then compare to the exact decimal threshold using
# integer cross-multiplication. Floating point rounding must never affect the
# gate verdict, and blocks outside functions cannot inflate it.
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
count_helper="$script_dir/../.github/scripts/coverage-profile-counts.go"
if ! statement_counts="$(go run "$count_helper" "$profile" 2>&1)"; then
  echo "::error::coverage gate: could not count function statements in $profile" >&2
  printf '%s\n' "$statement_counts" >&2
  exit 1
fi
read -r covered_statements total_statements <<< "$statement_counts"
if ! counts="$(python3 - "$threshold" "$total" "$covered_statements" "$total_statements" <<'PYCODE'
import sys

threshold, displayed, covered_raw, total_raw = sys.argv[1:]
if not covered_raw.isdigit() or not total_raw.isdigit():
    print('coverage gate: invalid function statement counts', file=sys.stderr)
    sys.exit(1)
covered, total = int(covered_raw), int(total_raw)
if total <= 0 or covered < 0 or covered > total:
    print('coverage gate: invalid function statement denominator or numerator', file=sys.stderr)
    sys.exit(1)
whole, _, fraction = threshold.partition('.')
scale = 10 ** len(fraction)
threshold_scaled = int(whole) * scale + (int(fraction) if fraction else 0)
# go tool cover reports one decimal. This cross-check ensures the AST-based
# statement counts agree with its displayed total within that rounding range.
dwhole, _, dfrac = displayed.partition('.')
dscale = 10 ** len(dfrac)
display_scaled = int(dwhole) * dscale + (int(dfrac) if dfrac else 0)
if abs(covered * 100 * dscale - total * display_scaled) * 20 > total * dscale:
    print('coverage gate: profile statement counts disagree with go tool cover total', file=sys.stderr)
    sys.exit(1)
passed = covered * 100 * scale >= total * threshold_scaled
print(covered, total, int(passed))
PYCODE
)"; then
  echo "::error::coverage gate: could not recompute statement counts from $profile" >&2
  exit 1
fi
read -r covered_statements total_statements passes <<< "$counts"

echo "coverage gate report:"
echo "commit: ${COVERAGE_GATE_COMMIT:-unknown}"
echo "go version: ${COVERAGE_GATE_GO_VERSION:-unknown}"
echo "covered statements: ${covered_statements}"
echo "total statements: ${total_statements}"
echo "coverage: ${total}% (go tool cover rounded display)"
awk -v c="$covered_statements" -v n="$total_statements" 'BEGIN { printf "coverage from counts: %.8f%%\n", 100 * c / n }'
echo "threshold: ${threshold}%"

# The exact integer verdict above is independent of the rounded display.
if [[ "$passes" == 1 ]]; then
  echo "coverage gate: PASS total=${total}% (threshold >= ${threshold}%) profile=$profile"
  exit 0
fi

echo "::error::coverage gate: FAIL covered=${covered_statements}/${total_statements} is below the ${threshold}% threshold (rounded display ${total}%) profile=$profile" >&2
exit 1
