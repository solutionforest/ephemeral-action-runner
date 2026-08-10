//go:build !windows

package dockersandboxes

import "os/exec"

func isolateKeepaliveProcess(*exec.Cmd) {}
