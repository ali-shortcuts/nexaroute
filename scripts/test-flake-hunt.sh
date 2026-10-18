#!/usr/bin/env bash
# Hermetic contract test for scripts/flake-hunt.sh (issue #404).
#
# Covers both known harness defects: the original global-loop-counter
# clobbering bug, and Go build scratch being created under LOGDIR inside
# the repo, where verify.sh's recursive gofmt check can see generated .go
# files. The real harness is exercised against stub go/verify binaries.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HARNESS="$REPO_ROOT/scripts/flake-hunt.sh"
[ -f "$HARNESS" ] || { echo "FAIL: harness not found at $HARNESS"; exit 1; }
command -v timeout >/dev/null 2>&1 || { echo "FAIL: timeout(1) is required"; exit 1; }

SBX="$(mktemp -d)"
trap 'rm -rf "$SBX"' EXIT
mkdir -p "$SBX/scripts" "$SBX/bin" "$SBX/logs"
cp "$HARNESS" "$SBX/scripts/flake-hunt.sh"

# Both stubs record their TMPDIR/GOTMPDIR so the test can assert that all
# five invocations use isolated scratch outside the simulated repository.
export STUB_GO_LOG="$SBX/go-invocations.log"
export STUB_TEMP_LOG="$SBX/tempdirs.log"
cat > "$SBX/scripts/verify.sh" <<'STUB'
#!/usr/bin/env bash
printf 'verify|%s|%s\n' "${TMPDIR:-}" "${GOTMPDIR:-}" >> "$STUB_TEMP_LOG"
exit 0
STUB
chmod +x "$SBX/scripts/verify.sh"

# Stub go: records the invocation, throttles to 1s (so a defective,
# non-advancing harness cannot spin unboundedly before the timeout),
# prints a fake package result, and always succeeds.
cat > "$SBX/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "go $*" >> "$STUB_GO_LOG"
printf 'go|%s|%s\n' "${TMPDIR:-}" "${GOTMPDIR:-}" >> "$STUB_TEMP_LOG"
sleep 1
echo "ok  \tgithub.com/ali-shortcuts/nexaroute/stub\t0.001s"
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
	FLAKE_HUNT_LOGDIR="$SBX/logs" FLAKE_HUNT_TMP_ROOT="$(dirname "$SBX")" \
	timeout 30 bash "$SBX/scripts/flake-hunt.sh" >"$OUT" 2>&1
rc=$?

[ "$rc" -eq 0 ]
check "harness terminates with exit 0 (the old loop-counter defect timed out, rc=$rc)" $?

grep -q "Phase 1: 3 passed, 0 failed out of 3" "$OUT"
check "phase 1 reports exactly 3/3 iterations passed" $?

grep -q "FLAKE-HUNT PASS: suite_iterations=3/3 verify_runs=2/2" "$OUT"
check "exact PASS marker with matching counts" $?

[ "$(grep -c '^go test' "$STUB_GO_LOG")" -eq 3 ]
check "stub go test invoked exactly once per iteration (3 total)" $?

grep -q -- "-- verify run 2/2 passed" "$OUT"
check "both verify runs executed" $?

if [ "$(wc -l < "$STUB_TEMP_LOG")" -eq 5 ] && awk -F '|' -v root="$SBX" '
	{
		for (i = 2; i <= 3; i++) {
			if ($i == root || index($i, root "/") == 1) bad = 1
		}
	}
	END { exit bad }
' "$STUB_TEMP_LOG"; then
	temp_paths_ok=0
else
	temp_paths_ok=1
fi
check "TMPDIR and GOTMPDIR for all five runs stay outside the repository" "$temp_paths_ok"

# A misconfigured temp root inside the checkout must fail closed before
# launching any suite, rather than cause a later false verify failure.
BAD_OUT="$SBX/bad-temp-root.out"
PATH="$SBX/bin:$PATH" FLAKE_HUNT_ITERATIONS=1 FLAKE_HUNT_VERIFY_RUNS=1 \
	FLAKE_HUNT_LOGDIR="$SBX/logs" FLAKE_HUNT_TMP_ROOT="$SBX/logs" \
	timeout 5 bash "$SBX/scripts/flake-hunt.sh" >"$BAD_OUT" 2>&1
bad_rc=$?
[ "$bad_rc" -eq 2 ] && grep -q "must be outside repository root" "$BAD_OUT"
check "in-repository temp root is rejected before the gate starts" $?

suite_logs_ok=0
for n in 001 002 003; do
	if [ ! -s "$SBX/logs/iter-$n.log" ]; then suite_logs_ok=1; fi
done
check "raw logs retained for all three successful suite iterations" "$suite_logs_ok"

verify_logs_ok=0
for n in 01 02; do
	if [ ! -s "$SBX/logs/verify-$n.log" ]; then verify_logs_ok=1; fi
done
check "raw logs retained for both successful verify runs" "$verify_logs_ok"

if [ "$failures" -gt 0 ]; then
	echo "test-flake-hunt: $failures assertion(s) FAILED"
	exit 1
fi
echo "test-flake-hunt: PASS (9 assertions)"
