# GitHub AI coding-agent integrations

This guide describes supported integrations for NexaRoute. Adding repository instructions does **not** install an external GitHub App or grant it repository access.

## Google Jules — implementation agent

Official entry point: <https://jules.google/>

1. Sign in with the Google account that should own the integration.
2. Choose **Connect to GitHub account** and authorize Google Labs Jules.
3. In the GitHub installation screen, grant access to **only** `ali-shortcuts/nexaroute` unless broader access is genuinely needed.
4. Return to Jules, select this repository and the intended branch, and give it one small, clearly scoped Issue first.
5. Review the generated plan and diff. Require the repository's relevant tests and CI to pass before merging.

Jules reads the root `AGENTS.md` file. API-based automation is also possible, but the Jules API is documented as alpha and requires a separately managed API key; do not commit that key to the repository.

Official docs:
- <https://jules.google/docs/>
- <https://jules.google/docs/tasks-repos/>
- <https://jules.google/docs/api/reference/>

## Gemini Code Assist on GitHub — PR reviewer

Official GitHub App: <https://github.com/apps/gemini-code-assist>

This integration is intended primarily for automated PR summaries and code reviews, not as a replacement for a full implementation agent.

1. Follow Google's setup guide in Google Cloud.
2. A valid Google Cloud project with a billing account and the required IAM permissions is needed for setup.
3. Install the Gemini Code Assist GitHub App for **only** `ali-shortcuts/nexaroute`.
4. Link the repository through Developer Connect and finish the setup.
5. Open a test PR and verify that `gemini-code-assist[bot]` posts a review. The documented manual commands include `/gemini summary` and `/gemini review`.

This repository includes `.gemini/styleguide.md` with project-specific review expectations. During the Preview described by Google's documentation, Gemini Code Assist on GitHub usage has no charge, but a valid billing account is still required and terms/availability can change.

Official docs:
- <https://docs.cloud.google.com/gemini/docs/code-review/set-up-code-assist-github>
- <https://docs.cloud.google.com/gemini/docs/code-review/use-code-assist-github>

## Existing OpenCode GitHub Actions agent

The repository already has `.github/workflows/opencode.yml`. It responds to authorized `/opencode` or `/oc` comments and runs through GitHub Actions. Keep this workflow as a separate implementation path; do not assume an external app is connected just because its instructions are present in the repository.

## Security and operating rules

- Never store provider keys, OAuth tokens, Jules API keys, or other secrets in tracked files or Issue/PR comments.
- Prefer least-privilege installation scoped to this repository.
- Agents must use branches and PRs; do not let an agent bypass required checks or merge its own unverified work.
- Do not run multiple agents against the same Issue/branch concurrently.
- Treat free quotas, Preview terms, and third-party model availability as changeable; verify current terms before relying on them.
- A successful app installation, a completed agent task, passing CI, and a merged PR are four different states. Verify each independently.
