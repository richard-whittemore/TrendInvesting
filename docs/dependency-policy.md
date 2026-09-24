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

Go-based tools must be tracked with `tool` directives for the Go version this module currently declares and invoked with `go tool`. Pin released, compatible versions; do not install `@latest` in CI. The tools are:

- Staticcheck (pinned to v0.6.1, unchanged since it only requires `go 1.23` — well under the current floor) for repeatable linting;
- `govulncheck` (pinned to v1.1.4, unchanged since it only requires `go 1.22.0`) for reachable known-vulnerability analysis; and
- golangci-lint (**pinned to v2.14.0**) for correctness linting and for mechanically enforcing the architectural and determinism rules in `AGENTS.md` — see `.golangci.yml`.

> **Do not upgrade golangci-lint without checking its Go requirement.** Every release states, in its own `go.mod`, the minimum Go version it needs; check that file (`go mod download` the candidate version and read its `go` directive) before bumping the pin, not the release notes alone. `go get` a newer golangci-lint pulls its own dependency versions along with it under minimum version selection — including a newer `honnef.co/go/tools` (the same module that backs the separate Staticcheck pin above) — so a routine bump can still change what a `go tool` command sees, even when this policy's own Staticcheck pin line is untouched.
>
> **History:** v2.8.0 was the newest release compatible with Go 1.24 (`go 1.24.0`). v2.10.0 onward requires `go >= 1.25`; v2.13 requires `go >= 1.26`. `os.ReadDir`, `os.DirFS`, and `fs.ReadDir` were reachable-vulnerable on 1.24.4 (GO-2026-4602, fixed in 1.25.8), which forced the floor to Go 1.27.1 and, with it, golangci-lint to v2.14.0 (`go 1.26.0`, the newest v2 release at the time and comfortably under the new floor). `.go-version`, `go.mod`'s `go` directive, and the installed toolchain moved together, as this policy requires; `GOTOOLCHAIN=local` was unaffected — it still refuses any toolchain the machine does not already have, it now just refuses anything short of 1.27.1.

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
