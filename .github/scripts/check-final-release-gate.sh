#!/usr/bin/env bash
# Final release completion gate.
#
# Every product acceptance blocker (#646-#653) and the umbrella (#654) must be
# CLOSED before a stable release may be published. For v0.25.0 only completed
# closures with current provenance metadata are accepted, and no exclusions are
# allowed. Independent review of the evidence remains mandatory. For other
# versions, a non-completed closure may use the generic owner-approved,
# version-scoped exclusion mechanism in docs/releases/<version>-exclusions.json.
#
# This is not a way to hide an open blocker:
#   * an OPEN issue always fails the gate, excluded or not;
#   * a closed-but-not-completed issue fails the gate unless the manifest
#     names that exact issue for that exact version;
#   * every manifest entry must carry a non-empty reason, the approving
#     owner login and a YYYY-MM-DD approval date;
#   * a manifest entry for an issue outside the required set is a hard
#     failure (typo guard), so the manifest can never silently widen;
#   * a manifest whose "release" does not match the tag is a hard failure,
#     so a stale manifest cannot be reused for a later version.
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"
: "${GH_TOKEN:?GH_TOKEN must be set}"

required_issues=(646 647 648 649 650 651 652 653 654)

usage() {
  echo "usage: $0 [--version vX.Y.Z]" >&2
  echo "  --version  release version used to locate docs/releases/<version>-exclusions.json" >&2
  echo "             (defaults to \$GITHUB_REF_NAME when it looks like a tag)" >&2
}

