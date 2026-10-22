#!/usr/bin/env bash
# Documentation contract for the HA/config boundary.
#
# The deployment report, HA inventory, known gaps and deployment guide must
# keep saying the same thing: config is local and per-instance until a
# separately approved implementation ships. This check is intentionally
# textual and narrow; behavior is covered by the Go regression test
# TestConfigSnapshotReportMatchesLocalStore.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
require() {
  local file=$1 needle=$2
  if ! grep -Fq -- "$needle" "$file"; then
    printf 'missing %s in %s\n' "$needle" "$file" >&2
    fail=1
  fi
}
forbid() {
  local file=$1 needle=$2
  if grep -Fq -- "$needle" "$file"; then
    printf 'stale claim %s still present in %s\n' "$needle" "$file" >&2
    fail=1
  fi
}

require docs/HA_STATE_INVENTORY.md "**per-instance** — the control-plane store never holds config"
require docs/HA_STATE_INVENTORY.md "[ADR 0001](adr/0001-shared-config-snapshot.md)"
require docs/KNOWN_GAPS.md "config edits do not propagate across replicas"
require docs/KNOWN_GAPS.md "[ADR 0001](adr/0001-shared-config-snapshot.md)"
require docs/CONFIGURATION.md "control-plane store never stores"
require docs/DEPLOYMENT.md "They do **not** share configuration"
# ADR 0001 was accepted as Option 1 on 2026-10-22 (local config stays
# authoritative; propagation is not a product requirement). The check
# now pins the settled status instead of the former Proposed marker;
# the contract's purpose is unchanged -- every HA document must keep
# saying config is local and per-instance.
require docs/adr/0001-shared-config-snapshot.md "- Status: **Accepted — Option 1 (local configuration stays authoritative)"
require docs/adr/0001-shared-config-snapshot.md "## Owner decisions required"

forbid docs/DEPLOYMENT.md "until the M4-2"
forbid deploy/helm/nexaroute/values.yaml "until the M4-2"

if [[ "$fail" -ne 0 ]]; then
  echo 'HA documentation consistency check failed' >&2
  exit 1
fi
echo 'HA documentation consistency OK'
