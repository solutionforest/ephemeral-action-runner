//go:build windows

package promotion

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestKillPreflightProcessTerminatesProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	powershellPath := strings.ReplaceAll(filepath.ToSlash(pidFile), "'", "''")
	powershell := fmt.Sprintf("$child=Start-Process -FilePath ping.exe -ArgumentList @('-t','127.0.0.1') -PassThru; Set-Content -NoNewline -Path '%s' -Value $child.Id; Wait-Process -Id $child.Id", powershellPath)
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", powershell)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	cleanup, err := attachPreflightProcess(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("attachPreflightProcess() = %v", err)
	}
	defer cleanup()
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if childPID <= 0 {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("child process did not publish its PID")
	}
	if running, err := processIsRunning(childPID); err != nil || !running {
		t.Fatalf("captured child process was not running before termination: running=%t error=%v", running, err)
	}
	if err := killPreflightProcess(command); err != nil {
		t.Fatalf("killPreflightProcess() = %v", err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("preflight process tree did not terminate")
	}
	running, err := processIsRunning(childPID)
	if err != nil {
		t.Fatalf("verify child process termination: %v", err)
	}
	if running {
		t.Fatalf("child process %d survived preflight process termination", childPID)
	}
}

func processIsRunning(pid int) (bool, error) {
	output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		return false, err
	}
	return strings.Contains(string(output), fmt.Sprintf("\"%d\"", pid)), nil
}
