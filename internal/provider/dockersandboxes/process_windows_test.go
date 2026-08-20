//go:build windows

package dockersandboxes

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

func TestKillManagedProcessTerminatesProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	powershellPath := strings.ReplaceAll(filepath.ToSlash(pidFile), "'", "''")
	powershell := fmt.Sprintf("$child=Start-Process -FilePath ping.exe -ArgumentList @('-t','127.0.0.1') -PassThru; Set-Content -NoNewline -Path '%s' -Value $child.Id; Wait-Process -Id $child.Id", powershellPath)
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", powershell)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	cleanup, err := attachManagedProcess(command, false)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("attachManagedProcess() = %v", err)
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
	if err := killManagedProcess(command); err != nil {
		t.Fatalf("killManagedProcess() = %v", err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("managed process tree did not terminate after taskkill")
	}
	running, err := processIsRunning(childPID)
	if err != nil {
		t.Fatalf("verify child process termination: %v", err)
	}
	if running {
		t.Fatalf("child process %d survived managed process termination", childPID)
	}
}

func TestAttachManagedProcessPreservesDetachedDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	powershellPath := strings.ReplaceAll(filepath.ToSlash(pidFile), "'", "''")
	powershell := fmt.Sprintf("$child=Start-Process -FilePath ping.exe -ArgumentList @('-t','127.0.0.1') -PassThru; Set-Content -NoNewline -Path '%s' -Value $child.Id; exit 0", powershellPath)
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", powershell)
	isolateManagedProcess(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	cleanup, err := attachManagedProcess(command, true)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("attachManagedProcess(preserve) = %v", err)
	}
	if err := command.Wait(); err != nil {
		cleanup()
		t.Fatalf("detached launcher exited with error: %v", err)
	}
	cleanup()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid detached child pid %q: %v", data, err)
	}
	defer func() { _ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(childPID)).Run() }()
	running, err := processIsRunning(childPID)
	if err != nil {
		t.Fatalf("verify detached child process: %v", err)
	}
	if !running {
		t.Fatalf("detached child process %d was terminated when the containment job closed", childPID)
	}
}

func processIsRunning(pid int) (bool, error) {
	output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		return false, err
	}
	return strings.Contains(string(output), fmt.Sprintf("\"%d\"", pid)), nil
}
