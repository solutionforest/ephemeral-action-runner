//go:build !windows

package dockersandboxes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRelayActivationBootstrapPreservesInputAndExecLifetime(t *testing.T) {
	for _, state := range []string{"missing", "existing", "symlink"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"sudo":      "#!/bin/sh\nshift\nexec \"$@\"\n",
				"bootstrap": "#!/bin/bash\nset -e\ncat > \"$EPAR_TEST_ROOT/bootstrap.stdin\"\nsleep 30 &\necho $! > \"$EPAR_TEST_ROOT/child.pid\"\necho bridge > \"$EPAR_TEST_ROOT/bridge.pid\"\n",
				"activate":  "#!/bin/bash\nset -e\ntest -f \"$EPAR_TEST_ROOT/bridge.pid\"\ntest ! -L \"$EPAR_TEST_ROOT/bridge.pid\"\ncat > \"$EPAR_TEST_ROOT/activation.stdin\"\n",
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if state == "existing" {
				if err := os.WriteFile(filepath.Join(root, "bridge.pid"), []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if state == "symlink" {
				if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "bridge.pid")); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if data, err := os.ReadFile(filepath.Join(root, "child.pid")); err == nil {
					if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
						if child, err := os.FindProcess(pid); err == nil {
							_ = child.Kill()
							_ = child.Release()
						}
					}
				}
			})
			script := strings.NewReplacer(
				"/run/epar/egress-bridge.pid", `"$EPAR_TEST_ROOT/bridge.pid"`,
				"/opt/epar/template-entrypoint.sh", `"$EPAR_TEST_ROOT/bootstrap"`,
				"/opt/epar/configure-egress-relay.sh", `"$EPAR_TEST_ROOT/activate"`,
			).Replace(hostTrustRelayActivationScript)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-c", script)
			command.Env = append(os.Environ(), "EPAR_TEST_ROOT="+root, "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"))
			command.WaitDelay = 100 * time.Millisecond
			const payload = "{\"token\":\"fixture-only\"}\n"
			command.Stdin = strings.NewReader(payload)
			output, err := command.CombinedOutput()
			if state == "symlink" {
				if err == nil {
					t.Fatal("activation accepted a symlink PID path")
				}
			} else {
				if err != nil {
					t.Fatalf("activation did not finish with a long-lived bridge child: %v / %s", err, output)
				}
				data, err := os.ReadFile(filepath.Join(root, "activation.stdin"))
				if err != nil || string(data) != payload {
					t.Fatal("bootstrap consumed or changed activation input")
				}
			}
			bootstrapInput, bootstrapErr := os.ReadFile(filepath.Join(root, "bootstrap.stdin"))
			if state == "missing" && (bootstrapErr != nil || len(bootstrapInput) != 0) {
				t.Fatal("bootstrap did not receive an empty input stream")
			}
			if state != "missing" && !os.IsNotExist(bootstrapErr) {
				t.Fatal("bootstrap ran despite an existing or symlink PID path")
			}
		})
	}
}
