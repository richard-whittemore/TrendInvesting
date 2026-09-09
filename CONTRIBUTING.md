# Contributing

## Toolchain

Use the exact Go version declared in `.go-version` and `go.mod`. The build sets `GOTOOLCHAIN=local` so Go does not silently download or select a different toolchain.

Run the complete local quality gate before opening a pull request:

```sh
make check
```

That command verifies module integrity, formatting, `go vet`, Staticcheck, race-enabled tests, the coverage threshold, known vulnerabilities, and compilation. The same command runs in GitHub Actions.

## Changes

- Link each change to its GitHub issue: the PR title begins with the identifier (`#N: <title>`) and the body includes `Closes #N`. See `docs/agents/issue-tracker.md`.
- Keep methodology claims separate from experimental rules.
- Add tests for successful behavior, rejected inputs, and safety invariants.
- Document replay, audit, schema, and configuration effects.
- Do not add live-trading behavior before the corresponding readiness gates pass.

See `docs/dependency-policy.md` before adding or upgrading a dependency.
