//go:build windows

package wsl

import (
	"os/exec"
	"syscall"
)

func isolateKeepaliveProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:       true,
		NoInheritHandles: true,
	}
}
