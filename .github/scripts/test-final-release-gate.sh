#!/usr/bin/env bash
# Offline regression tests for the final release completion gate.
#
# The gate is the last thing standing between a reviewed tree and a published
# stable release, so every refusal path is pinned here: an open blocker, a
# closed blocker without acceptance, an unreadable API, malformed payloads,
# and every way a version-scoped exclusion manifest can be incomplete, stale,
# widened or duplicated.
set -euo pipefail
cd "$(dirname "$0")/../.."
gate="$PWD/.github/scripts/check-final-release-gate.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/work"
# The gate resolves docs/releases/<version>-exclusions.json relative to the
# working directory, so every invocation runs inside the throwaway sandbox.
work="$tmp/work"

cat > "$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "api" && -n "${2:-}" ]] || exit 90
issue="${2##*/}"
mode="${FAKE_GH_MODE:-pass}"
# FAKE_GH_CLOSED lets a test close a specific issue as not_planned while the
# rest of the blocker set is completed, so exclusion handling can be isolated.
if [[ -n "${FAKE_GH_CLOSED:-}" && "$issue" == "${FAKE_GH_CLOSED}" ]]; then
  printf '{"number":%s,"state":"closed","state_reason":"not_planned"}\n' "$issue"
  exit 0
fi
case "${mode}:${issue}" in
  api-error:650) echo "simulated API failure" >&2; exit 1 ;;
  open:646) printf '{"number":%s,"state":"open","state_reason":null}\n' "$issue" ;;
  not-planned:649) printf '{"number":%s,"state":"closed","state_reason":"not_planned"}\n' "$issue" ;;
  missing-reason:651) printf '{"number":%s,"state":"closed"}\n' "$issue" ;;
  wrong-issue:646) printf '{"number":999,"state":"closed","state_reason":"completed"}\n' ;;
  malformed:646) printf 'not-json{{{\n' ;;
  missing-state:647) printf '{"number":%s,"state_reason":"completed"}\n' "$issue" ;;
  empty-payload:648) printf '' ;;
  null-state:652) printf '{"number":%s,"state":null,"state_reason":null}\n' "$issue" ;;
  *) printf '{"number":%s,"state":"closed","state_reason":"completed"}\n' "$issue" ;;
esac
GH
chmod +x "$tmp/bin/gh"

export GITHUB_REPOSITORY="example/nexaroute"
export GH_TOKEN="stub-token"
export PATH="$tmp/bin:$PATH"
unset GITHUB_REF_NAME || true

failures=0
fails() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

run_gate() {
  (cd "$work" && bash "$gate" "$@")
}

# --- 1. The happy path: every blocker closed as completed. ------------------
if ! run_gate > "$tmp/pass.out" 2>&1; then
  fails "gate rejected the all-completed case"
  cat "$tmp/pass.out" >&2
fi
grep -q "Final release completion gate passed" "$tmp/pass.out" ||
  fails "success output missing"

# --- 2. Every refusal mode of the issue-state reader. ----------------------
for mode in open not-planned api-error wrong-issue missing-reason malformed missing-state empty-payload null-state; do
  if FAKE_GH_MODE="$mode" run_gate > "$tmp/$mode.out" 2>&1; then
    fails "gate accepted failure mode $mode"
    cat "$tmp/$mode.out" >&2
  fi
  grep -q "Refusing to publish" "$tmp/$mode.out" ||
    fails "failure mode $mode did not print the refusal line"
done

# --- 3. Version-scoped exclusion manifest. ---------------------------------
export FAKE_GH_CLOSED=649
mkdir -p "$work/docs/releases"

write_manifest() { # $1 = version file name, stdin = body
  cat > "$work/docs/releases/$1"
}

valid_entry() {
  printf '{"issue":649,"reason":"No operator-owned Vault Transit instance was available; live acceptance was never executed.","approved_by":"owner","approved_at":"2026-11-01"}'
}

# 3a. closed-not-completed with no --version is refused.
if run_gate > "$tmp/nover.out" 2>&1; then
  fails "gate accepted a not_planned closure without a release version"
fi
grep -q "no release version was supplied" "$tmp/nover.out" ||
  fails "missing-version refusal message is wrong"

# 3b. closed-not-completed with a version but no manifest is refused.
if run_gate --version v9.9.9 > "$tmp/nomanifest.out" 2>&1; then
  fails "gate accepted a not_planned closure without an exclusion manifest"
fi
grep -q "no exclusion manifest exists" "$tmp/nomanifest.out" ||
  fails "missing-manifest refusal message is wrong"

