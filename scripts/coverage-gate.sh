#!/usr/bin/env bash
# Deterministic repository-wide statement-coverage gate for issue #663.
#
# Canonical measurement (see .github/workflows/coverage-report.yml):
#   go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...
# This gate validates the profile with `go tool cover -func` and compares
# the raw covered/total statement counts, not its rounded display percentage.
# No package, file, or statement is excluded, and the denominator
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

# Parse profile counters as arbitrary-precision integers and compare to the
# exact decimal threshold using integer cross-multiplication. Floating point
# rounding (including awk's doubles) must never affect the gate verdict.
if ! counts="$(python3 - "$profile" "$threshold" "$total" <<'PYCODE'
import sys

path, threshold, displayed = sys.argv[1:]
whole, _, fraction = threshold.partition('.')
scale = 10 ** len(fraction)
threshold_scaled = int(whole) * scale + (int(fraction) if fraction else 0)
covered = total = 0
try:
    with open(path, encoding='utf-8') as profile:
        mode = next(profile).strip()
        if mode not in ('mode: set', 'mode: count', 'mode: atomic'):
            raise ValueError('invalid profile mode')
        for number, line in enumerate(profile, 2):
            fields = line.split()
            if len(fields) != 3 or not fields[1].isdigit() or not fields[2].isdigit():
                raise ValueError(f'invalid counters on line {number}')
            statements, executions = int(fields[1]), int(fields[2])
            total += statements
            if executions > 0:
                covered += statements
    if total <= 0:
        raise ValueError('empty coverage denominator')
except (OSError, UnicodeError, ValueError, StopIteration) as err:
    print(f'coverage gate: invalid profile: {err}', file=sys.stderr)
    sys.exit(1)
# go tool cover reports one decimal and ignores profile blocks outside
# functions. Detect inconsistent totals rather than allowing such blocks to
# inflate the gate. This rounded cross-check is not a complete authenticity
# proof for small differences within the display's rounding interval.
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
