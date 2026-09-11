// Package version holds the build version of all tetherd binaries.
package version

// Version is overridden at build time via -ldflags "-X .../version.Version=...".
var Version = "0.1.0-dev"
