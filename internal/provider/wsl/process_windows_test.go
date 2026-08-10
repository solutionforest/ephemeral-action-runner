//go:build windows

package wsl

import (
	"os/exec"
	"testing"
)

func TestIsolateKeepaliveProcessPreventsControllerLockInheritance(t *testing.T) {
	command := exec.Command("wsl.exe", "--help")
	isolateKeepaliveProcess(command)
	if command.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if !command.SysProcAttr.HideWindow {
		t.Fatal("HideWindow is false")
	}
	if !command.SysProcAttr.NoInheritHandles {
		t.Fatal("NoInheritHandles is false")
	}
}
