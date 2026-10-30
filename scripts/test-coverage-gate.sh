#!/usr/bin/env bash
# Tests for scripts/coverage-gate.sh (issue #663).
#
# The gate is exercised against synthetic coverage profiles generated in a
# temporary module, so no test below depends on the repository's own
# coverage or on any network. Each fixture's source file is generated
# alongside its profile because `go tool cover -func` only counts blocks
# that fall inside a real function body; a block outside any function is
# ignored for the total, which is why the fixture generator writes both.
#
# Cases: exactly at threshold passes (inclusive), below threshold fails,
# above threshold passes, the threshold argument is honored, and a
# missing or malformed profile fails closed. No sleeps, no live calls.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/go/bin:/usr/local/go/bin:$PATH"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
script_abs="$(pwd)/scripts/coverage-gate.sh"

# make_fixture <dir> <statements> <covered>
# Writes dir/go.mod, dir/f.go (one function with <statements> one-line
# statements) and dir/coverage.out (one 1-statement block per line, the
# first <covered> blocks with count 1 and the rest with count 0).
make_fixture() {
  local dir="$1" n="$2" cov="$3" i cnt
  mkdir -p "$dir"
  printf 'module gatefixture\n\ngo 1.27\n' > "$dir/go.mod"
  {
    printf 'package p\n\nfunc A() {\n'
    for ((i = 0; i < n; i++)); do
      printf '\t_ = %s\n' "$i"
    done
    printf '}\n'
  } > "$dir/f.go"
  {
    printf 'mode: count\n'
    for ((i = 0; i < n; i++)); do
      cnt=1
      if ((i >= cov)); then cnt=0; fi
      printf 'gatefixture/f.go:%s.2,%s.8 1 %s\n' "$((3 + i))" "$((3 + i))" "$cnt"
    done
  } > "$dir/coverage.out"
}

# check <dir> <profile-name> <threshold-or-empty> <want-exit> <label>
# An empty threshold argument runs the gate with its default. The gate
# runs with its working directory inside <dir> because `go tool cover`
# resolves the profile's import paths through the current module.
check() {
  local dir="$1" prof="$2" th="$3" want="$4" label="$5" got
  set +e
  if [[ -z "$th" ]]; then
    (cd "$dir" && "$script_abs" "$prof" > "$tmp/last.out" 2>&1)
  else
    (cd "$dir" && "$script_abs" "$prof" "$th" > "$tmp/last.out" 2>&1)
  fi
  got=$?
  set -e
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: $label: want exit $want, got $got" >&2
    cat "$tmp/last.out" >&2
    exit 1
  fi
  echo "ok: $label (exit $got)"
}

make_fixture "$tmp/at" 20 19
check "$tmp/at" "coverage.out" "95.0" 0 "exactly 95.0% passes (inclusive threshold)"

make_fixture "$tmp/below" 1000 949
check "$tmp/below" "coverage.out" "95.0" 1 "94.9% fails below threshold"

make_fixture "$tmp/above" 20 20
check "$tmp/above" "coverage.out" "95.0" 0 "100% passes above threshold"

check "$tmp/below" "coverage.out" "90.0" 0 "threshold argument honored (94.9% passes a 90.0% threshold)"

check "$tmp/at" "coverage.out" "" 0 "default threshold is 95.0%"

mkdir -p "$tmp/missing"
check "$tmp/missing" "does-not-exist.out" "95.0" 1 "missing profile fails closed"

mkdir -p "$tmp/malformed"
printf 'module gatefixture\n\ngo 1.27\n' > "$tmp/malformed/go.mod"
printf 'not a coverage profile\n' > "$tmp/malformed/coverage.out"
check "$tmp/malformed" "coverage.out" "95.0" 1 "malformed profile fails closed"

(cd "$tmp/at" && "$script_abs" "coverage.out" "95.0" > "$tmp/pass.out" 2>&1)
if ! grep -q "PASS total=95.0%" "$tmp/pass.out"; then
  echo "FAIL: passing gate output does not report PASS total=95.0%" >&2
  cat "$tmp/pass.out" >&2
  exit 1
fi
echo "ok: passing gate output reports PASS total=95.0%"
# The "at" fixture has 20 statements, 19 covered: the report must carry
# the exact counts, not just the percentage.
if ! grep -q "covered statements: 19" "$tmp/pass.out"; then
  echo "FAIL: gate report does not carry covered statements: 19" >&2
  cat "$tmp/pass.out" >&2
  exit 1
fi
if ! grep -q "total statements: 20" "$tmp/pass.out"; then
  echo "FAIL: gate report does not carry total statements: 20" >&2
  cat "$tmp/pass.out" >&2
  exit 1
fi
echo "ok: gate report carries covered/total statement counts"

echo "coverage gate tests: PASS"