# 3c. A complete, owner-approved manifest for the tagged version passes.
{
  printf '{"release":"v9.9.9","exclusions":['
  valid_entry
  printf ']}\n'
} | write_manifest "v9.9.9-exclusions.json"
if ! run_gate --version v9.9.9 > "$tmp/excluded.out" 2>&1; then
  fails "gate rejected a complete exclusion manifest"
  cat "$tmp/excluded.out" >&2
fi
grep -q "WITH DOCUMENTED EXCLUSIONS for #649" "$tmp/excluded.out" ||
  fails "exclusion summary line missing"
grep -q "Final release completion gate passed" "$tmp/excluded.out" ||
  fails "exclusion success output missing"

# 3d. The version may also arrive as the tag ref.
if ! (cd "$work" && GITHUB_REF_NAME=v9.9.9 bash "$gate") > "$tmp/tagref.out" 2>&1; then
  fails "gate rejected a valid manifest resolved from GITHUB_REF_NAME"
fi

# 3e. Every incomplete-manifest shape is refused.
refuse_manifest() { # $1 = label, $2 = manifest body, optional $3 = expected text
  local label="$1" body="$2" expect="${3:-Refusing to publish}"
  printf '%s\n' "$body" > "$work/docs/releases/v9.9.9-exclusions.json"
  if run_gate --version v9.9.9 > "$tmp/manifest-$label.out" 2>&1; then
    fails "gate accepted an invalid exclusion manifest: $label"
  fi
  grep -q "$expect" "$tmp/manifest-$label.out" ||
    fails "manifest case $label printed the wrong message"
}

refuse_manifest "wrong-release" \
  '{"release":"v0.24.0","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":"2026-11-01"}]}' \
  "Exclusions are version-scoped"
refuse_manifest "empty-reason" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"","approved_by":"owner","approved_at":"2026-11-01"}]}'
refuse_manifest "empty-approved-by" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"","approved_at":"2026-11-01"}]}'
refuse_manifest "whitespace-reason" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":" \t","approved_by":"owner","approved_at":"2026-11-01"}]}'
refuse_manifest "whitespace-approved-by" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"   ","approved_at":"2026-11-01"}]}'
refuse_manifest "bad-date" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":"01/11/2026"}]}'
refuse_manifest "impossible-date" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":"2026-02-30"}]}'
refuse_manifest "numeric-date" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":123}]}'
refuse_manifest "numeric-reason" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":123,"approved_by":"owner","approved_at":"2026-11-01"}]}'
refuse_manifest "numeric-approver" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":123,"approved_at":"2026-11-01"}]}'
# The issue identifier must be an integer, not a numeric-looking string.
# Otherwise 649 and "649" compare as distinct in jq's duplicate grouping.
refuse_manifest "string-issue" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":"2026-11-01"},{"issue":"649","reason":"r2","approved_by":"owner","approved_at":"2026-11-01"}]}'
refuse_manifest "not-a-blocker" \
  '{"release":"v9.9.9","exclusions":[{"issue":123,"reason":"r","approved_by":"owner","approved_at":"2026-11-01"}]}' \
  "which is not a release blocker"
refuse_manifest "duplicate" \
  '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":"2026-11-01"},{"issue":649,"reason":"r2","approved_by":"owner","approved_at":"2026-11-01"}]}' \
  "more than once"
refuse_manifest "not-json" '{"release":"v9.9.9","exclusions":[' "is not valid JSON"
refuse_manifest "no-exclusions-array" '{"release":"v9.9.9"}'

# The duplicate-check jq command must itself be fail-closed. Bash suppresses
# errexit inside validate_manifest because the caller invokes it under !.
real_jq="$(command -v jq)"
cat > "$tmp/bin/jq" <<'JQ'
#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "${FAKE_JQ_FAIL_DUPLICATES:-}" == 1 && "$arg" == *'group_by(.)'* ]]; then
    echo "simulated duplicate-check parser failure" >&2
    exit 42
  fi
  if [[ "${FAKE_JQ_FAIL_RELEASE:-}" == 1 && "$arg" == '.release // ""' ]]; then
    echo v9.9.9
    exit 42
  fi
  if [[ "${FAKE_JQ_FAIL_STATE:-}" == 1 && "$arg" == '.state // "unknown"' ]]; then
    echo closed
    exit 42
  fi
  if [[ "${FAKE_JQ_FAIL_REASON:-}" == 1 && "$arg" == '.state_reason // "none"' ]]; then
    echo completed
    exit 42
  fi
