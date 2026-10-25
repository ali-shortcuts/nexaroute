#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]]; then
  echo "DBUS_SESSION_BUS_ADDRESS must be set by dbus-run-session" >&2
  exit 1
fi

# The CI keyring has no personal data; unlock its disposable Secret Service.
eval "$(printf '\n' | gnome-keyring-daemon --unlock --components=secrets)"
go test -tags integration ./internal/secrets/ -run '^TestRealLinuxKeyringStoreRoundTrips$' -count=1 -v
