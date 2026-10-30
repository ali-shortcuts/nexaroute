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

# Statement counts, recomputed from the same profile (mode line skipped;
# field 2 is the statement count, field 3 the execution count). The
# profile already passed `go tool cover` above, so a counts failure here
# is a gate failure, not a new trust assumption.
counts="$(awk 'NR > 1 { s += $2; if ($3 > 0) c += $2 } END { print c + 0, s + 0 }' "$profile")"
covered_statements="${counts%% *}"
total_statements="${counts##* }"
if [[ ! "$covered_statements" =~ ^[0-9]+$ ]] || [[ ! "$total_statements" =~ ^[0-9]+$ ]] || [[ "$total_statements" -eq 0 ]]; then
  echo "::error::coverage gate: could not recompute statement counts from $profile" >&2
  exit 1
fi

echo "coverage gate report:"
echo "commit: ${COVERAGE_GATE_COMMIT:-unknown}"
echo "go version: ${COVERAGE_GATE_GO_VERSION:-unknown}"
echo "covered statements: ${covered_statements}"
echo "total statements: ${total_statements}"
echo "coverage: ${total}%"
echo "threshold: ${threshold}%"

if awk -v t="$total" -v th="$threshold" 'BEGIN { exit !(t + 0 >= th + 0) }'; then
  echo "coverage gate: PASS total=${total}% (threshold >= ${threshold}%) profile=$profile"
  exit 0
fi

echo "::error::coverage gate: FAIL total=${total}% is below the ${threshold}% threshold profile=$profile" >&2
exit 1
