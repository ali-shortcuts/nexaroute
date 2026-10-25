#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat > "$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "api" && -n "${2:-}" ]] || exit 90
issue="${2##*/}"
case "${FAKE_GH_MODE:-pass}:${issue}" in
  api-error:650) echo "simulated API failure" >&2; exit 1 ;;
  open:646) printf '{"state":"open","state_reason":null}\n' ;;
  not-planned:649) printf '{"state":"closed","state_reason":"not_planned"}\n' ;;
  missing-reason:651) printf '{"state":"closed"}\n' ;;
  *) printf '{"state":"closed","state_reason":"completed"}\n' ;;
esac
GH
chmod +x "$tmp/bin/gh"
export GITHUB_REPOSITORY="example/nexaroute"
export GH_TOKEN="stub-token"
export PATH="$tmp/bin:$PATH"

FAKE_GH_MODE=pass bash .github/scripts/check-final-release-gate.sh > "$tmp/pass.out"
grep -q "Final release completion gate passed" "$tmp/pass.out"

for mode in open not-planned api-error missing-reason; do
  if FAKE_GH_MODE="$mode" bash .github/scripts/check-final-release-gate.sh > "$tmp/$mode.out" 2>&1; then
    echo "FAIL: gate accepted failure mode $mode" >&2
    cat "$tmp/$mode.out" >&2
    exit 1
  fi
  grep -q "Refusing to publish" "$tmp/$mode.out"
done

echo "FINAL RELEASE GATE TEST PASS (success plus open, not_planned, API error, and missing-reason failures)"
