#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"
: "${GH_TOKEN:?GH_TOKEN must be set}"

check_completed() {
  local issue="$1"
  local payload state reason
  if ! payload="$(gh api "repos/${GITHUB_REPOSITORY}/issues/${issue}")"; then
    echo "::error::Could not inspect release blocker #${issue}." >&2
    return 1
  fi
  state="$(jq -r '.state // "unknown"' <<< "$payload")"
  reason="$(jq -r '.state_reason // "none"' <<< "$payload")"
  if [[ "$state" != "closed" || "$reason" != "completed" ]]; then
    echo "::error::Release blocked: issue #${issue} is ${state} (state_reason=${reason}); attach acceptance evidence and close it as completed." >&2
    return 1
  fi
  echo "Release acceptance complete: #${issue}"
}

failed=0
for issue in 646 647 648 649 650 651 652 653; do
  check_completed "$issue" || failed=1
done
# The umbrella is closed last, only after all evidence and the final release
# decision have been reviewed.
check_completed 654 || failed=1

if (( failed != 0 )); then
  echo "::error::Refusing to publish. Resolve every product acceptance blocker and complete issue #654 first." >&2
  exit 1
fi
echo "Final release completion gate passed."
