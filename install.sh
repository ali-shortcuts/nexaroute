#!/usr/bin/env bash
# Download a checksum-verified release, or explicitly build a source ref.
set -euo pipefail
REPO='ali-shortcuts/nexaroute'
VERSION="${NEXAROUTE_VERSION:-v0.6.1-beta.1}"
SOURCE_REF=''
case "${1:-}" in
  --source) SOURCE_REF="${2:-main}" ;;
  '') ;;
  *) echo 'Usage: install.sh [--source git-ref]' >&2; exit 1 ;;
esac
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
if [[ -n "$SOURCE_REF" ]]; then
  for dep in git go; do command -v "$dep" >/dev/null || { echo "Missing: $dep (Go 1.23+ required)" >&2; exit 1; }; done
  git clone --quiet "https://github.com/$REPO.git" "$WORK/nexaroute"
  git -C "$WORK/nexaroute" checkout --quiet --detach "$SOURCE_REF"
  bash "$WORK/nexaroute/install-user.sh"
  exit 0
fi
case "$(uname -s)" in Linux) TARGET_OS=linux ;; Darwin) TARGET_OS=darwin ;; *) echo 'Use Linux, macOS, or WSL.' >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) TARGET_ARCH=amd64 ;; aarch64|arm64) TARGET_ARCH=arm64 ;; *) echo 'Unsupported architecture.' >&2; exit 1 ;; esac
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9.-]+)?$ ]] || { echo 'Invalid release version.' >&2; exit 1; }
ASSET="nexaroute-$VERSION-$TARGET_OS-$TARGET_ARCH.tar.gz"
URL="https://github.com/$REPO/releases/download/$VERSION"
for dep in curl tar; do command -v "$dep" >/dev/null || { echo "Missing: $dep" >&2; exit 1; }; done
curl --fail --location --retry 3 --connect-timeout 15 --max-time 300 "$URL/$ASSET" -o "$WORK/$ASSET"
curl --fail --location --retry 3 --connect-timeout 15 --max-time 60 "$URL/SHA256SUMS" -o "$WORK/SHA256SUMS"
EXPECTED="$(awk -v asset="$ASSET" '$2 == asset {print $1}' "$WORK/SHA256SUMS")"
[[ "$EXPECTED" =~ ^[a-fA-F0-9]{64}$ ]] || { echo 'Missing or invalid release checksum.' >&2; exit 1; }
if command -v sha256sum >/dev/null; then
  ACTUAL="$(sha256sum "$WORK/$ASSET")"; ACTUAL="${ACTUAL%% *}"
elif command -v shasum >/dev/null; then
  ACTUAL="$(shasum -a 256 "$WORK/$ASSET")"; ACTUAL="${ACTUAL%% *}"
else echo 'Install sha256sum or shasum.' >&2; exit 1
fi
[[ "$ACTUAL" == "$EXPECTED" ]] || { echo 'Checksum mismatch; installation stopped.' >&2; exit 1; }
tar -xzf "$WORK/$ASSET" -C "$WORK"
bash "$WORK/nexaroute/install-user.sh"
