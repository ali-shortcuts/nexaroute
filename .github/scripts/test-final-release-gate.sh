#!/usr/bin/env bash
# Offline regression tests for the strict, completed-only final release gate.
set -euo pipefail
cd "$(dirname "$0")/../.."

gate="$PWD/.github/scripts/check-final-release-gate.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/work"
work="$tmp/work"

cat > "$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
[[ "\${1:-}" == "api" && -n "\${2:-}" ]] || exit 90
issue="\${2##*/}"
mode="\${FAKE_GH_MODE:-pass}"
case "\${mode}:\${issue}" in
  api-error:650)
    echo "simulated GitHub API failure" >&2
    exit 1
    ;;
  open:646)
    printf '{"number":646,"state":"open","state_reason":null}\n'
    ;;
  not-planned:649)
    printf '{"number":649,"state":"closed","state_reason":"not_planned"}\n'
    ;;
  missing-reason:651)
    printf '{"number":651,"state":"closed"}\n'
    ;;
  missing-state:647)
    printf '{"number":647,"state_reason":"completed"}\n'
    ;;
  malformed:646)
    printf 'not-json{{{\n'
    ;;
  null-state:652)
    printf '{"number":652,"state":null,"state_reason":"completed"}\n'
    ;;
  null-reason:653)
    printf '{"number":653,"state":"closed","state_reason":null}\n'
    ;;
  wrong-number:648)
    printf '{"number":999,"state":"closed","state_reason":"completed"}\n'
    ;;
  *)
    printf '{"number":%s,"state":"closed","state_reason":"completed"}\n' "$issue"
    ;;
esac
GH
chmod +x "$tmp/bin/gh"

real_jq="$(command -v jq || true)"
if [[ -z "$real_jq" ]]; then
  echo "jq is required by the release gate tests" >&2
  exit 2
fi

cat > "$tmp/bin/jq" <<'JQ'
#!/usr/bin/env bash
set -euo pipefail
if [[ "\${FAKE_JQ_MODE:-}" == "partial-output-then-fail" ]]; then
  printf 'closed\tcompleted\n'
  exit 42
fi
exec "$REAL_JQ" "$@"
JQ
chmod +x "$tmp/bin/jq"

export REAL_JQ="$real_jq"
export GITHUB_REPOSITORY="example/nexaroute"
export GH_TOKEN="stub-token"
export PATH="$tmp/bin:$PATH"

failures=0
fails() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

run_gate() {
  (cd "$work" && bash "$gate" "$@")
}

# 1. Happy path: every required issue is closed as completed.
if ! run_gate > "$tmp/pass.out" 2>&1; then
  fails "gate rejected the all-completed case"
  cat "$tmp/pass.out" >&2
fi
grep -q "Final release completion gate passed" "$tmp/pass.out" ||
  fails "success output missing"

# 2. Every non-completed or unreadable state must fail closed.
for mode in open not-planned api-error missing-reason missing-state malformed null-state null-reason wrong-number; do
  if FAKE_GH_MODE="$mode" run_gate > "$tmp/$mode.out" 2>&1; then
    fails "gate accepted failure mode $mode"
    cat "$tmp/$mode.out" >&2
  fi
  grep -qE "Final release gate NOT passed|Release blocked|Could not inspect|Could not validate" "$tmp/$mode.out" ||
    fails "failure mode $mode did not explain the refusal"
done

# 3. Partial stdout from a failing jq process must not be accepted.
if FAKE_JQ_MODE=partial-output-then-fail run_gate > "$tmp/jq-failure.out" 2>&1; then
  fails "gate accepted partial jq output from a failing parser"
fi
grep -q "Could not validate GitHub response" "$tmp/jq-failure.out" ||
  fails "jq failure was not reported"

# 4. No version or exclusion override may be supplied.
if run_gate --version v0.25.0 > "$tmp/override.out" 2>&1; then
  fails "gate accepted a version override"
fi
grep -q "no version or exclusion override" "$tmp/override.out" ||
  fails "override refusal message missing"

# 5. A valid-looking owner-approved manifest must never bypass not_planned.
if FAKE_GH_MODE=not-planned run_gate > "$tmp/not-planned-no-manifest.out" 2>&1; then
  fails "gate accepted not_planned without a manifest"
fi
grep -q "only state_reason=completed is accepted" "$tmp/not-planned-no-manifest.out" ||
  fails "not_planned refusal message missing"

# The existence of an exclusion manifest itself is an error, even when every
# issue would otherwise pass. This prevents accidental policy resurrection.
mkdir -p "$work/docs/releases"
cat > "$work/docs/releases/v0.25.0-exclusions.json" <<'JSON'
{
  "release": "v0.25.0",
  "exclusions": [
    {
      "issue": 649,
      "reason": "Owner-approved test fixture; this must not bypass acceptance.",
      "approved_by": "owner",
      "approved_at": "2026-11-01"
    }
  ]
}
JSON
if run_gate > "$tmp/manifest-present.out" 2>&1; then
  fails "gate accepted a release while an exclusion manifest exists"
fi
grep -q "Unsupported release exclusion manifest" "$tmp/manifest-present.out" ||
  fails "manifest rejection message missing"

if ((failures != 0)); then
  echo "FINAL RELEASE GATE TEST FAILED ($failures case(s))" >&2
  exit 1
fi

echo "FINAL RELEASE GATE TEST PASS (all-completed, open, not_planned, API error, malformed payload, missing/null fields, issue identity, jq partial-output failure, version override, exclusion-manifest rejection)"
