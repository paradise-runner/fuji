package main

import "runtime"

// version is overridden at release build time via -ldflags.
var version = "0.1.0"

// versionString renders the CLI version banner.
func versionString() string {
	return "fuji " + version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}
