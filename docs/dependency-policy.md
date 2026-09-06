# Dependency policy

Dependencies increase reproducibility, security, licensing, and operational risk. Prefer the Go standard library when it provides a clear and maintainable solution.

## Runtime dependencies

A new runtime dependency requires a pull-request rationale covering:

- the capability it provides and alternatives considered;
- maintenance activity and release stability;
- license compatibility;
- transitive dependency impact;
- security history and data-access implications; and
- how it affects deterministic replay or production failure modes.

Commit `go.mod` and `go.sum`. Do not use an unreviewed `replace` or `exclude` directive. A long-lived fork or replacement requires an architecture decision record.

## Development tools

Go-based tools must be tracked with Go 1.24 `tool` directives and invoked with `go tool`. Pin released, Go-1.24-compatible versions; do not install `@latest` in CI. The tools are:

- Staticcheck for repeatable linting;
- `govulncheck` for reachable known-vulnerability analysis; and
- golangci-lint (**pinned to v2.8.0**) for correctness linting and for mechanically enforcing the architectural and determinism rules in `AGENTS.md` — see `.golangci.yml`.

> **Do not upgrade golangci-lint without checking its Go requirement.** Releases from v2.10.0 onward declare `go >= 1.25`, and v2.13 declares `go >= 1.26`. Adding one rewrites this module's `go` directive to match, which breaks `GOTOOLCHAIN=local` against the `1.24.4` pin in `.go-version` and silently reintroduces automatic toolchain downloads. v2.8.0 is the newest release compatible with Go 1.24. Moving past it is a deliberate toolchain upgrade: bump `.go-version`, `go.mod`, and the installed toolchain together, and record the decision.

## GitHub Actions

Third-party Actions must use immutable commit SHAs with the release version recorded in a comment. Grant the workflow only the permissions it needs.

## Updating dependencies

Dependabot checks Go modules and GitHub Actions weekly. Every update must pass `make check`. Major upgrades require explicit review of behavior and migration notes; security updates receive priority but may not bypass tests.

Before merging a dependency change, run:

```sh
go mod tidy -diff
go mod verify
go tool govulncheck ./...
```

Known reachable vulnerabilities block merging unless a time-bounded exception documents exposure, compensating controls, and removal criteria.
