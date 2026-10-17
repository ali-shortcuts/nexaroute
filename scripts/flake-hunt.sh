#!/usr/bin/env bash
# Flake-hunt harness for the load-flaky test class (issue #404).
#
# The three named tests failed under full-suite load while passing in
# isolation, so a single green run proves nothing. This harness provides
# statistical evidence instead:
#   Phase 1: the full unit test suite runs FLAKE_HUNT_ITERATIONS times
#     (default 50) while all CPUs are saturated by background burners,
#     simulating the loaded scheduler the flakes were observed under.
#     Any single package failure fails the phase and prints the log.
#   Phase 2: scripts/verify.sh (race-enabled) runs FLAKE_HUNT_VERIFY_RUNS
#     times (default 5) consecutively; all must be green.
#
# Raw counts are printed at the end. Exit 0 only if every run passed.
set -euo pipefail
cd "$(dirname "$0")/.."

ITERATIONS="${FLAKE_HUNT_ITERATIONS:-50}"
VERIFY_RUNS="${FLAKE_HUNT_VERIFY_RUNS:-5}"
if ! [[ "$ITERATIONS" =~ ^[1-9][0-9]*$ && "$VERIFY_RUNS" =~ ^[1-9][0-9]*$ ]]; then
	echo "FLAKE_HUNT_ITERATIONS and FLAKE_HUNT_VERIFY_RUNS must be positive integers" >&2
	exit 2
fi

if ! command -v sha256sum >/dev/null 2>&1; then
	echo "sha256sum is required for the CPU burners" >&2
	exit 2
fi

LOGDIR="${FLAKE_HUNT_LOGDIR:-$(pwd)/.flake-hunt-logs}"
mkdir -p "$LOGDIR"

# Per-run TMPDIR isolation (#404, round-2 failure): several tests scan
# os.TempDir() for artifacts they own (e.g. restore journals in
# cmd/gateway), so a suite run that shares the machine-default temp dir
# with ANY other test process — a parallel lane, a second suite, the
# operator's own run — can observe foreign artifacts and fail on them.
# Every iteration and every verify run below gets a fresh, private
# TMPDIR/GOTMPDIR under the log dir, removed on success and kept with
# the failing log on failure.
fresh_tmpdir() {
	local d="$LOGDIR/tmp-$1"
	rm -rf "$d"
	mkdir -p "$d"
	printf '%s' "$d"
}

burners=()
start_burners() {
	# `i` MUST be local: the main loop below uses the global `i` as its
	# iteration counter, and bash function variables are global unless
	# declared local. Without this, every call clobbered the counter
	# (leaving it at nproc) and the harness looped on iteration 3
	# forever — see scripts/test-flake-hunt.sh.
	local n i
	n=$(nproc 2>/dev/null || echo 2)
	for ((i = 0; i < n; i++)); do
		# Saturate one CPU with hashing; output discarded.
		sha256sum < /dev/zero > /dev/null 2>&1 &
		burners+=($!)
	done
}

stop_burners() {
	for pid in "${burners[@]:-}"; do
		kill "$pid" 2>/dev/null || true
	done
	burners=()
	wait 2>/dev/null || true
}
trap stop_burners EXIT

pass=0
fail=0
echo "== Phase 1: $ITERATIONS full-suite iterations under CPU load =="
for ((i = 1; i <= ITERATIONS; i++)); do
	log="$LOGDIR/iter-$(printf '%03d' "$i").log"
	iter_tmp="$(fresh_tmpdir "iter-$(printf '%03d' "$i")")"
	start_burners
	if TMPDIR="$iter_tmp" GOTMPDIR="$iter_tmp" go test -count=1 -timeout=20m ./... >"$log" 2>&1; then
		pass=$((pass + 1))
		rm -f "$log"
		rm -rf "$iter_tmp"
	else
		fail=$((fail + 1))
		echo "ITERATION $i FAILED (log: $log)"
		grep -E "^(--- FAIL|FAIL|panic)" "$log" | head -20
		echo "flake-hunt: ABORTING on first failure"
		exit 1
	fi
	stop_burners
	if ((i % 10 == 0)); then
		echo "-- $i/$ITERATIONS iterations passed"
	fi
done
echo "Phase 1: $pass passed, $fail failed out of $ITERATIONS"

echo "== Phase 2: $VERIFY_RUNS consecutive scripts/verify.sh runs (race-enabled) =="
vpass=0
for ((i = 1; i <= VERIFY_RUNS; i++)); do
	log="$LOGDIR/verify-$(printf '%02d' "$i").log"
	verify_tmp="$(fresh_tmpdir "verify-$(printf '%02d' "$i")")"
	if TMPDIR="$verify_tmp" GOTMPDIR="$verify_tmp" ./scripts/verify.sh >"$log" 2>&1; then
		vpass=$((vpass + 1))
		rm -f "$log"
		rm -rf "$verify_tmp"
		echo "-- verify run $i/$VERIFY_RUNS passed"
	else
		echo "VERIFY RUN $i FAILED (log: $log)"
		grep -E "^(--- FAIL|FAIL|panic)" "$log" | head -20
		echo "flake-hunt: ABORTING on first failure"
		exit 1
	fi
done
echo "Phase 2: $vpass passed out of $VERIFY_RUNS"

echo "FLAKE-HUNT PASS: suite_iterations=$pass/$ITERATIONS verify_runs=$vpass/$VERIFY_RUNS"
