# Ubuntu install guide — NexaRoute v0.3

Tested path for a clean Ubuntu machine (22.04 / 24.04, amd64 or arm64).
NexaRoute is a single Go binary plus a JSON config file; there is no database.

## Option A — release package (recommended, no Go needed)

```bash
sudo apt update && sudo apt install -y curl tar
# Download the release tarball for your version, then:
tar -xzf nexaroute-v0.3-linux.tar.gz
cd nexaroute
chmod +x install-user.sh
./install-user.sh
```

This installs (existing config is kept, never overwritten):

```text
~/.local/bin/nexaroute
~/.config/nexaroute/config.json   (mode 0600)
```

Run it:

```bash
~/.local/bin/nexaroute -config ~/.config/nexaroute/config.json
```

Open the dashboard:

```text
http://127.0.0.1:8080/
```

The sample providers ship **disabled**, so nothing contacts the network
until you configure and enable a provider in **Providers → Add provider**,
then press **Probe all models**.

## Option B — build from source (needs Go 1.23+)

```bash
sudo apt update && sudo apt install -y git curl
# Install Go 1.23 or newer from https://go.dev/dl/ if `go version` is older:
#   sudo rm -rf /usr/local/go
#   sudo tar -C /usr/local -xzf go1.23.<patch>.linux-$(dpkg --print-architecture).tar.gz
#   echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile && source ~/.profile

git clone https://github.com/ali-shortcuts/nexaroute.git
cd nexaroute
./scripts/verify.sh     # formatting, tests, vet, race, JS syntax, fuzz, Linux builds
./install-user.sh       # builds from source when no bundled binary exists
~/.local/bin/nexaroute -config ~/.config/nexaroute/config.json
```

Before adding any real provider, self-test the binary without external calls:

```bash
./scripts/smoke-local.sh
```

## Run on boot (user-level systemd service, optional)

After `./install-user.sh`:

```bash
./scripts/install-systemd-user.sh
systemctl --user enable --now nexaroute
journalctl --user -u nexaroute -f
```

Detailed bounded application log:

```bash
tail -F ~/.config/nexaroute/nexaroute.log
```

## First-run checklist

1. Open `http://127.0.0.1:8080/` → **About** tab shows version and creator info.
2. **Providers → Add provider** → fill Base URL + credentials → **Detect models** → **Test selected models** → Save.
3. Press **Probe all models**; healthy deployments appear in **Models**.
4. Point a client at the gateway (see **CLI Tools** tab for copy-paste snippets).
5. Optional: **Access Keys** → generate a key → require client keys on `/v1/*`.

## Updating

```bash
cd nexaroute   # new release or `git pull` for source
./install-user.sh
systemctl --user restart nexaroute   # only if the service is enabled
```

Your existing `~/.config/nexaroute/config.json` is preserved.

## Uninstalling

```bash
systemctl --user disable --now nexaroute 2>/dev/null || true
rm -f ~/.local/bin/nexaroute
rm -rf ~/.config/nexaroute   # only if you want to delete config + logs too
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `address already in use` | Another process owns port 8080. Stop it, or edit `listen` in `~/.config/nexaroute/config.json` (e.g. `127.0.0.1:8081`) and restart. |
| `401 Unauthorized` on admin API | Set an admin key in **Settings → Admin & security**, or export `NEXAROUTE_ADMIN_KEY` before starting. |
| `/readyz` returns 503 | Normal until a deployment proves healthy. Enable a provider and run **Probe all models**. |
| UI loads but no models | Providers ship disabled. Enable one in **Providers → Edit**, then probe. |
| Need remote (non-loopback) access | Change `listen` to `0.0.0.0:8080`, set a strong admin key, and set `bind_local_only: false`. Never expose the admin UI to the public internet without TLS and perimeter controls. |
| Check the version | `~/.local/bin/nexaroute -version` → `NexaRoute v0.3` |

Security notes live in `SECURITY.md`; every runtime knob is documented in
`docs/CONFIGURATION.md`.
