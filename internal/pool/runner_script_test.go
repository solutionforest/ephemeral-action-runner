package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCheckRunnerPIDFilePath(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-runner.sh")
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleep.Process.Kill()
		_, _ = sleep.Process.Wait()
	}()

	pidFile := filepath.Join(t.TempDir(), "actions-runner.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(sleep.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "EPAR_DISABLE_SYSTEMD=1", "EPAR_RUNNER_PID_FILE="+pidFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check-runner PID path failed: %v\n%s", err, out)
	}

	if err := os.WriteFile(pidFile, []byte("999999"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "EPAR_DISABLE_SYSTEMD=1", "EPAR_RUNNER_PID_FILE="+pidFile)
	if err := cmd.Run(); err == nil {
		t.Fatal("check-runner accepted stale PID")
	}
}

func TestCheckRunnerSystemdPath(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-runner.sh")
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "systemctl.args")
	systemctl := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(systemctl, []byte(`#!/usr/bin/env bash
printf '%s\n' "$*" >"${EPAR_SYSTEMCTL_ARGS_FILE}"
exit 0
`), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"EPAR_FORCE_SYSTEMD=1",
		"EPAR_SYSTEMCTL_ARGS_FILE="+argsFile,
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check-runner systemd path failed: %v\n%s", err, out)
	}

	deadline := time.Now().Add(time.Second)
	var args string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(argsFile)
		if err == nil {
			args = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if args != "is-active --quiet actions-runner.service" {
		t.Fatalf("systemctl args = %q", args)
	}
}
