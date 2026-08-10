//go:build windows

package hosttrust

import (
	"os"
	"os/exec"
	"testing"
)

func TestPlatformProcessAliveUsesProcessSnapshot(t *testing.T) {
	if !platformProcessAlive(os.Getpid()) {
		t.Fatal("current process was not reported alive")
	}

	command := exec.Command("cmd.exe", "/d", "/c", "exit", "0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if platformProcessAlive(pid) {
		t.Fatalf("exited process %d was reported alive", pid)
	}
}
