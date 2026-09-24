#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$HOME/.config/systemd/user"
install -m 0644 "$ROOT/deploy/systemd/nexaroute.service" "$HOME/.config/systemd/user/nexaroute.service"

# The unit file is what this script installs; reloading the user manager is a
# convenience. Headless sessions, containers and WSL have no user systemd bus,
# and failing here would abort before the operator sees the enable/log
# instructions even though the service definition was installed correctly.
if ! systemctl --user daemon-reload 2>/dev/null; then
  printf '%s\n' \
    "warning: could not run 'systemctl --user daemon-reload' (no user systemd bus in this session)." \
    "         Run that command once inside a logged-in user session, then enable the service." >&2
fi
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
