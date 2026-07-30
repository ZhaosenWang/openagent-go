package main

import "time"

// version is the build version. It is injected at compile time via
// -ldflags "-X main.version=<ver>"; when unset, a dev build tag is
// generated from the current timestamp.
var version = ""

func init() {
	// Fallback for a bare `go build` (no ldflags): tag as a dev build
	// stamped with the build timestamp.
	if version == "" {
		version = "0.0.0-dev." + time.Now().Format("20060102150405")
	}
	// Wire the version into cobra's built-in --version path. We add the
	// flag ourselves so it gets the -v shorthand; cobra sees the flag
	// already exists and skips its default (shorthand-less) one, while
	// still short-circuiting to print the version template.
	rootCmd.Version = version
	rootCmd.Flags().BoolP("version", "v", false, "show version number")
	rootCmd.SetVersionTemplate("{{.Version}}\n")
}
