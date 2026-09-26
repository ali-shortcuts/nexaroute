#!/usr/bin/env bash
# Release-only Ubuntu installer. Safe to run via curl | bash; never compiles source.
set -euo pipefail

# Do not run a partially downloaded script.
main() {
  umask 077
  fail() { printf 'NexaRoute install: %s\n' "$*" >&2; exit 1; }
  trap 'echo "NexaRoute installation failed; existing config is untouched." >&2' ERR
  [[ "$(uname -s)" == Linux ]] || fail 'Only Linux is supported.'
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail "Unsupported architecture: $(uname -m) (use amd64 or arm64)." ;;
  esac
  for dep in curl sha256sum awk mktemp install mv; do
    command -v "$dep" >/dev/null || fail "Missing $dep. On Ubuntu install curl and coreutils."
  done
  version="${NEXAROUTE_VERSION:-latest}"
  repo=https://github.com/ali-shortcuts/nexaroute/releases
  if [[ "$version" == latest ]]; then
    base="$repo/latest/download"
  else
    [[ "$version" =~ ^v[0-9][a-zA-Z0-9._-]*$ ]] || fail 'NEXAROUTE_VERSION must be a release tag such as v0.6.0.'
    base="$repo/download/$version"
  fi
  # /usr/local/bin is already in Ubuntu's PATH. A child shell cannot change the
  # caller's PATH; do not silently pretend that a new ~/.local/bin is available.
  dest="${NEXAROUTE_INSTALL_DIR:-/usr/local/bin}"
  [[ "$dest" == /* && "$dest" != *$'\n'* ]] || fail 'Install directory must be an absolute path.'
  case ":$PATH:" in *":$dest:"*) ;; *) fail "$dest is not in PATH. Add it to PATH first, then retry." ;; esac
  sudo_cmd=()
  if [[ ! -d "$dest" || ! -w "$dest" ]]; then
    if [[ "$EUID" -ne 0 ]]; then
      command -v sudo >/dev/null || fail "Cannot write $dest; create a writable directory in PATH or install sudo."
      echo "Administrator permission is required only to install into $dest."
      sudo_cmd=(sudo)
    fi
  fi
  cfg_home="${XDG_CONFIG_HOME:-${HOME:?HOME is required}/.config}"
  [[ "$cfg_home" == /* ]] || fail 'XDG_CONFIG_HOME must be absolute.'
  cfg="${NEXAROUTE_CONFIG:-$cfg_home/nexaroute/config.json}"
  [[ "$cfg" == /* ]] || fail 'NEXAROUTE_CONFIG must be absolute for installation.'
  tmp="$(mktemp -d)"
  staged=""
  cleanup() {
    rm -rf "$tmp"
    if [[ -n "$staged" ]]; then "${sudo_cmd[@]}" rm -f -- "$staged"; fi
  }
  trap cleanup EXIT
  asset="nexaroute-linux-$arch"
  fetch() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --show-error --silent --location \
      --connect-timeout 15 --max-time 300 --retry 3 "$1" -o "$2" || fail "Download failed: $1. Check the release exists and your network connection."
  }
  echo "Downloading NexaRoute $version ($arch)..."
  fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
  fetch "$base/$asset" "$tmp/$asset"
  # Verify only the exact asset, rejecting duplicate/malformed entries. Never
  # execute paths or filenames supplied by the remote checksum manifest.
  expected="$(awk -v asset="$asset" '$2 == asset {print $1}' "$tmp/SHA256SUMS")"
  [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || fail "Invalid or missing checksum for $asset."
  printf '%s  %s\n' "$expected" "$asset" > "$tmp/checksum"
  (cd "$tmp" && sha256sum --check --status checksum) || fail 'SHA256 mismatch; nothing installed. Retry with a pinned release tag.'
  "${sudo_cmd[@]}" mkdir -p -- "$dest"
  staged="$("${sudo_cmd[@]}" mktemp "$dest/.nexaroute-install.XXXXXX")"
  "${sudo_cmd[@]}" install -m 0755 "$tmp/$asset" "$staged"
  # Atomic replacement also works while the old executable is running.
  "${sudo_cmd[@]}" mv -fT -- "$staged" "$dest/nexaroute"
  staged=""
  mkdir -p -- "$(dirname "$cfg")"
  # Never seed examples or rewrite existing configuration during installation.
  printf '\nInstalled: %s/nexaroute\nConfig (created on first run): %s\n\nRun: nexaroute\nUpgrading? Stop and restart NexaRoute to use the new binary.\n' "$dest" "$cfg"
  if [[ "$(command -v nexaroute)" != "$dest/nexaroute" ]]; then
    echo "WARNING: another nexaroute shadows this installation in PATH; use $dest/nexaroute or remove the older entry." >&2
  fi
}

main "$@"
