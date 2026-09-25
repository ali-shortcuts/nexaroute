#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="v$(sed -n 's/^const Version = "\([^"]*\)"/\1/p' internal/buildinfo/version.go)"
WORK="$(mktemp -d)"
TEST_PID=''
cleanup() { if [[ -n "$TEST_PID" ]]; then kill "$TEST_PID" 2>/dev/null || true; wait "$TEST_PID" 2>/dev/null || true; fi; rm -rf "$WORK"; }
trap cleanup EXIT
(cd dist && sha256sum -c SHA256SUMS)
tar -C "$WORK" -xzf "dist/nexaroute-$VERSION-linux-amd64.tar.gz"
TEST_HOME="$WORK/user home"
mkdir -p "$TEST_HOME"
HOME="$TEST_HOME" bash "$WORK/nexaroute/install-user.sh"
BIN="$TEST_HOME/.local/bin/nexaroute"
CFG="$TEST_HOME/.config/nexaroute/config.json"
test "$("$BIN" -version)" = "NexaRoute $VERSION"
test "$(stat -c '%a' "$CFG")" = 600
# Existing config must survive byte-for-byte; replace a currently running binary.
BEFORE="$(sha256sum "$CFG")"
TEST_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
NEXAROUTE_LISTEN="127.0.0.1:$TEST_PORT" "$BIN" -config "$CFG" >"$WORK/run.log" 2>&1 &
TEST_PID=$!
for _ in $(seq 1 50); do
  if grep -q 'listening' "$WORK/run.log"; then break; fi
  kill -0 "$TEST_PID" || { cat "$WORK/run.log" >&2; exit 1; }
  sleep 0.1
done
kill -0 "$TEST_PID"
HOME="$TEST_HOME" bash "$WORK/nexaroute/install-user.sh"
test "$(sha256sum "$CFG")" = "$BEFORE"
test "$("$BIN" -version)" = "NexaRoute $VERSION"
HOME="$TEST_HOME" bash -c 'source "$HOME/.bashrc"; export PATH="$HOME/.local/bin:$PATH"; command -v nexaroute; nexaroute -version'
for rc in .profile .bashrc .zshrc; do
  test "$(grep -c '^# NexaRoute command path$' "$TEST_HOME/$rc")" = 1
done
echo 'INSTALLER PASS: checksums, package, paths with spaces, config preservation, live binary upgrade'
