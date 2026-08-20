//go:build windows

package dockersandboxes

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func isolateKeepaliveProcess(command *exec.Cmd) {
	// Leave handle inheritance enabled so os/exec can pass the command's
	// stdin/stdout/stderr handles to the child. On Windows, os/exec supplies an
	// explicit PROC_THREAD_ATTRIBUTE_HANDLE_LIST, so unrelated inheritable
	// handles are still excluded. NoInheritHandles would suppress that list as
	// well and break EPAR's output capture (and some child process startups).
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED}
}

func isolateManagedProcess(command *exec.Cmd) {
	// Keep the standard-handle list that os/exec builds; see the keepalive
	// process comment above.
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED}
}

var managedProcessJobs sync.Map

func attachManagedProcess(command *exec.Cmd, preserveDescendantsOnSuccess bool) (func(), error) {
	if command.Process == nil {
		return nil, fmt.Errorf("managed process has not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create Windows process job: %w", err)
	}
	closeJob := func() { _ = windows.CloseHandle(job) }
	if !preserveDescendantsOnSuccess {
		var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
			closeJob()
			return nil, fmt.Errorf("configure Windows process job: %w", err)
		}
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		closeJob()
		return nil, fmt.Errorf("open managed process for Windows job: %w", err)
	}
	assignErr := windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if assignErr != nil {
		closeJob()
		return nil, fmt.Errorf("assign managed process to Windows job: %w", assignErr)
	}
	if err := assignExistingDescendantsToJob(job, command.Process.Pid); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		closeJob()
		return nil, fmt.Errorf("assign pre-existing Docker Sandboxes descendants to Windows job: %w", err)
	}
	if managedProcessWasCreatedSuspended(command) {
		if err := resumeManagedProcess(command); err != nil {
			_ = windows.TerminateJobObject(job, 1)
			closeJob()
			return nil, fmt.Errorf("resume Docker Sandboxes process after containment: %w", err)
		}
	}
	managedProcessJobs.Store(command.Process.Pid, job)
	var once sync.Once
	return func() {
		once.Do(func() {
			managedProcessJobs.Delete(command.Process.Pid)
			closeJob()
		})
	}, nil
}

func managedProcessWasCreatedSuspended(command *exec.Cmd) bool {
	return command.SysProcAttr != nil && command.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED != 0
}

func resumeManagedProcess(command *exec.Cmd) error {
	threadID, err := mainThreadID(command.Process.Pid)
	if err != nil {
		return err
	}
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, threadID)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(thread)
	_, err = windows.ResumeThread(thread)
	return err
}

func mainThreadID(processID int) (uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return 0, err
	}
	for {
		if entry.OwnerProcessID == uint32(processID) {
			return entry.ThreadID, nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return 0, err
		}
	}
	return 0, fmt.Errorf("main thread for process %d was not found", processID)
}

func assignExistingDescendantsToJob(job windows.Handle, rootPID int) error {
	assigned := map[uint32]bool{uint32(rootPID): true}
	for pass := 0; pass < 3; pass++ {
		descendants, err := descendantProcessIDs(rootPID)
		if err != nil {
			return err
		}
		for _, pid := range descendants {
			if assigned[pid] {
				continue
			}
			process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
			if err != nil {
				return fmt.Errorf("open descendant process %d: %w", pid, err)
			}
			assignErr := windows.AssignProcessToJobObject(job, process)
			_ = windows.CloseHandle(process)
			if assignErr != nil {
				return fmt.Errorf("assign descendant process %d: %w", pid, assignErr)
			}
			assigned[pid] = true
		}
		if pass < 2 {
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

func descendantProcessIDs(rootPID int) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	children := make(map[uint32][]uint32)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	for {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, err
		}
	}
	seen := map[uint32]bool{uint32(rootPID): true}
	queue := []uint32{uint32(rootPID)}
	var descendants []uint32
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if seen[child] {
				continue
			}
			seen[child] = true
			descendants = append(descendants, child)
			queue = append(queue, child)
		}
	}
	return descendants, nil
}

func killManagedProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if value, ok := managedProcessJobs.Load(command.Process.Pid); ok {
		if err := windows.TerminateJobObject(value.(windows.Handle), 1); err == nil {
			return nil
		}
	}
	taskkill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid))
	if err := taskkill.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- taskkill.Wait()
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = taskkill.Process.Kill()
		return fmt.Errorf("taskkill did not finish within 2s")
	}
}
