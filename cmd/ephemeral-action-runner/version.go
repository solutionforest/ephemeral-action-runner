package main

import (
	"fmt"
	"io"
	"runtime"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
	// sourceRevision is populated only by the native-controller build scripts.
	// Promotion requires its exact clean-source sha256 identity; go run,
	// release builds without equivalent plumbing, and dirty builds fail closed.
	sourceRevision = "unknown"
)

func versionString() string {
	return fmt.Sprintf(`%s %s
commit: %s
buildDate: %s
sourceRevision: %s
go: %s
platform: %s/%s
`, binaryName, version, commit, buildDate, sourceRevision, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func printVersion(w io.Writer) {
	fmt.Fprint(w, versionString())
}
