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

	"golang.org/x/sys/windows"
)

const (
	preflightProcessTreeHelperEnv    = "EPAR_WINDOWS_PREFLIGHT_PROCESS_TREE_HELPER"
	preflightProcessTreePIDFileEnv   = "EPAR_WINDOWS_PREFLIGHT_PROCESS_TREE_PID_FILE"
	preflightProcessTreeReadyFileEnv = "EPAR_WINDOWS_PREFLIGHT_PROCESS_TREE_READY_FILE"
	preflightProcessTreeDetachedEnv  = "EPAR_WINDOWS_PREFLIGHT_PROCESS_TREE_DETACHED"
	preflightProcessTreeHelperMarker = "preflight-process-ready"
)

func TestKillPreflightProcessTerminatesProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := preflightProcessTreeCommand(pidFile, false)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := 0
	waited := false
	waiting := false
	defer func() {
		if childPID <= 0 {
			childPID = readChildPID(pidFile)
		}
		if childPID > 0 {
			terminateProcess(childPID)
		}
		if !waited && !waiting {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	childPID, err := waitForChildPID(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := attachPreflightProcess(command)
	if err != nil {
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
	waiting = true
	go func() { finished <- command.Wait() }()
	select {
	case <-finished:
		waiting = false
		waited = true
	case <-time.After(5 * time.Second):
		_ = killPreflightProcess(command)
		select {
		case <-finished:
			waiting = false
			waited = true
		case <-time.After(5 * time.Second):
			t.Fatal("preflight process tree did not terminate")
		}
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
	command := preflightProcessOutputCommand()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	isolatePreflightProcess(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	cleanup, err := attachPreflightProcess(command)
	if err != nil {
		t.Fatalf("attachPreflightProcess() = %v", err)
	}
	waitErr := command.Wait()
	waited = true
	if waitErr != nil {
		cleanup()
		t.Fatalf("preflight launcher exited with error: %v", waitErr)
	}
	cleanup()
	if !strings.Contains(stdout.String(), "preflight-process-ready") {
		t.Fatalf("preflight process stdout was not captured: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestPreflightProcessTreeHelper(t *testing.T) {
	switch os.Getenv(preflightProcessTreeHelperEnv) {
	case "":
		return
	case "output-only":
		fmt.Fprintln(os.Stdout, preflightProcessTreeHelperMarker)
	case "child":
		fmt.Fprintln(os.Stdout, "preflight-process-child-ready")
		time.Sleep(30 * time.Second)
	case "parent":
		pidFile := os.Getenv(preflightProcessTreePIDFileEnv)
		readyFile := os.Getenv(preflightProcessTreeReadyFileEnv)
		if pidFile == "" {
			t.Fatal("preflight process tree parent PID file is missing")
		}
		if readyFile == "" {
			t.Fatal("preflight process tree parent ready file is missing")
		}
		child := exec.Command(os.Args[0], "-test.run=^TestPreflightProcessTreeHelper$")
		child.Env = append(os.Environ(), preflightProcessTreeHelperEnv+"=child", preflightProcessTreePIDFileEnv+"="+pidFile, preflightProcessTreeReadyFileEnv+"="+readyFile)
		if err := child.Start(); err != nil {
			t.Fatalf("start preflight process tree child: %v", err)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatalf("publish preflight process tree child PID: %v", err)
		}
		if _, err := fmt.Fprintln(os.Stdout, preflightProcessTreeHelperMarker); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatalf("publish preflight process tree readiness marker: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatalf("publish preflight process tree parent readiness: %v", err)
		}
		if os.Getenv(preflightProcessTreeDetachedEnv) == "1" {
			return
		}
		if err := child.Wait(); err != nil {
			t.Fatalf("wait for preflight process tree child: %v", err)
		}
	default:
		t.Fatalf("unexpected preflight process tree helper mode %q", os.Getenv(preflightProcessTreeHelperEnv))
	}
}

func preflightProcessTreeCommand(pidFile string, detached bool) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestPreflightProcessTreeHelper$")
	readyFile := pidFile + ".ready"
	command.Env = append(os.Environ(), preflightProcessTreeHelperEnv+"=parent", preflightProcessTreePIDFileEnv+"="+pidFile, preflightProcessTreeReadyFileEnv+"="+readyFile)
	if detached {
		command.Env = append(command.Env, preflightProcessTreeDetachedEnv+"=1")
	}
	return command
}

func preflightProcessOutputCommand() *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestPreflightProcessTreeHelper$")
	command.Env = append(os.Environ(), preflightProcessTreeHelperEnv+"=output-only")
	return command
}

func waitForChildPID(pidFile string) (int, error) {
	readyFile := pidFile + ".ready"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			if pid := readChildPID(pidFile); pid > 0 {
				return pid, nil
			}
		} else if !os.IsNotExist(err) {
			return 0, fmt.Errorf("check process tree readiness: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return 0, fmt.Errorf("child process did not publish its PID within 5s")
}

func readChildPID(pidFile string) int {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func processIsRunning(pid int) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return false, nil
		}
		return false, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(process)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process, &exitCode); err != nil {
		return false, fmt.Errorf("get process %d exit code: %w", pid, err)
	}
	return exitCode == 259, nil
}

func terminateProcess(pid int) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
	_ = process.Release()
}
