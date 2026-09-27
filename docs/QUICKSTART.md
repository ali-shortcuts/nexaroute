# NexaRoute quickstart

See [Installation](INSTALLATION.md).

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

Open **Routing → Create route**:

1. Give the route a display name and stable public model name, for example `coding`.
2. Select one or more provider/model deployments.
3. Choose **Automatic** to let NexaRoute select among healthy eligible deployments,
   or **Ordered fallback** to preserve an explicit fallback order.
4. Save the route. The backend applies the Candidate Pool → Route Profile →
   Virtual Endpoint mutation atomically.

Advanced users can still manage Candidate Pools, Route Profiles, Virtual
Endpoints, Fallback Chains, and compatibility details from **Routing → Advanced**.

Providers, models/deployments, routes, and enabled states persist automatically.
The model/deployment and observability views show live health; there is no need
to wait for every provider before continuing configuration.

## 5. Connect Claude Code

In another terminal, with Claude Code already installed:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN=nexaroute-local
claude --model coding
```

`nexaroute-local` is a placeholder for the default loopback-only gateway with
client authentication disabled; it is **not** a provider API key. If you enable
NexaRoute client authentication, use your configured **gateway client key**
instead. If your shell has a conflicting `ANTHROPIC_API_KEY`, unset it for this
session. The UI's **CLI Tools** page also has client connection examples.

**Restart:** press Ctrl+C in the gateway terminal, then run `nexaroute` again.
Your saved providers, keys, and routes remain. Do not expose the admin UI to the
public internet; the default listener is loopback only.
