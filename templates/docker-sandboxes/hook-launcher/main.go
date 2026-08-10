//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

const (
	realBash = "/bin/bash"
	hookPath = "/opt/epar/prepare-job-start.sh"
)

func main() {
	arguments := append([]string{"bash"}, os.Args[1:]...)
	environment := os.Environ()
	if isHostTrustHookInvocation(os.Args[1:]) {
		arguments = append([]string{"bash", "-p"}, os.Args[1:]...)
		environment = isolatedHookEnvironment(environment)
	}
	if err := syscall.Exec(realBash, arguments, environment); err != nil {
		fmt.Fprintf(os.Stderr, "EPAR bash launcher: exec failed: %v\n", err)
		os.Exit(126)
	}
}

func isHostTrustHookInvocation(arguments []string) bool {
	for _, argument := range arguments {
		if argument == hookPath {
			return true
		}
	}
	return false
}

func isolatedHookEnvironment(environment []string) []string {
	allowed := map[string]bool{
		"LANG":       true,
		"LC_ALL":     true,
		"TZ":         true,
		"GITHUB_ENV": true,
	}
	result := make([]string, 0, len(allowed)+2)
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && allowed[name] {
			result = append(result, entry)
		}
	}
	result = append(result, "PATH=/usr/bin:/bin", "EPAR_HOOK_LAUNCHER=isolated-v1")
	return result
}