done
exec "$REAL_JQ" "$@"
JQ
chmod +x "$tmp/bin/jq"
export REAL_JQ="$real_jq"
printf '%s\n' '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":"2026-11-01"}]}' > "$work/docs/releases/v9.9.9-exclusions.json"
if FAKE_JQ_FAIL_DUPLICATES=1 run_gate --version v9.9.9 > "$tmp/jq-duplicates.out" 2>&1; then
  fails "gate accepted a failed duplicate validation query"
fi
grep -q "could not validate exclusion duplicates" "$tmp/jq-duplicates.out" ||
  fails "duplicate parser failure message missing"

if FAKE_JQ_FAIL_RELEASE=1 run_gate --version v9.9.9 > "$tmp/jq-release.out" 2>&1; then
  fails "gate accepted release query output from a failing parser"
fi
if (unset FAKE_GH_CLOSED; FAKE_JQ_FAIL_STATE=1 run_gate) > "$tmp/jq-state.out" 2>&1; then
  fails "gate accepted issue state output from a failing parser"
fi
if (unset FAKE_GH_CLOSED; FAKE_JQ_FAIL_REASON=1 run_gate) > "$tmp/jq-reason.out" 2>&1; then
  fails "gate accepted issue reason output from a failing parser"
fi

# Valid leap days remain accepted.
printf '%s\n' '{"release":"v9.9.9","exclusions":[{"issue":649,"reason":"r","approved_by":"owner","approved_at":"2024-02-29"}]}' > "$work/docs/releases/v9.9.9-exclusions.json"
if ! run_gate --version v9.9.9 > "$tmp/leap-date.out" 2>&1; then
  fails "gate rejected a valid leap date"
fi

# 3f. A manifest that omits the closed-but-not-completed issue is refused.
printf '{"release":"v9.9.9","exclusions":[]}\n' > "$work/docs/releases/v9.9.9-exclusions.json"
if run_gate --version v9.9.9 > "$tmp/omitted.out" 2>&1; then
  fails "gate accepted a manifest that omits the not_planned issue"
fi
grep -q "does not exclude it" "$tmp/omitted.out" ||
  fails "omitted-exclusion message is wrong"

# 3g. An OPEN blocker is never excludable, even with a perfect manifest.
{
  printf '{"release":"v9.9.9","exclusions":['
  valid_entry
  printf ']}\n'
} | write_manifest "v9.9.9-exclusions.json"
if (cd "$work" && FAKE_GH_MODE=open bash "$gate" --version v9.9.9) > "$tmp/open-excluded.out" 2>&1; then
  fails "gate accepted an OPEN blocker that had a recorded exclusion"
fi
grep -q "An open blocker can never be excluded" "$tmp/open-excluded.out" ||
  fails "open-blocker-with-exclusion message is wrong"

# 3h. A malformed --version is rejected before any API call.
if run_gate --version 9.9.9 > "$tmp/badversion.out" 2>&1; then
  fails "gate accepted a version without the leading v"
fi
grep -q "Invalid release version" "$tmp/badversion.out" ||
  fails "invalid-version message is wrong"

# 3i. A stale or unused manifest must never fail an otherwise clean release:
# when every blocker closed with acceptance evidence the manifest is ignored.
printf '{"release":"v9.9.9","exclusions":[{"issue":649}]}\n' > "$work/docs/releases/v9.9.9-exclusions.json"
unset FAKE_GH_CLOSED
if ! run_gate --version v9.9.9 > "$tmp/unused.out" 2>&1; then
  fails "gate failed a clean release because of an unused exclusion manifest"
  cat "$tmp/unused.out" >&2
fi
grep -q "the manifest is not used" "$tmp/unused.out" ||
  fails "unused-manifest note missing"

# 3j. An OPEN blocker plus a present manifest must fail AND must not print the
# "manifest is not used / every blocker closed with acceptance evidence" note,
# which would contradict the ::error:: lines in the same log.
if (cd "$work" && FAKE_GH_MODE=open bash "$gate" --version v9.9.9) > "$tmp/open-with-manifest.out" 2>&1; then
  fails "gate accepted an OPEN blocker while a manifest was present"
fi
if grep -q "every blocker closed with acceptance evidence" "$tmp/open-with-manifest.out"; then
  fails "open-blocker log claims every blocker closed with acceptance evidence"
fi
grep -q "Refusing to publish" "$tmp/open-with-manifest.out" ||
  fails "open-blocker-with-manifest refusal line missing"

if ((failures != 0)); then
  echo "FINAL RELEASE GATE TEST FAILED (${failures} case(s))" >&2
  exit 1
fi

PATH="${PATH#"$tmp/bin:"}" python3 .github/scripts/test-v025-completion.py
echo "FINAL RELEASE GATE TEST PASS (issue-state refusals, version-scoped manifest validation, unused manifest and open-blocker log cases)"
