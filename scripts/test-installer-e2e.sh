#!/usr/bin/env bash
# E2E test verifying installer asset verification, clean install, upgrade, and config isolation.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

case "$(uname -m)" in
  x86_64|amd64) ASSET_NAME="nexaroute-linux-amd64" ;;
  aarch64|arm64) ASSET_NAME="nexaroute-linux-arm64" ;;
  *) echo "Unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

RELEASE_DIR="$WORK/release-v1"
mkdir -p "$RELEASE_DIR"

# 1. Create mock release binary for v0.6.0
cat <<'MOCK' > "$RELEASE_DIR/$ASSET_NAME"
#!/usr/bin/env bash
if [[ "${1:-}" == "-version" || "${1:-}" == "--version" ]]; then
  echo "NexaRoute v0.6.0"
  exit 0
fi
echo "NexaRoute v0.6.0 running"
exit 0
MOCK
chmod 0755 "$RELEASE_DIR/$ASSET_NAME"
(cd "$RELEASE_DIR" && sha256sum "$ASSET_NAME" > SHA256SUMS)

INSTALL_BIN_DIR="$WORK/install-bin"
CONFIG_HOME="$WORK/config-home"
export NEXAROUTE_INSTALL_DIR="$INSTALL_BIN_DIR"
export XDG_CONFIG_HOME="$CONFIG_HOME"
export NEXAROUTE_LOCAL_RELEASE_DIR="$RELEASE_DIR"
export NEXAROUTE_LOCAL_RELEASE_TAG="v0.6.0"

# 2. Run clean install
echo "== testing clean install =="
bash "$ROOT/scripts/install.sh"

[[ -x "$INSTALL_BIN_DIR/nexaroute" ]] || { echo "installer did not create executable binary" >&2; exit 1; }
[[ -d "$CONFIG_HOME/nexaroute" ]] || { echo "installer did not create config directory" >&2; exit 1; }

BIN_MODE="$(stat -c '%a' "$INSTALL_BIN_DIR/nexaroute")"
[[ "$BIN_MODE" == "755" ]] || { echo "binary mode is $BIN_MODE; want 755" >&2; exit 1; }

CFG_DIR_MODE="$(stat -c '%a' "$CONFIG_HOME/nexaroute")"
[[ "$CFG_DIR_MODE" == "700" ]] || { echo "config dir mode is $CFG_DIR_MODE; want 700" >&2; exit 1; }

VERSION_OUT="$("$INSTALL_BIN_DIR/nexaroute" -version)"
[[ "$VERSION_OUT" == *"v0.6.0"* ]] || { echo "version output mismatch: $VERSION_OUT" >&2; exit 1; }

# 3. Test checksum verification rejection
echo "== testing checksum mismatch rejection =="
CORRUPT_DIR="$WORK/release-corrupt"
mkdir -p "$CORRUPT_DIR"
cp "$RELEASE_DIR/$ASSET_NAME" "$CORRUPT_DIR/$ASSET_NAME"
echo "corrupted payload" >> "$CORRUPT_DIR/$ASSET_NAME"
# Use original SHA256SUMS so checksum fails
cp "$RELEASE_DIR/SHA256SUMS" "$CORRUPT_DIR/SHA256SUMS"

NEXAROUTE_LOCAL_RELEASE_DIR="$CORRUPT_DIR" bash "$ROOT/scripts/install.sh" 2>/dev/null && {
  echo "installer succeeded despite corrupted checksum" >&2; exit 1;
} || true

# Assert existing binary was not modified by the failed attempt
VERSION_CHECK="$("$INSTALL_BIN_DIR/nexaroute" -version)"
[[ "$VERSION_CHECK" == *"v0.6.0"* ]] || { echo "binary was corrupted by failed install" >&2; exit 1; }

# 4. Test upgrade without config clobber
echo "== testing upgrade flow and config preservation =="
# Create existing config with custom contents and 0600 mode
CFG_FILE="$CONFIG_HOME/nexaroute/config.json"
echo '{"listen":"127.0.0.1:9999","custom":"preserve-canary"}' > "$CFG_FILE"
chmod 0600 "$CFG_FILE"

RELEASE_DIR_V2="$WORK/release-v2"
mkdir -p "$RELEASE_DIR_V2"
cat <<'MOCK2' > "$RELEASE_DIR_V2/$ASSET_NAME"
#!/usr/bin/env bash
if [[ "${1:-}" == "-version" || "${1:-}" == "--version" ]]; then
  echo "NexaRoute v0.6.1"
  exit 0
fi
echo "NexaRoute v0.6.1 running"
exit 0
MOCK2
chmod 0755 "$RELEASE_DIR_V2/$ASSET_NAME"
(cd "$RELEASE_DIR_V2" && sha256sum "$ASSET_NAME" > SHA256SUMS)

export NEXAROUTE_LOCAL_RELEASE_DIR="$RELEASE_DIR_V2"
export NEXAROUTE_LOCAL_RELEASE_TAG="v0.6.1"

bash "$ROOT/scripts/install.sh"

NEW_VERSION_OUT="$("$INSTALL_BIN_DIR/nexaroute" -version)"
[[ "$NEW_VERSION_OUT" == *"v0.6.1"* ]] || { echo "upgrade did not update version: $NEW_VERSION_OUT" >&2; exit 1; }

# Config must remain intact with 0600 mode
CFG_CONTENT="$(cat "$CFG_FILE")"
[[ "$CFG_CONTENT" == *"preserve-canary"* ]] || { echo "upgrade overwritten user config" >&2; exit 1; }

CFG_MODE="$(stat -c '%a' "$CFG_FILE")"
[[ "$CFG_MODE" == "600" ]] || { echo "config mode changed to $CFG_MODE; want 600" >&2; exit 1; }

echo "INSTALLER E2E PASS"
