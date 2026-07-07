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
)

func versionString() string {
	return fmt.Sprintf(`%s %s
commit: %s
buildDate: %s
go: %s
platform: %s/%s
`, binaryName, version, commit, buildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func printVersion(w io.Writer) {
	fmt.Fprint(w, versionString())
}
