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

if FAKE_GH_MODE=api-error bash scripts/release-gate-status.sh "$sha" example/repo > "$tmp/api.out" 2>&1; then
	echo "FAIL: gate passed on API error" >&2; exit 1
fi
grep -q "BLOCKED" "$tmp/api.out" || { cat "$tmp/api.out"; exit 1; }

if bash scripts/release-gate-status.sh "not-a-sha" example/repo > "$tmp/bad.out" 2>&1; then
	echo "FAIL: gate accepted a non-SHA" >&2; exit 1
fi

echo "RELEASE GATE STATUS TEST PASS (pass, one-failed, missing=NOT_RUN, running, api-error=BLOCKED, non-SHA refused)"
