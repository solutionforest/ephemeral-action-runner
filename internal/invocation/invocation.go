package invocation

import (
	"os"
	"path/filepath"
	"strings"
)

// Environment identifies the user-facing entry point selected by an EPAR
// wrapper. Values are deliberately closed so an inherited environment variable
// cannot inject arbitrary text into a suggested command.
const Environment = "EPAR_INVOCATION"

// Command returns a command line that uses the same entry point as the current
// process.
func Command(args ...string) string {
	parts := append([]string{commandPrefix(os.Getenv(Environment), os.Args[0], executablePath())}, args...)
	return strings.Join(parts, " ")
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}

func commandPrefix(marker, arg0, executable string) string {
	switch marker {
	case "start":
		return "./start"
	case "run-with-docker":
		return "scripts/run-with-docker.sh"
	case "run-with-docker-powershell":
		return `scripts\run-with-docker.ps1`
	}
	if isGoRunExecutable(executable) {
		return "go run ./cmd/ephemeral-action-runner"
	}
	if strings.TrimSpace(arg0) == "" {
		return "ephemeral-action-runner"
	}
	if strings.ContainsAny(arg0, " \t") {
		return `"` + arg0 + `"`
	}
	return arg0
}

func isGoRunExecutable(path string) bool {
	normalized := strings.ToLower(filepath.ToSlash(strings.ReplaceAll(path, `\`, "/")))
	for _, segment := range strings.Split(normalized, "/") {
		if strings.HasPrefix(segment, "go-build") {
			return true
		}
	}
	return false
}
