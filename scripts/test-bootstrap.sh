#!/usr/bin/env bash
# Run the real source bootstrap against an isolated Git fixture, including a
# branch that exists only as origin/<name> after clone. No network or home edits.
set -euo pipefail
cd "$(dirname "$0")/.."
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
REVIEW_REPO="$WORK/source"
mkdir -p "$REVIEW_REPO"
git -C "$REVIEW_REPO" init -q -b main
git -C "$REVIEW_REPO" config user.name 'Installer Test'
git -C "$REVIEW_REPO" config user.email 'installer-test@example.invalid'
printf '#!/usr/bin/env bash\nexit 9\n' > "$REVIEW_REPO/install-user.sh"
git -C "$REVIEW_REPO" add .
git -C "$REVIEW_REPO" commit -qm main
git -C "$REVIEW_REPO" switch -qc feature/test
printf '#!/usr/bin/env bash\nprintf success > "$NEXAROUTE_TEST_MARKER"\n' > "$REVIEW_REPO/install-user.sh"
git -C "$REVIEW_REPO" add .
git -C "$REVIEW_REPO" commit -qm feature
REVIEW_SHA="$(git -C "$REVIEW_REPO" rev-parse HEAD)"
git -C "$REVIEW_REPO" tag v0.0.1-test
git -C "$REVIEW_REPO" switch -q main
# Git itself redirects only this exact repository URL to the fixture.
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0="url.file://$REVIEW_REPO.insteadOf"
export GIT_CONFIG_VALUE_0=https://github.com/ali-shortcuts/nexaroute.git
export NEXAROUTE_TEST_MARKER="$WORK/marker"
for ref in feature/test v0.0.1-test "$REVIEW_SHA"; do
  rm -f "$NEXAROUTE_TEST_MARKER"
  bash ./install.sh --source "$ref"
  test "$(cat "$NEXAROUTE_TEST_MARKER")" = success
done
if bash ./install.sh --source missing-ref 2>/dev/null; then
  echo 'Invalid source ref accepted' >&2; exit 1
fi
echo 'BOOTSTRAP PASS: remote branch, tag, commit and invalid ref'
