#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$HOME/.config/systemd/user"
install -m 0644 "$ROOT/deploy/systemd/nexaroute.service" "$HOME/.config/systemd/user/nexaroute.service"
systemctl --user daemon-reload
cat <<'MSG'
Installed the NexaRoute user service definition but did not enable/start it automatically.

After configuring providers:
  systemctl --user enable --now nexaroute

View logs:
  journalctl --user -u nexaroute -f
MSG
