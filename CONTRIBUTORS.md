# Contributors

## Optional `.env` file (devcontainer)

A `.env` file at the workspace root is **not required** to build, test, or run
this project. It exists only to grant a coding agent (e.g. Claude Code, Codex)
read-only access to GitHub via the `gh` CLI — useful for inspecting CI logs and
PRs without leaving the devcontainer.

If you want that capability, create `.env` after cloning:

```sh
echo 'GH_TOKEN=<your-fine-grained-PAT>' > .env
```

Recommendations for the token:

- **Fine-grained personal access token**, scoped to the repos the agent needs.
- **Read-only**: contents `Read`, metadata `Read`, pull requests `Read`,
  actions `Read`. No write scopes.
- Treat it like a credential — `.env` is gitignored (`*.env` rule).

The devcontainer auto-loads any `/workspaces/*/.env` into login shells via a
small `/etc/profile.d` snippet — so `gh` is authenticated without manual
sourcing. If `.env` is absent, `post-start.sh` prints a one-line hint and
nothing else changes.
