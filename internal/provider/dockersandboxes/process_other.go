//go:build !windows

package dockersandboxes

import (
	"os/exec"
	"syscall"
)

func isolateKeepaliveProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func isolateManagedProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachManagedProcess(*exec.Cmd, bool) (func(), error) {
	return func() {}, nil
}

func killManagedProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
