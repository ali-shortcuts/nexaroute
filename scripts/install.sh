#!/usr/bin/env bash
# Canonical release-only Linux installer. Safe to run via curl | bash; it never
# compiles source and never rewrites an existing NexaRoute configuration.
set -Eeuo pipefail

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
    command -v "$dep" >/dev/null 2>&1 || fail "Missing $dep. On Ubuntu install curl and coreutils."
  done

  version="${NEXAROUTE_VERSION:-latest}"
  if [[ "$version" != latest ]]; then
    [[ "$version" =~ ^v[0-9][a-zA-Z0-9._-]*$ ]] || fail 'NEXAROUTE_VERSION must be a release tag such as v0.7.0.'
  fi

  # /usr/local/bin is already in Ubuntu's PATH. A child shell cannot change the
  # caller's PATH, so custom destinations must also already be in PATH.
  dest="${NEXAROUTE_INSTALL_DIR:-/usr/local/bin}"
  [[ "$dest" == /* && "$dest" != *$'\n'* ]] || fail 'Install directory must be an absolute path.'
  case ":$PATH:" in *":$dest:"*) ;; *) fail "$dest is not in PATH. Add it to PATH first, then retry." ;; esac

  sudo_cmd=()
  if [[ ! -d "$dest" ]]; then
    if ! mkdir -p -- "$dest" 2>/dev/null; then
      if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
        command -v sudo >/dev/null 2>&1 || fail "Cannot create $dest; create a writable directory in PATH or install sudo."
        echo "Administrator permission is required only to install into $dest."
        sudo_cmd=(sudo)
      fi
    fi
  elif [[ ! -w "$dest" && "${EUID:-$(id -u)}" -ne 0 ]]; then
    command -v sudo >/dev/null 2>&1 || fail "Cannot write $dest; choose a writable directory in PATH or install sudo."
    echo "Administrator permission is required only to install into $dest."
    sudo_cmd=(sudo)
  fi

  cfg_home="${XDG_CONFIG_HOME:-${HOME:?HOME is required}/.config}"
  [[ "$cfg_home" == /* ]] || fail 'XDG_CONFIG_HOME must be absolute.'
  cfg="${NEXAROUTE_CONFIG:-$cfg_home/nexaroute/config.json}"
  [[ "$cfg" == /* ]] || fail 'NEXAROUTE_CONFIG must be absolute for installation.'

  tmp="$(mktemp -d)"
  staged=""
  cleanup() {
    rm -rf -- "$tmp"
    if [[ -n "$staged" ]]; then "${sudo_cmd[@]}" rm -f -- "$staged"; fi
  }
  trap cleanup EXIT

  asset="nexaroute-linux-$arch"
  if [[ -n "${NEXAROUTE_LOCAL_RELEASE_DIR:-}" ]]; then
    release_dir="$NEXAROUTE_LOCAL_RELEASE_DIR"
    tag="${NEXAROUTE_LOCAL_RELEASE_TAG:-$version}"
    [[ -d "$release_dir" ]] || fail "Local release directory does not exist: $release_dir"
    [[ -f "$release_dir/$asset" ]] || fail "Local release asset not found: $release_dir/$asset"
    [[ -f "$release_dir/SHA256SUMS" ]] || fail "Local release checksum manifest not found: $release_dir/SHA256SUMS"
    cp -- "$release_dir/$asset" "$tmp/$asset"
    cp -- "$release_dir/SHA256SUMS" "$tmp/SHA256SUMS"
  else
    repo=https://github.com/ali-shortcuts/nexaroute/releases
    if [[ "$version" == latest ]]; then
      base="$repo/latest/download"
    else
      base="$repo/download/$version"
    fi
    tag="$version"
    fetch() {
      curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --show-error --silent --location \
        --connect-timeout 15 --max-time 300 --retry 3 "$1" -o "$2" ||
        fail "Download failed: $1. Check that the release exists and retry."
    }
    echo "Downloading NexaRoute $version ($arch)..."
    fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
    fetch "$base/$asset" "$tmp/$asset"
  fi

  # Verify exactly one checksum entry for the selected asset. Never execute a
  # path or filename supplied by the remote manifest.
  expected="$(awk -v asset="$asset" '$2 == asset {print $1}' "$tmp/SHA256SUMS")"
  [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || fail "Invalid, duplicate, or missing checksum for $asset."
  printf '%s  %s\n' "$expected" "$asset" > "$tmp/checksum.selected"
  (cd "$tmp" && sha256sum --check --status checksum.selected) || fail 'SHA256 mismatch; nothing installed.'
  [[ -s "$tmp/$asset" ]] || fail 'Downloaded binary is empty.'

  "${sudo_cmd[@]}" mkdir -p -- "$dest"
  staged="$("${sudo_cmd[@]}" mktemp "$dest/.nexaroute-install.XXXXXX")"
  "${sudo_cmd[@]}" install -m 0755 "$tmp/$asset" "$staged"
  # Atomic replacement works even while an older executable is running.
  "${sudo_cmd[@]}" mv -f -- "$staged" "$dest/nexaroute"
  staged=""

  mkdir -p -- "$(dirname "$cfg")"
  chmod 700 -- "$(dirname "$cfg")"
  printf '\nInstalled NexaRoute %s to %s/nexaroute\nConfig (created on first run): %s\n\nRun: nexaroute\nUpgrading? Stop and restart NexaRoute to use the new binary.\n' "$tag" "$dest" "$cfg"
  if [[ "$(command -v nexaroute 2>/dev/null || true)" != "$dest/nexaroute" ]]; then
    echo "WARNING: another nexaroute shadows this installation in PATH; use $dest/nexaroute or remove the older entry." >&2
  fi
}

main "$@"
