//go:build !windows

package promotion

import (
	"os/exec"
	"syscall"
)

func isolatePreflightProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachPreflightProcess(*exec.Cmd) (func(), error) {
	return func() {}, nil
}

func killPreflightProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
