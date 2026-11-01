#!/usr/bin/env bash
# Final release completion gate.
#
# Every required acceptance issue (#646-#654) must be genuinely completed.
# The only accepted GitHub state is:
#   state=closed AND state_reason=completed
#
# There is deliberately no exclusion, waiver, or not_planned path.
set -euo pipefail

: "\${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"
: "\${GH_TOKEN:?GH_TOKEN must be set}"

required_issues=(646 647 648 649 650 651 652 653 654)

if (($# != 0)); then
  echo "usage: $0" >&2
  echo "The completion gate accepts no version or exclusion override." >&2
  exit 2
fi

# Exclusion manifests are no longer a supported release mechanism. Reject any
# such file explicitly so a copied or stale manifest cannot restore the old
# policy.
shopt -s nullglob
exclusion_manifests=(docs/releases/*-exclusions.json)
shopt -u nullglob
if (("\${#exclusion_manifests[@]}" > 0)); then
  for manifest in "\${exclusion_manifests[@]}"; do
    echo "::error::Unsupported release exclusion manifest: \${manifest}. All mandatory issues must be completed; exclusions are not accepted." >&2
  done
  exit 1
fi

failed=0
for issue in "\${required_issues[@]}"; do
  if ! payload="$(gh api "repos/\${GITHUB_REPOSITORY}/issues/\${issue}" 2>/dev/null)"; then
    echo "::error::Could not inspect required release issue #\${issue}; refusing to publish." >&2
    failed=1
    continue
  fi

  # Validate the response shape and issue identity before trusting its state.
  # Capture both fields in one jq process and check jq's exit status explicitly;
  # partial stdout from a failed parser must never be treated as valid evidence.
  if ! state_record="$(jq -er --argjson expected "\${issue}" '
    if type != "object" then error("issue response is not an object")
    elif ((.number | type) != "number") or (.number != $expected) then error("issue number mismatch")
    elif ((.state | type) != "string") then error("missing or invalid state")
    elif ((.state_reason | type) != "string") then error("missing or invalid state_reason")
    else [.state, .state_reason] | @tsv
    end
  ' <<<"\${payload}" 2>/dev/null)"; then
    echo "::error::Could not validate GitHub response for issue #\${issue}; refusing to publish." >&2
    failed=1
    continue
  fi

  IFS=$'\t' read -r issue_state issue_reason <<<"\${state_record}"

  if [[ "\${issue_state}" != "closed" ]]; then
    echo "::error::Release blocked: issue #\${issue} is \${issue_state}; every required issue must be closed as completed." >&2
    failed=1
    continue
  fi

  if [[ "\${issue_reason}" != "completed" ]]; then
    echo "::error::Release blocked: issue #\${issue} has state_reason=\${issue_reason}; only state_reason=completed is accepted. not_planned and all other reasons are rejected." >&2
    failed=1
    continue
  fi

  echo "Release acceptance complete: #\${issue}"
done

if ((failed != 0)); then
  echo "::error::Final release gate NOT passed. Complete and close every required issue #646-#654 with state_reason=completed, attach real acceptance evidence, and rerun this gate." >&2
  exit 1
fi

echo "Final release completion gate passed: all required issues #646-#654 are closed as completed."
