//go:build windows

package promotion

import (
	"bytes"
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
	powershell := fmt.Sprintf("$child=Start-Process -FilePath ping.exe -ArgumentList @('-t','127.0.0.1') -PassThru; Write-Output 'preflight-process-ready'; Set-Content -NoNewline -Path '%s' -Value $child.Id; Wait-Process -Id $child.Id", powershellPath)
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", powershell)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	childPID, err := waitForChildPID(pidFile)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	cleanup, err := attachPreflightProcess(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("attachPreflightProcess() = %v", err)
	}
	defer cleanup()
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
	if !strings.Contains(stdout.String(), "preflight-process-ready") {
		t.Fatalf("preflight process stdout was not captured: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestPreflightProcessCapturesStdout(t *testing.T) {
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Write-Output 'preflight-process-ready'")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	isolatePreflightProcess(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	cleanup, err := attachPreflightProcess(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("attachPreflightProcess() = %v", err)
	}
	if err := command.Wait(); err != nil {
		cleanup()
		t.Fatalf("preflight launcher exited with error: %v", err)
	}
	cleanup()
	if !strings.Contains(stdout.String(), "preflight-process-ready") {
		t.Fatalf("preflight process stdout was not captured: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func waitForChildPID(pidFile string) (int, error) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return 0, fmt.Errorf("child process did not publish its PID within 5s")
}

func processIsRunning(pid int) (bool, error) {
	output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		return false, err
	}
	return strings.Contains(string(output), fmt.Sprintf("\"%d\"", pid)), nil
}
