package pool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckRunnerPIDFilePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX process signaling")
	}
	script := filepath.ToSlash(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-runner.sh"))
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "actions-runner.pid")

	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_, _ = sleeper.Process.Wait()
	})

	if err := os.WriteFile(pidFile, []byte(fmt.Sprint(sleeper.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"EPAR_DISABLE_SYSTEMD=1",
		"EPAR_RUNNER_PID_FILE="+bashPath(pidFile),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check-runner PID path failed: %v\n%s", err, out)
	}

	if err := sleeper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = sleeper.Process.Wait()

	if err := os.WriteFile(pidFile, []byte(fmt.Sprint(sleeper.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"EPAR_DISABLE_SYSTEMD=1",
		"EPAR_RUNNER_PID_FILE="+bashPath(pidFile),
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("check-runner accepted stale PID")
	}
}

func TestCheckRunnerSystemdPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX systemd command simulation")
	}
	script := filepath.ToSlash(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-runner.sh"))
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
		"EPAR_SYSTEMCTL_ARGS_FILE="+bashPath(argsFile),
		"PATH="+bashPath(dir)+":"+os.Getenv("PATH"),
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

func bashPath(path string) string {
	path = filepath.ToSlash(path)
	if len(path) >= 2 && path[1] == ':' {
		return "/" + strings.ToLower(path[:1]) + path[2:]
	}
	return path
}

func TestRunRunnerUsesSourceImageEnvWrapper(t *testing.T) {
	runRunnerPath := filepath.Join("..", "..", "scripts", "guest", "ubuntu", "run-runner.sh")
	runRunner, err := os.ReadFile(runRunnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runRunner), "/opt/epar/start-runner-with-env.sh"); got != 2 {
		t.Fatalf("run-runner wrapper references = %d, want 2\n%s", got, runRunner)
	}

	wrapperPath := filepath.Join("..", "..", "scripts", "guest", "ubuntu", "start-runner-with-env.sh")
	wrapper, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/opt/epar/source-image.env", "set -a", "exec ./run.sh"} {
		if !strings.Contains(string(wrapper), want) {
			t.Fatalf("wrapper missing %q:\n%s", want, wrapper)
		}
	}
}
