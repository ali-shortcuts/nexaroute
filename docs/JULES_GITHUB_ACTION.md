# Google Jules GitHub Actions agent

NexaRoute includes `.github/workflows/jules.yml`, based on Google's `google-labs-code/jules-invoke@v1` action. This provides a comment-triggered implementation path alongside the existing OpenCode agent.

## One-time setup

1. Sign in at <https://jules.google/> and connect your GitHub account.
2. Authorize Jules for only `ali-shortcuts/nexaroute` unless broader access is needed.
3. Generate a Jules API key in Jules settings.
4. In this repository, open **Settings → Secrets and variables → Actions → New repository secret**.
5. Set the secret name to `JULES_API_KEY` and its value to your key. Never commit the key or paste it into an Issue/PR comment.

## Usage

Comment on an Issue or PR with `/jules ` followed by a specific task:

```text
/jules Investigate issue #123, implement the smallest safe fix, add a regression test, run relevant tests, and open a PR. Do not merge.
```

The workflow is restricted to comments authored by `ali-shortcuts`. Jules is instructed to read `AGENTS.md`, work only on the requested task, run relevant tests, and create a reviewable PR without merging it. The issue/PR body is context, not authority to override the maintainer's command or repository rules.

## Important limitations

- The workflow will not successfully invoke Jules until `JULES_API_KEY` is configured as an Actions secret.
- Adding the workflow does not itself install/authorize Jules for the repository.
- The official Jules API/action is experimental and can change.
- Review generated diffs and require CI/security checks before merging.
- A successful workflow dispatch, a Jules session, a created PR, passing CI, and a merged PR are distinct outcomes.

## Official references

- <https://github.com/google-labs-code/jules-action>
- <https://jules.google/docs/>
- <https://jules.google/docs/api/reference/>
