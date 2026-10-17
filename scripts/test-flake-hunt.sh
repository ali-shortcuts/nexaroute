#!/usr/bin/env bash
# Hermetic contract test for scripts/flake-hunt.sh (issue #404).
#
# Regression coverage for the harness counter defect found in round 8:
# start_burners() used the global `i` as its loop variable, clobbering the
# main loop counter on every call. The iteration sequence degenerated to
# 1, 3, 3, 3, ... — iteration 2 was skipped, iteration 3 repeated forever,
# and the FLAKE-HUNT PASS marker was unreachable, so the gate could never
# pass no matter how green the suite was.
#
# This test runs the real harness against stub `go` and `verify.sh`
# binaries that always succeed, with small iteration counts, and asserts
# the harness actually advances through every iteration and prints the
# exact PASS marker with matching counts. Against the defective harness
# the run never terminates (timeout) and the test fails.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HARNESS="$REPO_ROOT/scripts/flake-hunt.sh"
[ -f "$HARNESS" ] || { echo "FAIL: harness not found at $HARNESS"; exit 1; }
command -v timeout >/dev/null 2>&1 || { echo "FAIL: timeout(1) is required"; exit 1; }

SBX="$(mktemp -d)"
trap 'rm -rf "$SBX"' EXIT
mkdir -p "$SBX/scripts" "$SBX/bin" "$SBX/logs"
cp "$HARNESS" "$SBX/scripts/flake-hunt.sh"

# Stub verify.sh: always succeeds.
cat > "$SBX/scripts/verify.sh" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$SBX/scripts/verify.sh"

# Stub go: records the invocation, throttles to 1s (so a defective,
# non-advancing harness cannot spin unboundedly before the timeout),
# prints a fake package result, and always succeeds.
export STUB_GO_LOG="$SBX/go-invocations.log"
cat > "$SBX/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "go $*" >> "$STUB_GO_LOG"
sleep 1
echo "ok  	github.com/ali-shortcuts/nexaroute/stub	0.001s"
exit 0
STUB
chmod +x "$SBX/bin/go"

failures=0
check() { # check <description> <0=pass>
	if [ "$2" -eq 0 ]; then
		echo "ok: $1"
	else
		echo "FAIL: $1"
		failures=$((failures + 1))
	fi
}

OUT="$SBX/harness.out"
PATH="$SBX/bin:$PATH" FLAKE_HUNT_ITERATIONS=3 FLAKE_HUNT_VERIFY_RUNS=2 \
	FLAKE_HUNT_LOGDIR="$SBX/logs" \
	timeout 30 bash "$SBX/scripts/flake-hunt.sh" >"$OUT" 2>&1
rc=$?

[ $rc -eq 0 ]
check "harness terminates with exit 0 (defective harness loops forever on iteration 3 and is killed by timeout, rc=$rc)" $?

grep -q "Phase 1: 3 passed, 0 failed out of 3" "$OUT"
check "phase 1 reports exactly 3/3 iterations passed" $?

grep -q "FLAKE-HUNT PASS: suite_iterations=3/3 verify_runs=2/2" "$OUT"
check "exact PASS marker with matching counts" $?

[ "$(grep -c '^go test' "$STUB_GO_LOG")" -eq 3 ]
check "stub go test invoked exactly once per iteration (3 total)" $?

grep -q -- "-- verify run 2/2 passed" "$OUT"
check "both verify runs executed" $?

if [ $failures -gt 0 ]; then
	echo "test-flake-hunt: $failures assertion(s) FAILED"
	exit 1
fi
echo "test-flake-hunt: PASS"
