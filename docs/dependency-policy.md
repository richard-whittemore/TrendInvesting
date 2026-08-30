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

Go-based tools must be tracked with Go 1.24 `tool` directives and invoked with `go tool`. Pin released, Go-1.24-compatible versions; do not install `@latest` in CI. The initial tools are:

- Staticcheck for repeatable linting; and
- `govulncheck` for reachable known-vulnerability analysis.

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
