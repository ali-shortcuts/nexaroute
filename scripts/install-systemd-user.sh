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

Detailed bounded application log:
  tail -F "$HOME/.config/nexaroute/nexaroute.log"

Rate-limited service console:
  journalctl --user -u nexaroute -f

The default application log automatically rotates at 32 MB and keeps three backups.
MSG
