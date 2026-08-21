//go:build windows

package dockersandboxes

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
	managedProcessTreeHelperEnv      = "EPAR_WINDOWS_MANAGED_PROCESS_TREE_HELPER"
	managedProcessTreePIDFileEnv     = "EPAR_WINDOWS_MANAGED_PROCESS_TREE_PID_FILE"
	managedProcessTreeReadyFileEnv   = "EPAR_WINDOWS_MANAGED_PROCESS_TREE_READY_FILE"
	managedProcessTreeDetachedEnv    = "EPAR_WINDOWS_MANAGED_PROCESS_TREE_DETACHED"
	managedProcessTreeHelperMarker   = "managed-process-ready"
	managedProcessTreeDetachedMarker = "detached-process-ready"
)

func TestKillManagedProcessTerminatesProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := managedProcessTreeCommand(pidFile, false)
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
	cleanup, err := attachManagedProcess(command, false)
	if err != nil {
		t.Fatalf("attachManagedProcess() = %v", err)
	}
	defer cleanup()
	if running, err := processIsRunning(childPID); err != nil || !running {
		t.Fatalf("captured child process was not running before termination: running=%t error=%v", running, err)
	}
	if err := killManagedProcess(command); err != nil {
		t.Fatalf("killManagedProcess() = %v", err)
	}
	finished := make(chan error, 1)
	waiting = true
	go func() { finished <- command.Wait() }()
	select {
	case <-finished:
		waiting = false
		waited = true
	case <-time.After(5 * time.Second):
		_ = killManagedProcess(command)
		select {
		case <-finished:
			waiting = false
			waited = true
		case <-time.After(5 * time.Second):
			t.Fatal("managed process tree did not terminate after taskkill")
		}
	}
	running, err := processIsRunning(childPID)
	if err != nil {
		t.Fatalf("verify child process termination: %v", err)
	}
	if running {
		t.Fatalf("child process %d survived managed process termination", childPID)
	}
	if !strings.Contains(stdout.String(), "managed-process-ready") {
		t.Fatalf("managed process stdout was not captured: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestAttachManagedProcessPreservesDetachedDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := managedProcessTreeCommand(pidFile, true)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	isolateManagedProcess(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := 0
	waited := false
	defer func() {
		if childPID <= 0 {
			childPID = readChildPID(pidFile)
		}
		if childPID > 0 {
			terminateProcess(childPID)
		}
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	cleanup, err := attachManagedProcess(command, true)
	if err != nil {
		t.Fatalf("attachManagedProcess(preserve) = %v", err)
	}
	childPID, err = waitForChildPID(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	waitErr := command.Wait()
	waited = true
	if waitErr != nil {
		cleanup()
		t.Fatalf("detached launcher exited with error: %v", waitErr)
	}
	cleanup()
	if !strings.Contains(stdout.String(), "detached-process-ready") {
		t.Fatalf("managed process stdout was not captured: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	running, err := processIsRunning(childPID)
	if err != nil {
		t.Fatalf("verify detached child process: %v", err)
	}
	if !running {
		t.Fatalf("detached child process %d was terminated when the containment job closed", childPID)
	}
}

func TestManagedProcessTreeHelper(t *testing.T) {
	switch os.Getenv(managedProcessTreeHelperEnv) {
	case "":
		return
	case "child":
		fmt.Fprintln(os.Stdout, "managed-process-child-ready")
		time.Sleep(30 * time.Second)
	case "parent":
		pidFile := os.Getenv(managedProcessTreePIDFileEnv)
		readyFile := os.Getenv(managedProcessTreeReadyFileEnv)
		if pidFile == "" {
			t.Fatal("managed process tree parent PID file is missing")
		}
		if readyFile == "" {
			t.Fatal("managed process tree parent ready file is missing")
		}
		child := exec.Command(os.Args[0], "-test.run=^TestManagedProcessTreeHelper$")
		child.Env = append(os.Environ(), managedProcessTreeHelperEnv+"=child", managedProcessTreePIDFileEnv+"="+pidFile, managedProcessTreeReadyFileEnv+"="+readyFile)
		if err := child.Start(); err != nil {
			t.Fatalf("start managed process tree child: %v", err)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatalf("publish managed process tree child PID: %v", err)
		}
		marker := managedProcessTreeHelperMarker
		if os.Getenv(managedProcessTreeDetachedEnv) == "1" {
			marker = managedProcessTreeDetachedMarker
		}
		if _, err := fmt.Fprintln(os.Stdout, marker); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatalf("publish managed process tree readiness marker: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatalf("publish managed process tree parent readiness: %v", err)
		}
		if os.Getenv(managedProcessTreeDetachedEnv) == "1" {
			return
		}
		if err := child.Wait(); err != nil {
			t.Fatalf("wait for managed process tree child: %v", err)
		}
	default:
		t.Fatalf("unexpected managed process tree helper mode %q", os.Getenv(managedProcessTreeHelperEnv))
	}
}

func managedProcessTreeCommand(pidFile string, detached bool) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestManagedProcessTreeHelper$")
	readyFile := pidFile + ".ready"
	command.Env = append(os.Environ(), managedProcessTreeHelperEnv+"=parent", managedProcessTreePIDFileEnv+"="+pidFile, managedProcessTreeReadyFileEnv+"="+readyFile)
	if detached {
		command.Env = append(command.Env, managedProcessTreeDetachedEnv+"=1")
	}
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
