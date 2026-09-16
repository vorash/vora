// Package version exposes build version information.
package version

// Version is set at build time via -ldflags
// (-X github.com/vorash/vora/internal/version.Version=<version>).
var Version = "dev"

// String returns the human-readable build version.
func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
