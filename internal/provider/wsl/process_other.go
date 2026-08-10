//go:build !windows

package wsl

import "os/exec"

func isolateKeepaliveProcess(*exec.Cmd) {}
