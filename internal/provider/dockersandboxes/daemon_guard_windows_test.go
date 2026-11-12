//go:build windows

package dockersandboxes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

// The test executable also acts as an sbx CLI double, before flag parsing.
func TestMain(m *testing.M) {
	if state := os.Getenv("EPAR_WINDOWS_SBX_GUARD_STATE"); state != "" {
		if strings.Join(os.Args[1:], " ") == "daemon status --json" {
			fmt.Print(state)
		} else {
			if err := os.WriteFile(os.Getenv("EPAR_WINDOWS_SBX_GUARD_COMMAND"), []byte(strings.Join(os.Args[1:], " ")), 0600); err != nil {
				os.Exit(2)
			}
			fmt.Print("[]")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWindowsCommandDoesNotAutoStartStoppedDaemon(t *testing.T) {
	for _, state := range []string{"running", "stopped", "unknown"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			t.Setenv("EPAR_WINDOWS_SBX_GUARD_STATE", fmt.Sprintf(`{"status":%q}`, state))
			marker := filepath.Join(t.TempDir(), "command")
			t.Setenv("EPAR_WINDOWS_SBX_GUARD_COMMAND", marker)
			p := New(os.Args[0])
			result, err := p.runRaw(context.Background(), commandRequest{args: []string{"ls", "--json"}, operation: "inventory Docker Sandboxes", outputLimit: 1024})
			_, markerErr := os.Stat(marker)
			if state == "running" {
				if err != nil || result.Stdout != "[]" || markerErr != nil {
					t.Fatalf("running daemon command = %v, %v; marker %v", result, err, markerErr)
				}
				return
			}
			if err == nil || !os.IsNotExist(markerErr) {
				t.Fatalf("command ran without a verified running daemon: result=%v error=%v marker=%v", result, err, markerErr)
			}
			if state == "stopped" && (!errors.Is(err, provider.ErrControlPlaneFailure) || !strings.Contains(err.Error(), "sbx daemon start --detach")) {
				t.Fatalf("stopped daemon error lacks classification or remediation: %v", err)
			}
		})
	}
}
