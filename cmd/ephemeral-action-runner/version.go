package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
	// sourceRevision is populated by native-controller build scripts as a
	// diagnostic identifier. Source matching is performed with sourceDigest so
	// it remains valid without Git metadata and for locally patched source.
	sourceRevision = "unknown"
	// sourceDigest and buildDigest are populated by the native-controller build
	// scripts. They are intentionally separate: sourceDigest identifies the
	// effective source inputs, while buildDigest also identifies the target and
	// build recipe.
	sourceDigest = "unknown"
	buildDigest  = "unknown"
)

func versionString() string {
	return fmt.Sprintf(`%s %s
commit: %s
buildDate: %s
sourceRevision: %s
sourceDigest: %s
buildDigest: %s
controllerSlot: %s
go: %s
platform: %s/%s
`, binaryName, version, commit, buildDate, sourceRevision, sourceDigest, buildDigest, controllerSlot(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func controllerSlot() string {
	if slot := os.Getenv("EPAR_CONTROLLER_SLOT"); slot != "" {
		return slot
	}
	return "unmanaged"
}

func printVersion(w io.Writer) {
	fmt.Fprint(w, versionString())
}
