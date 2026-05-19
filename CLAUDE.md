# CLAUDE.md

**Read [AGENTS.md](AGENTS.md) before making any changes.** It is the source of
truth for how AI coding agents must work in this repository:

- Use `task` targets — never invoke `go`, `golangci-lint`, `helm`, or `docker`
  directly when an equivalent target exists.
- Run `task ci` and the relevant `task e2e:*` lanes before asking for a commit.
- Never run `git commit` / `git push` or open PRs yourself — stage at most.

See [AGENTS.md](AGENTS.md) for the full Task runner reference, e2e flows, code
conventions, and the related-repository map.
