// Package buildinfo exposes build metadata that release tooling may override.
package buildinfo

// Unstamped is what Version reports for a build that was not stamped: one
// built with a plain `go build`, `go test` or `go run`.
const Unstamped = "dev"

// Version is the application version. A stamped build replaces it with
// -ldflags "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=<version>"
// (the Makefile's LDFLAGS, `git describe --always --dirty`).
var Version = Unstamped
