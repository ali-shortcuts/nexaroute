#!/usr/bin/env bash
# Generates the release SBOM (SPDX JSON) with a pinned, checksum-
# verified Syft, describing the actual distributable assets: Syft
# catalogs the Go module inventory embedded in the shipped binaries
# plus the shipped files themselves. Traceability metadata (source
# repository, commit and release version) is recorded in the SPDX
# document namespace and comment.
#
# Usage: scripts/generate-sbom.sh <version> [dist-dir]
set -euo pipefail
cd "$(dirname "$0")/.."

SYFT_VERSION="1.54.1"
# Official checksums file: syft_1.54.1_checksums.txt (anchore/syft v1.54.1).
SYFT_TARBALL="syft_${SYFT_VERSION}_linux_amd64.tar.gz"
SYFT_SHA256="c069905b391cc4c20a5ba65ad5c10be2a7ba074f8ea6ad203e24d14e303dad47"

version="${1:?usage: scripts/generate-sbom.sh <version> [dist-dir]}"
dist="${2:-dist}"
[[ "$version" =~ ^v[0-9][a-zA-Z0-9._-]*$ ]] || { echo "generate-sbom: invalid version: $version" >&2; exit 1; }
[[ -d "$dist" ]] || { echo "generate-sbom: no such directory: $dist" >&2; exit 1; }
[[ "$(uname -s)-$(uname -m)" == "Linux-x86_64" ]] || {
  echo "generate-sbom: the pinned Syft checksum covers linux/amd64 only (found $(uname -s)-$(uname -m))" >&2
  exit 1
}

cache="${SYFT_CACHE_DIR:-$HOME/.cache/nexaroute-syft}/$SYFT_VERSION"
mkdir -p "$cache"
tarball="$cache/$SYFT_TARBALL"
# SYFT_DOWNLOAD_BASE exists for offline regression tests; production
# always uses the official release location.
download_base="${SYFT_DOWNLOAD_BASE:-https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}}"
tarball_ok() {
  [[ -f "$tarball" ]] &&
    (cd "$cache" && echo "$SYFT_SHA256  $SYFT_TARBALL" | sha256sum -c - >/dev/null 2>&1)
}
# Trust chain: the cached tarball is re-verified against the pinned
# SHA-256 on EVERY invocation (a corrupt one is re-downloaded and
# re-verified, and the script fails closed if it still does not
# verify), and the Syft binary is always extracted fresh from the
# verified tarball. A cached executable is never trusted merely for
# existing — a tampered one is simply never executed.
if ! tarball_ok; then
  echo "generate-sbom: downloading Syft $SYFT_VERSION (pinned, checksum-verified)"
  rm -f "$tarball"
  curl -fsSL "$download_base/$SYFT_TARBALL" -o "$tarball" || {
    echo "generate-sbom: Syft download failed" >&2
    exit 1
  }
  tarball_ok || {
    echo "generate-sbom: Syft tarball checksum mismatch; refusing to use it" >&2
    exit 1
  }
fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/syft"
tar --no-same-owner -xzf "$tarball" -C "$work/syft" syft
syft="$work/syft/syft"
echo "generate-sbom: using $("$syft" version | head -1)"

commit="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
out="$dist/nexaroute-${version}-sbom.spdx.json"

# Scan a staging copy holding exactly the distributable assets, so the
# SBOM never describes the checksum manifest (rewritten afterwards)
# or a stale SBOM.
stage="$work/stage"
mkdir -p "$stage"
for f in "$dist"/*; do
  case "$(basename "$f")" in
    SHA256SUMS|*-sbom.spdx.json|SHA256SUMS.cosign.bundle) ;;
    *) cp "$f" "$stage/" ;;
  esac
done
"$syft" scan "dir:$stage" \
  --source-name "nexaroute-${version}" \
  --source-version "$version" \
  --output "spdx-json=$out" >/dev/null

# Record repository + commit traceability in the SPDX document.
python3 - "$out" "$version" "$commit" <<'PYEOF'
import json, sys
path, version, commit = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path) as fh:
    doc = json.load(fh)
doc["documentNamespace"] = (
    f"https://github.com/ali-shortcuts/nexaroute/releases/{version}/sbom-{commit}"
)
doc["comment"] = (
    f"SBOM for NexaRoute {version}; source repository "
    f"github.com/ali-shortcuts/nexaroute at commit {commit}; "
    f"describes the shipped binaries and files of this release."
)
with open(path, "w") as fh:
    json.dump(doc, fh, indent=2)
    fh.write("\n")
PYEOF
echo "generate-sbom: wrote $out (commit $commit)"
