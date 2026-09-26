# Installation (Ubuntu / Linux)

The release installer downloads the published binary for Linux x86-64 or ARM64 and verifies it against the release `SHA256SUMS` file. It does not clone or compile source and does not require Go.

```bash
curl -fsSL https://raw.githubusercontent.com/ali-shortcuts/nexaroute/main/scripts/install.sh | bash
```

By default, root installs to `/usr/local/bin`; a regular user installs to `~/.local/bin`. Override with `NEXAROUTE_INSTALL_DIR`. If `~/.local/bin` is not on `PATH`, add it as suggested by the installer. Configuration is stored under `${XDG_CONFIG_HOME:-~/.config}/nexaroute/config.json`; upgrades leave it untouched. The installer requires curl, sha256sum and python3.

This release installer requires a published GitHub release containing `nexaroute-linux-amd64` or `nexaroute-linux-arm64` and `SHA256SUMS`. If the latest release does not yet contain those assets, installation fails safely with an error; it will not fall back to an unverified source build.

Then run `nexaroute`. The local dashboard defaults to `http://127.0.0.1:8080/`. See [QUICKSTART](QUICKSTART.md) and [Claude Code setup](CLAUDE_CODE.md).
