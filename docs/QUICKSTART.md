# NexaRoute quickstart — Ubuntu

> The one-command release assets are prepared, not published yet. Use this guide
> once the reviewed release is available. See [Installation](INSTALLATION.md).

## 1. Install

```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash
```

No Go or git needed. Run as your desktop user; sudo may ask permission to install
only the binary into `/usr/local/bin`.

## 2. Run → browser opens

```bash
nexaroute
```

Keep the terminal open. Chrome/the available browser opens the UI. If no browser
opens, visit **http://127.0.0.1:8080/** (or the URL printed in the terminal).

## 3. Add providers

In **Providers → Add provider**:

- Enter a name, ID, compatibility/endpoint type, and base URL.
- Enter the provider API key (or an environment-variable reference).
- Discover models or enter the provider's model IDs manually. Select the models
  you want, their capabilities, and enable the provider/models.
- Save. Keys are write-only: they won't be shown when you reopen the editor.
  Leave credential fields untouched to keep the saved keys.

The UI is usable immediately; provider probes may still be in progress. Check
model/deployment health before making a real request. No usable provider means
no usable route yet, not a failed installation.

## 4. Create a route

Use the **Pools**, **Profiles**, and **Endpoints** sidebar pages:

1. In **Pools**, create a **candidate pool** (for example `coding`). Use `explicit` and select
   deployment IDs such as `my-provider/my-model-id`, or `all` for all candidates.
2. In **Profiles**, create a **route profile** (for example `coding`) that uses that pool.
3. In **Endpoints**, create and enable a **virtual endpoint** with public model `claude-coding`,
   the `coding` route profile. The default endpoint protocols include Anthropic Messages.

Providers, models/deployments, pools, profiles, endpoints, and enabled states
persist automatically. The model/deployment and compatibility views show health;
there is no need to wait for all providers before editing configuration.

## 5. Connect Claude Code

In another terminal, with Claude Code already installed:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN=nexaroute-local
claude --model claude-coding
```

`nexaroute-local` is a placeholder for the default loopback-only gateway with
client authentication disabled; it is **not** a provider API key. If you enable
NexaRoute client authentication, use your configured **gateway client key**
instead. If your shell has a conflicting `ANTHROPIC_API_KEY`, unset it for this
session. The UI's **CLI Tools** page also has client connection examples.

**Restart:** press Ctrl+C in the gateway terminal, then run `nexaroute` again.
Your saved providers, keys, and routes remain. Do not expose the admin UI to the
public internet; the default listener is loopback only.
