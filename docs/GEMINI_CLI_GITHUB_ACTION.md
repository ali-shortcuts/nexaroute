# Gemini CLI GitHub Actions coding agent

NexaRoute now has a dedicated workflow at `.github/workflows/gemini-cli.yml`, based on Google's official GitHub Action:
<https://github.com/google-github-actions/run-gemini-cli>

## One-time setup

1. Create a Gemini API key in [Google AI Studio](https://aistudio.google.com/apikey).
2. Open [NexaRoute Actions secrets](https://github.com/ali-shortcuts/nexaroute/settings/secrets/actions).
3. Choose **New repository secret**.
4. Set the name to `GEMINI_API_KEY` and paste the API key as its value.
5. Ensure GitHub Actions is enabled for the repository.

Do not commit or post the key in an Issue or PR. The official action documents free-of-charge quotas for Google AI Studio, but quotas, model availability, and pricing terms can change.

## Use the agent

After this PR is merged into `main` and the secret exists, add a comment to an Issue or PR authored by `ali-shortcuts` in this exact format:

```text
@gemini-cli /fix Investigate issue #123, implement a minimal safe fix, add a regression test, run relevant tests, and open a PR. Do not merge.
```

The workflow intentionally ignores other users' comments. It asks Gemini to create a separate branch and PR, never to merge or close issues. The workflow needs validation in a real Actions run before it should be trusted with important changes.

## Direct links

- [Google AI Studio — create API key](https://aistudio.google.com/apikey)
- [NexaRoute — Actions secrets](https://github.com/ali-shortcuts/nexaroute/settings/secrets/actions)
- [Google's official Gemini CLI GitHub Action](https://github.com/google-github-actions/run-gemini-cli)
- [Official Action README and setup](https://github.com/google-github-actions/run-gemini-cli/blob/v0.1.22/README.md)
- [Google's GitHub Actions setup instructions](https://github.com/google-github-actions/run-gemini-cli/blob/v0.1.22/examples/workflows/gemini-assistant/README.md)
