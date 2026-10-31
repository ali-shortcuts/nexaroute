#!/usr/bin/env bash
# Regression tests for scripts/release-gate-status.sh, using a fake
# `gh` in PATH (same pattern as test-final-release-gate.sh). No
# network, no real repository, no Go toolchain needed.
set -euo pipefail
cd "$(dirname "$0")/.."

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat > "$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "api" ]] || exit 90
# A real gh would fail if the script passed `--rc` as the repo
# (repos/--rc/...); the fake must fail the same way so the
# `<sha> --rc` invocation is actually covered by the tests.
case "${2:-}" in
  *repos/--rc/*) echo "simulated repo=--rc API failure" >&2; exit 1 ;;
esac
case "${FAKE_GH_MODE:-pass}" in
  api-error) echo "simulated API failure" >&2; exit 1 ;;
  pass) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
print(json.dumps({"total_count": len(names),
  "check_runs": [{"name": n, "status": "completed", "conclusion": "success"} for n in names]}))' ;;
  one-failed) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs[0]["conclusion"] = "failure"
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  missing) python3 -c '
import json
print(json.dumps({"total_count": 1, "check_runs":
  [{"name": "verify", "status": "completed", "conclusion": "success"}]}))' ;;
  running) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs[0]["status"] = "in_progress"; runs[0]["conclusion"] = None
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  latest-pass) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs.insert(0, {"name": "verify", "status": "completed", "conclusion": "failure",
  "started_at": "2026-01-01T00:00:00Z", "id": 1})
runs[1].update({"started_at": "2026-01-02T00:00:00Z", "id": 2})
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  latest-fail) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs.insert(0, {"name": "verify", "status": "completed", "conclusion": "success",
  "started_at": "2026-01-01T00:00:00Z", "id": 1})
runs[1].update({"started_at": "2026-01-02T00:00:00Z", "id": 2, "conclusion": "failure"})
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  latest-running) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs.insert(0, {"name": "verify", "status": "completed", "conclusion": "success",
  "started_at": "2026-01-01T00:00:00Z", "id": 1})
runs[1].update({"started_at": "2026-01-02T00:00:00Z", "id": 2,
  "status": "in_progress", "conclusion": None})
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  latest-oldfail-running) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs.insert(0, {"name": "verify", "status": "completed", "conclusion": "failure",
  "started_at": "2026-01-01T00:00:00Z", "id": 1})
runs[1].update({"started_at": "2026-01-02T00:00:00Z", "id": 2,
  "status": "in_progress", "conclusion": None})
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  latest-tie-fail) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs.insert(0, {"name": "verify", "status": "completed", "conclusion": "success",
  "started_at": "2026-01-01T00:00:00Z", "id": 10})
runs[1].update({"started_at": "2026-01-01T00:00:00Z", "id": 20, "conclusion": "failure"})
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  latest-tie-pass) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs.insert(0, {"name": "verify", "status": "completed", "conclusion": "failure",
  "started_at": "2026-01-01T00:00:00Z", "id": 10})
runs[1].update({"started_at": "2026-01-01T00:00:00Z", "id": 20})
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  latest-no-started) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load"]
runs = [{"name": n, "status": "completed", "conclusion": "success"} for n in names]
runs.insert(0, {"name": "verify", "status": "completed", "conclusion": "failure", "id": 5})
runs[1].update({"id": 9})
print(json.dumps({"total_count": len(runs), "check_runs": runs}))' ;;
  pass-rc) python3 -c '
import json
names = ["verify","fuzz-smoke","ci-soak-smoke","integration-postgres",
 "integration-redis","container","Go vulnerability scan","CodeQL (Go)",
 "Fresh Go coverage","Two real Gateway processes against PostgreSQL",
 "Real Secret Service Store/Load",
 "golangci-lint","staticcheck","gosec","deadcode","soak"]
print(json.dumps({"total_count": len(names),
  "check_runs": [{"name": n, "status": "completed", "conclusion": "success"} for n in names]}))' ;;
  *) exit 90 ;;
esac
GH
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH"
sha="0123456789abcdef0123456789abcdef01234567"

FAKE_GH_MODE=pass bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/pass.out"
grep -q "Release Gate Passed" "$tmp/pass.out" || { cat "$tmp/pass.out"; exit 1; }

for mode in one-failed missing running; do
	if FAKE_GH_MODE="$mode" bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/$mode.out" 2>&1; then
		echo "FAIL: gate passed in mode $mode" >&2; cat "$tmp/$mode.out" >&2; exit 1
	fi
	grep -q "Release Gate NOT passed" "$tmp/$mode.out" || { cat "$tmp/$mode.out"; exit 1; }
done
grep -q "NOT_RUN" "$tmp/missing.out" || { cat "$tmp/missing.out"; exit 1; }
grep -q "RUNNING" "$tmp/running.out" || { cat "$tmp/running.out"; exit 1; }

# Latest-run rule: only the newest run per name decides the row.
FAKE_GH_MODE=latest-pass bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/latest-pass.out" || { cat "$tmp/latest-pass.out"; exit 1; }
grep -q "Release Gate Passed" "$tmp/latest-pass.out" || { cat "$tmp/latest-pass.out"; exit 1; }

for mode in latest-fail latest-tie-fail; do
	if FAKE_GH_MODE="$mode" bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/$mode.out" 2>&1; then
		echo "FAIL: gate passed in mode $mode" >&2; cat "$tmp/$mode.out" >&2; exit 1
	fi
	grep -q "Release Gate NOT passed" "$tmp/$mode.out" || { cat "$tmp/$mode.out"; exit 1; }
	grep -qE "^verify[[:space:]]+FAIL" "$tmp/$mode.out" || { cat "$tmp/$mode.out"; exit 1; }
done

for mode in latest-running latest-oldfail-running; do
	if FAKE_GH_MODE="$mode" bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/$mode.out" 2>&1; then
		echo "FAIL: gate passed in mode $mode" >&2; cat "$tmp/$mode.out" >&2; exit 1
	fi
	grep -q "Release Gate NOT passed" "$tmp/$mode.out" || { cat "$tmp/$mode.out"; exit 1; }
	grep -qE "^verify[[:space:]]+RUNNING" "$tmp/$mode.out" || { cat "$tmp/$mode.out"; exit 1; }
done

for mode in latest-tie-pass latest-no-started; do
	FAKE_GH_MODE="$mode" bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/$mode.out" || { cat "$tmp/$mode.out"; exit 1; }
	grep -q "Release Gate Passed" "$tmp/$mode.out" || { cat "$tmp/$mode.out"; exit 1; }
done

# --rc without a repo argument must not turn `--rc` into the repo.
FAKE_GH_MODE=pass-rc bash scripts/release-gate-status.sh "$sha" --rc > "$tmp/rc-arg.out" || { cat "$tmp/rc-arg.out"; exit 1; }
grep -q "Release Gate Passed" "$tmp/rc-arg.out" || { cat "$tmp/rc-arg.out"; exit 1; }
grep -q "BLOCKED" "$tmp/rc-arg.out" && { cat "$tmp/rc-arg.out"; exit 1; }

if FAKE_GH_MODE=api-error bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/api.out" 2>&1; then
	echo "FAIL: gate passed on API error" >&2; exit 1
fi
grep -q "BLOCKED" "$tmp/api.out" || { cat "$tmp/api.out"; exit 1; }

if bash scripts/release-gate-status.sh "not-a-sha" example/repo > "$tmp/bad.out" 2>&1; then
	echo "FAIL: gate accepted a non-SHA" >&2; exit 1
fi

echo "RELEASE GATE STATUS TEST PASS (pass, one-failed, missing=NOT_RUN, running, api-error=BLOCKED, non-SHA refused, latest-pass, latest-fail, latest-running, latest-oldfail-running, latest-tie-fail, latest-tie-pass, latest-no-started, rc-without-repo)"
