#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$HOME/.config/systemd/user"
install -m 0644 "$ROOT/deploy/systemd/ulg.service" "$HOME/.config/systemd/user/ulg.service"
systemctl --user daemon-reload
cat <<'MSG'
Installed the user service definition but did not enable/start it automatically.
After configuring providers, run:
  systemctl --user enable --now ulg
View logs:
  journalctl --user -u ulg -f
MSG
