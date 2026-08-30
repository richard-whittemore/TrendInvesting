// Package buildinfo exposes build metadata that release tooling may override.
package buildinfo

// Version is the application version. Release builds may replace it with
// -ldflags "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=<version>".
var Version = "dev"
