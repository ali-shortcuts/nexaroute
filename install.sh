#!/usr/bin/env bash
set -euo pipefail

REPOSITORY="ali-shortcuts/nexaroute"
RELEASES_URL="${NEXAROUTE_RELEASES_URL:-https://github.com/${REPOSITORY}/releases}"
VERSION="${NEXAROUTE_VERSION:-latest}"

for dependency in curl tar awk tr; do
  command -v "$dependency" >/dev/null 2>&1 || {
    echo "Missing required command: $dependency" >&2
    exit 1
  }
done

if [[ "$VERSION" == "latest" ]]; then
  DOWNLOAD_URL="${RELEASES_URL%/}/latest/download"
else
  if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]]; then
    echo "Invalid NEXAROUTE_VERSION: $VERSION (expected a version such as v0.6.0)" >&2
    exit 1
  fi
  DOWNLOAD_URL="${RELEASES_URL%/}/download/${VERSION}"
fi

TMP_DIR="$(mktemp -d)"
cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT

ARCHIVE="nexaroute-universal.tar.gz"
CHECKSUMS="SHA256SUMS"
curl --fail --silent --show-error --location "${DOWNLOAD_URL}/${ARCHIVE}" -o "${TMP_DIR}/${ARCHIVE}" || {
  echo "Could not download a NexaRoute release from ${DOWNLOAD_URL}." >&2
  echo "No published release may exist yet; see https://github.com/${REPOSITORY}/releases." >&2
  exit 1
}
curl --fail --silent --show-error --location "${DOWNLOAD_URL}/${CHECKSUMS}" -o "${TMP_DIR}/${CHECKSUMS}"

EXPECTED="$(awk -v name="$ARCHIVE" '$2 == name { print $1 }' "${TMP_DIR}/${CHECKSUMS}")"
if [[ ! "$EXPECTED" =~ ^[[:xdigit:]]{64}$ ]]; then
  echo "No valid SHA-256 checksum for ${ARCHIVE} was found in ${CHECKSUMS}." >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMP_DIR}/${ARCHIVE}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMP_DIR}/${ARCHIVE}" | awk '{print $1}')"
else
  echo "Missing SHA-256 utility (install coreutils or shasum)." >&2
  exit 1
fi
ACTUAL_LOWER="$(printf '%s' "$ACTUAL" | tr '[:upper:]' '[:lower:]')"
EXPECTED_LOWER="$(printf '%s' "$EXPECTED" | tr '[:upper:]' '[:lower:]')"
if [[ "$ACTUAL_LOWER" != "$EXPECTED_LOWER" ]]; then
  echo "Checksum verification failed for ${ARCHIVE}." >&2
  exit 1
fi

tar -xzf "${TMP_DIR}/${ARCHIVE}" -C "$TMP_DIR"
PACKAGE="${TMP_DIR}/nexaroute"
if [[ ! -x "${PACKAGE}/install-user.sh" ]]; then
  echo "The release archive does not contain an executable install-user.sh." >&2
  exit 1
fi
"${PACKAGE}/install-user.sh"
