# NexaRoute quickstart

1. Install:

   ```bash
   ./install-user.sh
   ```

2. Start the gateway (opens the UI when a browser is available):

   ```bash
   nexaroute
   ```

3. Open `http://127.0.0.1:8080/` if the browser did not launch.

4. In **Providers**, add an OpenAI-compatible or Anthropic-compatible upstream.
   Enter the API key once. After save, the UI never shows the plaintext key
   again; leave the field blank to keep the stored secret.

5. Detect or enter models, enable the provider, run **Probe all models**.

6. Point Claude Code or any Anthropic/OpenAI client at the local gateway
   (see `docs/CLAUDE_CODE.md`).

Sample providers in the example config stay disabled until you enable them.
No paid provider is contacted during install or first start.
