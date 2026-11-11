// Package buildinfo provides build-time version information for the bunn CLI.
// Fields are populated at build time via -ldflags.
package buildinfo

import "runtime"

// Version is the CLI version, set at build time.
var Version = "dev"

// Commit is the Git commit SHA, set at build time.
var Commit = ""

// Date is the build timestamp, set at build time.
var Date = ""

// GoVersion reports the Go version used to build the CLI.
func GoVersion() string {
	return runtime.Version()
}
