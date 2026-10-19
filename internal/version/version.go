// Package version holds the release version, stamped in by the build:
//
//	go build -ldflags "-X github.com/thomasreichmann/outloud/internal/version.Version=0.1.0"
package version

// Version is the release version without a leading "v", or "dev".
var Version = "dev"

// String names the software and its version, e.g. "Outloud 0.1.0".
func String() string { return "Outloud " + Version }