version=""
tag_ref=""
if [[ "${GITHUB_REF:-}" == refs/tags/* ]]; then
  tag_ref="${GITHUB_REF#refs/tags/}"
  if [[ -n "${GITHUB_REF_NAME:-}" && "$GITHUB_REF_NAME" != "$tag_ref" ]]; then
    echo "::error::Tag ref and ref name disagree; refusing to determine release policy." >&2
    exit 2
  fi
  version="$tag_ref"
elif [[ -n "${GITHUB_REF_NAME:-}" && "${GITHUB_REF_NAME}" =~ ^v[0-9] ]]; then
  version="${GITHUB_REF_NAME}"
fi
while (($#)); do
  case "$1" in
    --version)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      version="$2"
      shift 2
      ;;
    --version=*)
      version="${1#*=}"
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

# A caller cannot override the actual v0.25.0 tag into a different policy.
if [[ ( "${GITHUB_REF_NAME:-}" == "v0.25.0" || "$tag_ref" == "v0.25.0" ) && "$version" != "v0.25.0" ]]; then
  echo "::error::Cannot override the v0.25.0 completion-only release policy." >&2
  exit 2
fi

if [[ -n "$version" && ! "$version" =~ ^v[0-9][a-zA-Z0-9._-]*$ ]]; then
  echo "::error::Invalid release version '${version}'; expected vX.Y.Z." >&2
  exit 2
fi

exclusions_file=""
if [[ -n "$version" ]]; then
  exclusions_file="docs/releases/${version}-exclusions.json"
fi

# Owner-directed completion-only policy for v0.25.0. Other releases retain the
# generic #822 exclusion mechanism below. Any v0.25.0 manifest must be a valid
# empty array; signed and unsigned exclusions alike can never waive a blocker.
strict_v025=0
if [[ "$version" == "v0.25.0" ]]; then
  strict_v025=1
  if [[ -f "$exclusions_file" ]] && ! jq -e --arg v "$version" \
    --slurp 'length == 1 and (.[0] | type == "object" and .release == $v and (.exclusions | type == "array" and length == 0))' \
    "$exclusions_file" >/dev/null 2>&1; then
    echo "::error::v0.25.0 permits no exclusions; its manifest must be valid and empty." >&2
    exit 1
  fi
  # jq accepts duplicate keys with last-value-wins semantics. Reject ambiguous
  # JSON even when its final exclusions member appears empty.
  if [[ -f "$exclusions_file" ]] && ! python3 - "$exclusions_file" <<'PYJSON'
import json
import sys

def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result

try:
    with open(sys.argv[1], encoding="utf-8") as file:
        json.load(file, object_pairs_hook=unique)
except (OSError, UnicodeError, ValueError):
    sys.exit(1)
PYJSON
  then
    echo "::error::v0.25.0 manifest is unreadable or has ambiguous JSON." >&2
    exit 1
  fi
  # Release evidence is bound to the candidate at the current main tip, not
  # merely an ancestor or PR merge commit. API/parser errors fail closed.
  if [[ ! "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ]] ||
     ! main_ref="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/main" 2>/dev/null)" ||
     ! main_sha="$(jq -er '.object | select(.type == "commit") | .sha | select(type == "string" and test("^[0-9a-f]{40}$"))' <<<"$main_ref" 2>/dev/null)" ||
     [[ "$main_sha" != "$GITHUB_SHA" ]]; then
    echo "::error::v0.25.0 candidate SHA is not verifiably the current main tip." >&2
    exit 1
  fi
fi

# validate_manifest runs every structural check on the manifest up front and
# fails closed on the first problem. It never mutates state.
validate_manifest() {
  [[ -f "$exclusions_file" ]] || return 0

  if ! jq -e . "$exclusions_file" >/dev/null 2>&1; then
    echo "::error::${exclusions_file} is not valid JSON." >&2
    return 1
  fi

  local declared
  if ! declared="$(jq -r '.release // ""' "$exclusions_file")"; then
    echo "::error::${exclusions_file}: could not read the release version." >&2
    return 1
  fi
  if [[ "$declared" != "$version" ]]; then
    echo "::error::${exclusions_file} declares release '${declared}' but the release being tagged is '${version}'. Exclusions are version-scoped and are never carried forward." >&2
    return 1
  fi

  if ! jq -e '(.exclusions | type) == "array"' "$exclusions_file" >/dev/null; then
    echo "::error::${exclusions_file} must contain an \"exclusions\" array." >&2
    return 1
  fi

  local allowed
  allowed=" $(printf '%s ' "${required_issues[@]}")"

  # Every entry must be complete, reference a required issue, and be unique.
  local problems
  if ! problems="$(jq -r --arg allowed "$allowed" '
    .exclusions
    | to_entries[]
    | .key as $i
    | .value as $e
    | [
        (if ($e | type) != "object" then "entry \($i) is not an object" else empty end),
        (if (($e.issue | type) != "number") or ((($e.issue // "") | tostring | test("^[0-9]+$")) | not) then "entry \($i) has no integer issue" else empty end),
        (if ($allowed | contains(" " + (($e.issue // "") | tostring) + " ")) | not then "entry \($i) names issue #\($e.issue), which is not a release blocker" else empty end),
        (if (($e.reason // "") | type) != "string" or (($e.reason // "") | test("[^[:space:]]") | not) then "entry \($i) has an empty reason" else empty end),
        (if (($e.approved_by // "") | type) != "string" or (($e.approved_by // "") | test("[^[:space:]]") | not) then "entry \($i) has an empty approved_by" else empty end),
        (if (try (($e.approved_at | type) == "string" and ($e.approved_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")) and (($e.approved_at + "T00:00:00Z" | fromdateiso8601 | strftime("%Y-%m-%d")) == $e.approved_at)) catch false) | not then "entry \($i) needs a valid approved_at date as YYYY-MM-DD" else empty end)
      ]
    | .[]
  ' "$exclusions_file")"; then
    echo "::error::${exclusions_file}: could not validate exclusion entries." >&2
    return 1
  fi

  if [[ -n "$problems" ]]; then
    while IFS= read -r line; do
      echo "::error::${exclusions_file}: ${line}" >&2
    done <<< "$problems"
    return 1
  fi

  local duplicates
  if ! duplicates="$(jq -r '[.exclusions[].issue] | group_by(.) | map(select(length > 1) | .[0]) | .[]' "$exclusions_file")"; then
    echo "::error::${exclusions_file}: could not validate exclusion duplicates." >&2
    return 1
  fi
  if [[ -n "$duplicates" ]]; then
    while IFS= read -r issue; do
      echo "::error::${exclusions_file} lists issue #${issue} more than once." >&2
    done <<< "$duplicates"
    return 1
  fi

  return 0
}

# exclusion_reason prints the recorded reason for one issue and fails when the
# issue is not excluded for this version.
exclusion_reason() {
  local issue="$1"
  local reason
  if ! reason="$(jq -r --argjson n "$issue" '.exclusions[] | select(.issue == $n) | .reason' "$exclusions_file" 2>/dev/null)"; then
    return 1
  fi
  [[ -n "$reason" ]] || return 1
  printf '%s\n' "$reason"
}

# PHASE 1 — read every blocker's state. Nothing is decided here; the gate must
# know the whole set (how many blockers need an exclusion) before it can decide
# whether a manifest is required at all. A manifest that exists but is not
# needed must never fail an otherwise clean release.
state_dir="$(mktemp -d)"
trap 'rm -rf "$state_dir"' EXIT

failed=0
latest_scope_closed=""
umbrella_closed=""
needs_exclusion=()
for issue in "${required_issues[@]}"; do
  if ! payload="$(gh api "repos/${GITHUB_REPOSITORY}/issues/${issue}" 2>/dev/null)"; then
    echo "::error::Could not inspect release blocker #${issue}." >&2
    failed=1
    printf '%s\n' "unknown none" > "$state_dir/$issue"
    continue
  fi
  if ! jq -e --argjson expected "$issue" \
    'type == "object" and (.number | type == "number") and .number == $expected' \
    <<<"$payload" >/dev/null 2>&1; then
    echo "::error::Could not verify identity of issue #${issue}." >&2
    failed=1
    continue
  fi
  if ! state="$(jq -r '.state // "unknown"' <<<"$payload" 2>/dev/null)" ||
     ! reason="$(jq -r '.state_reason // "none"' <<<"$payload" 2>/dev/null)"; then
    echo "::error::Could not parse release blocker #${issue}." >&2
    failed=1
    printf '%s\n' "unknown none" > "$state_dir/$issue"
    continue
  fi
  [[ -n "$state" ]] || state="unknown"
  [[ -n "$reason" ]] || reason="none"
  printf '%s %s\n' "$state" "$reason" > "$state_dir/$issue"

  if [[ "$state" != "closed" ]]; then
    if ((strict_v025)); then
      echo "::error::Release blocked: issue #${issue} is ${state} (state_reason=${reason}); v0.25.0 requires completed acceptance and permits no exclusions." >&2
    else
      echo "::error::Release blocked: issue #${issue} is ${state} (state_reason=${reason}). An open blocker can never be excluded; attach acceptance evidence and close it, or close it as not_planned with a reviewed, owner-approved exclusion." >&2
    fi
    failed=1
    continue
  fi
  if [[ "$reason" == "completed" ]]; then
    if ((strict_v025)); then
      if ! closed_at="$(jq -er '
        .closed_at | if type == "string" then
          if (try (fromdateiso8601 | strftime("%Y-%m-%dT%H:%M:%SZ")) catch "") == . then . else error("invalid close date") end
        else error("missing close date") end
      ' <<<"$payload" 2>/dev/null)"; then
        echo "::error::Release blocked: #${issue} has an invalid or missing completed timestamp." >&2
        failed=1
        continue
      fi
      if [[ "$issue" == "654" ]]; then
        umbrella_closed="$closed_at"
      elif [[ -z "$latest_scope_closed" || "$closed_at" > "$latest_scope_closed" ]]; then
        latest_scope_closed="$closed_at"
      fi
    fi
    if ((strict_v025)) && ! python3 "$(dirname "$0")/check-v025-acceptance.py" \
      --repo "$GITHUB_REPOSITORY" --issue "$issue" --version "$version" --sha "${GITHUB_SHA:-}"; then
      echo "::error::Release blocked: #${issue} lacks current v0.25.0 acceptance evidence metadata." >&2
      failed=1
    elif ((strict_v025)); then
      echo "Issue #${issue} is completed and has matching evidence metadata; independent evidence review required."
    else
      echo "Release acceptance complete: #${issue}"
    fi
    continue
  fi
  if ((strict_v025)); then
    echo "::error::Release blocked: #${issue} is not closed as completed; v0.25.0 does not allow exclusions." >&2
    failed=1
    continue
  fi
  needs_exclusion+=("$issue")
done

if ((strict_v025)) && [[ -n "$latest_scope_closed" && -n "$umbrella_closed" ]] && \
   [[ ! "$umbrella_closed" > "$latest_scope_closed" ]]; then
  echo "::error::Release blocked: #654 must be closed after all individual acceptance issues." >&2
  failed=1
fi

# PHASE 2 — resolve documented exclusions, only if a blocker actually needs one.
excluded_issues=()
if ((${#needs_exclusion[@]} > 0)); then
  if [[ -z "$version" ]]; then
    echo "::error::Release blocked: #${needs_exclusion[*]} closed without acceptance evidence and no release version was supplied, so no version-scoped exclusion can be evaluated. Pass --version vX.Y.Z or tag the release." >&2
    failed=1
  elif [[ ! -f "$exclusions_file" ]]; then
    echo "::error::Release blocked: #${needs_exclusion[*]} closed without acceptance evidence and no exclusion manifest exists at ${exclusions_file}." >&2
    failed=1
  elif ! validate_manifest; then
    failed=1
  else
    for issue in "${needs_exclusion[@]}"; do
      reason="$(awk '{print $2}' "$state_dir/$issue")"
      if ! recorded="$(exclusion_reason "$issue")"; then
        echo "::error::Release blocked: issue #${issue} is closed (state_reason=${reason}) but ${exclusions_file} does not exclude it for ${version}." >&2
        failed=1
        continue
      fi
      echo "::warning::Release exclusion for #${issue} recorded in ${exclusions_file}: ${recorded}"
      excluded_issues+=("#${issue}")
    done
  fi
elif ((failed == 0)) && [[ -n "$exclusions_file" && -f "$exclusions_file" ]]; then
  # Only true when nothing failed: with an open blocker (or an unreadable API)
  # the manifest is irrelevant, and claiming "every blocker closed with
  # acceptance evidence" beside the ::error:: lines above would misreport the
  # release state in the one log an operator is most likely to skim.
  if ((strict_v025)); then
    echo "Note: ${exclusions_file} is empty; issue evidence metadata matched. Independent content review remains required."
  else
    echo "Note: ${exclusions_file} exists but every blocker closed with acceptance evidence; the manifest is not used."
  fi
fi

if ((failed != 0)); then
  if ((strict_v025)); then
    echo "::error::Refusing to publish v0.25.0. Complete every acceptance blocker and close #654 last; no exclusions are permitted." >&2
  else
    echo "::error::Refusing to publish. Resolve every product acceptance blocker and complete issue #654 first (or record owner-approved exclusions in ${exclusions_file:-the release notes})." >&2
  fi
  exit 1
fi

if ((${#excluded_issues[@]} > 0)); then
  echo "::warning::Final release completion gate passed WITH DOCUMENTED EXCLUSIONS for ${excluded_issues[*]}. The published release notes must state these scopes as NOT VERIFIED."
fi
echo "Final release completion gate passed."
