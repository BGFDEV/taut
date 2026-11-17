// Package version holds build metadata, set at link time by the Makefile.
package version

// Set with -ldflags "-X github.com/bgfdev/taut/internal/version.Version=…".
var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
)
