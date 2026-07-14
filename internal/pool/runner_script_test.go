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
	pidStartFile := filepath.Join(dir, "actions-runner.pid.start")
	runnerWorkDir := t.TempDir()

	sleeper := exec.Command("sleep", "60")
	sleeper.Dir = runnerWorkDir
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
	startTime := linuxProcessStartTime(t, sleeper.Process.Pid)
	if err := os.WriteFile(pidStartFile, []byte(startTime), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"EPAR_DISABLE_SYSTEMD=1",
		"EPAR_RUNNER_PID_FILE="+bashPath(pidFile),
		"EPAR_RUNNER_PID_START_FILE="+bashPath(pidStartFile),
		"EPAR_RUNNER_WORK_DIR="+bashPath(runnerWorkDir),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check-runner PID path failed: %v\n%s", err, out)
	}

	if err := os.WriteFile(pidStartFile, []byte(startTime+"0"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"EPAR_DISABLE_SYSTEMD=1",
		"EPAR_RUNNER_PID_FILE="+bashPath(pidFile),
		"EPAR_RUNNER_PID_START_FILE="+bashPath(pidStartFile),
		"EPAR_RUNNER_WORK_DIR="+bashPath(runnerWorkDir),
	)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("check-runner accepted stale start marker for live PID %d", sleeper.Process.Pid)
	} else if !strings.Contains(string(out), "does not match stored start time") {
		t.Fatalf("check-runner stale marker error missing identity details: %s", out)
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
		"EPAR_RUNNER_PID_START_FILE="+bashPath(pidStartFile),
		"EPAR_RUNNER_WORK_DIR="+bashPath(runnerWorkDir),
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("check-runner accepted stale PID")
	}
}

func TestCheckRunnerRejectsLivePIDWithUnexpectedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX process signaling")
	}
	script := filepath.ToSlash(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-runner.sh"))
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "actions-runner.pid")
	pidStartFile := filepath.Join(dir, "actions-runner.pid.start")
	expectedWorkDir := t.TempDir()
	unrelatedWorkDir := t.TempDir()

	sleeper := exec.Command("sleep", "60")
	sleeper.Dir = unrelatedWorkDir
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
	if err := os.WriteFile(pidStartFile, []byte(linuxProcessStartTime(t, sleeper.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"EPAR_DISABLE_SYSTEMD=1",
		"EPAR_RUNNER_PID_FILE="+bashPath(pidFile),
		"EPAR_RUNNER_PID_START_FILE="+bashPath(pidStartFile),
		"EPAR_RUNNER_WORK_DIR="+bashPath(expectedWorkDir),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check-runner accepted unrelated live PID %d", sleeper.Process.Pid)
	}
	if !strings.Contains(string(out), "does not match runner work directory") {
		t.Fatalf("check-runner cwd mismatch error missing identity details: %s", out)
	}
}

func TestCheckRunnerRejectsZombiePID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX process state")
	}
	script := filepath.ToSlash(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-runner.sh"))
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "actions-runner.pid")
	pidStartFile := filepath.Join(dir, "actions-runner.pid.start")

	exited := exec.Command("sh", "-c", "exit 0")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = exited.Process.Wait() })
	if err := os.WriteFile(pidFile, []byte(fmt.Sprint(exited.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-p", fmt.Sprint(exited.Process.Pid), "-o", "stat=").Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(pidStartFile, []byte(linuxProcessStartTime(t, exited.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"EPAR_DISABLE_SYSTEMD=1",
		"EPAR_RUNNER_PID_FILE="+bashPath(pidFile),
		"EPAR_RUNNER_PID_START_FILE="+bashPath(pidStartFile),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check-runner accepted zombie PID %d", exited.Process.Pid)
	}
	if !strings.Contains(string(out), "is a zombie") {
		t.Fatalf("check-runner zombie error missing useful state: %s", out)
	}
}

func TestCheckRunnerSystemdPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX systemd command simulation")
	}
	script := filepath.ToSlash(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-runner.sh"))
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "systemctl.args")
	pidStartFile := filepath.Join(dir, "actions-runner.pid.start")
	systemctl := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(systemctl, []byte(`#!/usr/bin/env bash
printf '%s\n' "$*" >>"${EPAR_SYSTEMCTL_ARGS_FILE}"
if [[ "$1" == "show" ]]; then
  echo "${EPAR_SYSTEMD_MAIN_PID}"
fi
exit 0
`), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	runnerWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidStartFile, []byte(linuxProcessStartTime(t, os.Getpid())), 0644); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(),
		"EPAR_FORCE_SYSTEMD=1",
		"EPAR_SYSTEMD_MAIN_PID="+fmt.Sprint(os.Getpid()),
		"EPAR_SYSTEMCTL_ARGS_FILE="+bashPath(argsFile),
		"EPAR_RUNNER_PID_START_FILE="+bashPath(pidStartFile),
		"EPAR_RUNNER_WORK_DIR="+bashPath(runnerWorkDir),
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
	if args != "is-active --quiet actions-runner.service\nshow actions-runner.service --property=MainPID --value" {
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

func linuxProcessStartTime(t *testing.T, pid int) string {
	t.Helper()
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	closing := strings.LastIndex(text, ") ")
	if closing < 0 {
		t.Fatalf("process %d stat has no command terminator: %q", pid, text)
	}
	fields := strings.Fields(text[closing+2:])
	if len(fields) < 20 {
		t.Fatalf("process %d stat has %d post-command fields, want at least 20", pid, len(fields))
	}
	return fields[19]
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
	for _, want := range []string{"/opt/epar/source-image.env", "/opt/epar/apply-trusted-ca-runtime.sh", "set -a", "exec ./run.sh"} {
		if !strings.Contains(string(wrapper), want) {
			t.Fatalf("wrapper missing %q:\n%s", want, wrapper)
		}
	}
	sourceImageEnvIndex := strings.Index(string(wrapper), "/opt/epar/source-image.env")
	trustedCARuntimeIndex := strings.Index(string(wrapper), "/opt/epar/apply-trusted-ca-runtime.sh")
	execIndex := strings.Index(string(wrapper), "exec ./run.sh")
	if !(sourceImageEnvIndex < trustedCARuntimeIndex && trustedCARuntimeIndex < execIndex) {
		t.Fatalf("wrapper source order is wrong: source-image.env=%d trusted-CA=%d exec=%d\n%s", sourceImageEnvIndex, trustedCARuntimeIndex, execIndex, wrapper)
	}
}

func TestApplyTrustedCARuntimeNoOpsWithoutCurrentManifestCertificate(t *testing.T) {
	output := runTrustedCARuntimeHook(t, `
set -euo pipefail
manifest='{"manifest":{}}'
grep() {
  if [[ "$*" == *trustedCaCertificates* ]]; then
    printf '%s\n' "$manifest" | command grep "$1" "$2"
    return
  fi
  command grep "$@"
}
compgen() { return 0; }
unset NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE PIP_CERT
. "$1"
printf '%s|%s|%s\n' "${NODE_EXTRA_CA_CERTS+x}" "${REQUESTS_CA_BUNDLE+x}" "${PIP_CERT+x}"
`)
	if got := strings.TrimSpace(output); got != "||" {
		t.Fatalf("trusted-CA hook activated without a current manifest certificate: %q", got)
	}
}

func TestApplyTrustedCARuntimeNoOpsWithoutInstalledCertificate(t *testing.T) {
	output := runTrustedCARuntimeHook(t, `
set -euo pipefail
manifest='{"manifest":{"trustedCaCertificates":[{"path":"enterprise-root.pem"}]}}'
grep() {
  if [[ "$*" == *trustedCaCertificates* ]]; then
    printf '%s\n' "$manifest" | command grep "$1" "$2"
    return
  fi
  command grep "$@"
}
compgen() { return 1; }
unset NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE PIP_CERT
. "$1"
printf '%s|%s|%s\n' "${NODE_EXTRA_CA_CERTS+x}" "${REQUESTS_CA_BUNDLE+x}" "${PIP_CERT+x}"
`)
	if got := strings.TrimSpace(output); got != "||" {
		t.Fatalf("trusted-CA hook activated without an installed certificate: %q", got)
	}
}

func TestApplyTrustedCARuntimeDefaultsAndExports(t *testing.T) {
	output := runTrustedCARuntimeHook(t, `
set -euo pipefail
manifest='{"manifest":{"trustedCaCertificates":[{"path":"enterprise-root.pem"}]}}'
grep() {
  if [[ "$*" == *trustedCaCertificates* ]]; then
    printf '%s\n' "$manifest" | command grep "$1" "$2"
    return
  fi
  command grep "$@"
}
compgen() { return 0; }
unset NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE PIP_CERT
. "$1"
env | grep -E '^(NODE_EXTRA_CA_CERTS|REQUESTS_CA_BUNDLE|PIP_CERT)='
`)
	want := map[string]string{
		"NODE_EXTRA_CA_CERTS": "/etc/ssl/certs/ca-certificates.crt",
		"REQUESTS_CA_BUNDLE":  "/etc/ssl/certs/ca-certificates.crt",
		"PIP_CERT":            "/etc/ssl/certs/ca-certificates.crt",
	}
	got := parseEnvironment(t, output)
	if len(got) != len(want) {
		t.Fatalf("active trusted-CA hook exported %v, want %v", got, want)
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("active trusted-CA hook %s = %q, want %q", key, got[key], wantValue)
		}
	}
}

func TestApplyTrustedCARuntimeActivatesForHostTrustOverlay(t *testing.T) {
	output := runTrustedCARuntimeHook(t, `
set -euo pipefail
manifest='{"manifest":{"hostTrust":{"mode":"overlay","generation":"g1"}}}'
grep() { printf '%s\n' "$manifest" | command grep "$1" "$2"; }
compgen() { return 0; }
unset NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE PIP_CERT
. "$1"
printf '%s|%s|%s\n' "$NODE_EXTRA_CA_CERTS" "$REQUESTS_CA_BUNDLE" "$PIP_CERT"
`)
	if got := strings.TrimSpace(output); got != "/etc/ssl/certs/ca-certificates.crt|/etc/ssl/certs/ca-certificates.crt|/etc/ssl/certs/ca-certificates.crt" {
		t.Fatalf("host-trust runtime hook values = %q", got)
	}
}

func TestHostTrustGenerationHookAcceptsCurrentLease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the guest hook requires the Linux image's python3 runtime")
	}
	marker := `{"schemaVersion":1,"generation":"g1","hostOS":"windows","mode":"overlay","scopes":["system","user"]}`
	lease := fmt.Sprintf(`{"schemaVersion":1,"generation":"g1","hostOS":"windows","mode":"overlay","scopes":["system","user"],"expiresAt":%q}`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))
	output, err := runHostTrustGenerationHook(t, marker, lease)
	if err != nil {
		t.Fatalf("current host-trust lease rejected: %v\n%s", err, output)
	}
	if !strings.Contains(output, "generation and lease are current") {
		t.Fatalf("host-trust hook output = %q", output)
	}
}

func TestHostTrustGenerationHookRejectsMismatchAndExpiry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the guest hook requires the Linux image's python3 runtime")
	}
	marker := `{"schemaVersion":1,"generation":"g1","hostOS":"linux","mode":"overlay","scopes":["system"]}`
	for _, tc := range []struct {
		name, lease, want string
	}{
		{name: "generation mismatch", lease: fmt.Sprintf(`{"generation":"g2","hostOS":"linux","mode":"overlay","scopes":["system"],"expiresAt":%q}`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)), want: "generation mismatch"},
		{name: "expired", lease: fmt.Sprintf(`{"generation":"g1","hostOS":"linux","mode":"overlay","scopes":["system"],"expiresAt":%q}`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)), want: "lease expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runHostTrustGenerationHook(t, marker, tc.lease)
			if err == nil {
				t.Fatalf("host-trust hook accepted invalid lease: %s", output)
			}
			if !strings.Contains(strings.ToLower(output), tc.want) {
				t.Fatalf("host-trust hook output = %q, want %q", output, tc.want)
			}
		})
	}
}

func runHostTrustGenerationHook(t *testing.T, marker, lease string) (string, error) {
	t.Helper()
	hookPath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "check-host-trust-generation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker.json")
	leasePath := filepath.Join(dir, "lease.json")
	if err := os.WriteFile(markerPath, []byte(marker), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, []byte(lease), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gitBashForRunnerScriptTest(t), bashPath(hookPath))
	cmd.Env = append(os.Environ(), "EPAR_HOST_TRUST_MARKER="+bashPath(markerPath), "EPAR_HOST_TRUST_LEASE="+bashPath(leasePath))
	output, runErr := cmd.CombinedOutput()
	return string(output), runErr
}

func TestApplyTrustedCARuntimePreservesExistingValues(t *testing.T) {
	output := runTrustedCARuntimeHook(t, `
set -euo pipefail
manifest='{"manifest":{"trustedCaCertificates":[{"path":"enterprise-root.pem"}]}}'
grep() {
  if [[ "$*" == *trustedCaCertificates* ]]; then
    printf '%s\n' "$manifest" | command grep "$1" "$2"
    return
  fi
  command grep "$@"
}
compgen() { return 0; }
export NODE_EXTRA_CA_CERTS=/custom/node.pem
export REQUESTS_CA_BUNDLE=/custom/requests.pem
export PIP_CERT=/custom/pip.pem
. "$1"
printf '%s|%s|%s\n' "$NODE_EXTRA_CA_CERTS" "$REQUESTS_CA_BUNDLE" "$PIP_CERT"
`)
	if got := strings.TrimSpace(output); got != "/custom/node.pem|/custom/requests.pem|/custom/pip.pem" {
		t.Fatalf("active trusted-CA hook overwrote configured values: %q", got)
	}
}

func TestApplyTrustedCARuntimePreservesExplicitEmptyValues(t *testing.T) {
	output := runTrustedCARuntimeHook(t, `
set -euo pipefail
manifest='{"manifest":{"trustedCaCertificates":[{"path":"enterprise-root.pem"}]}}'
grep() {
  if [[ "$*" == *trustedCaCertificates* ]]; then
    printf '%s\n' "$manifest" | command grep "$1" "$2"
    return
  fi
  command grep "$@"
}
compgen() { return 0; }
export NODE_EXTRA_CA_CERTS=''
export REQUESTS_CA_BUNDLE=''
export PIP_CERT=''
. "$1"
printf '<%s>|<%s>|<%s>\n' "$NODE_EXTRA_CA_CERTS" "$REQUESTS_CA_BUNDLE" "$PIP_CERT"
`)
	if got := strings.TrimSpace(output); got != "<>|<>|<>" {
		t.Fatalf("active trusted-CA hook overwrote explicit empty values: %q", got)
	}
}

func runTrustedCARuntimeHook(t *testing.T, command string) string {
	t.Helper()
	hookPath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "apply-trusted-ca-runtime.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gitBashForRunnerScriptTest(t), "-c", command, "bash", bashPath(hookPath))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trusted-CA runtime hook failed: %v\n%s", err, output)
	}
	return string(output)
}

func gitBashForRunnerScriptTest(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		const gitBash = `C:\Program Files\Git\bin\bash.exe`
		if _, err := os.Stat(gitBash); err != nil {
			t.Skipf("Git Bash is required for shell hook tests: %v", err)
		}
		return gitBash
	}
	return "bash"
}

func parseEnvironment(t *testing.T, output string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid environment line %q", line)
		}
		result[key] = value
	}
	return result
}

func TestRunRunnerValidatesListenerStartup(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "guest", "ubuntu", "run-runner.sh")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, want := range []string{
		`systemctl is-active --quiet "${unit}"`,
		`--property=MainPID --value`,
		`validate_pid "${pid}"`,
		`[[ "${state}" == Z* ]]`,
		`readlink -f "/proc/${pid}/cwd"`,
		`EPAR_RUNNER_WORK_DIR:-/opt/actions-runner`,
		`EPAR_RUNNER_PID_START_FILE:-${pid_file}.start`,
		`printf '%s\n' "${main_pid}" >"${pid_file}"`,
		`record_pid_start "${main_pid}"`,
		`record_pid_start "${pid}"`,
		`tail -n 80 "${log_file}"`,
	} {
		if !strings.Contains(contents, want) {
			t.Fatalf("run-runner.sh missing %q:\n%s", want, contents)
		}
	}
}

func TestCollectRunnerDiagnosticsIsBoundedAndTextual(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "guest", "ubuntu", "collect-runner-diagnostics.sh")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, want := range []string{
		`/var/log/actions-runner/run.log`,
		`/opt/actions-runner/_diag/Runner_*.log`,
		`/var/log/epar-dockerd.log`,
		`ps -p "${pid}" -o pid=,ppid=,stat=,etime=,cmd=`,
		`stored_start=`,
		`current_start=`,
		`tail -n "${tail_lines}"`,
	} {
		if !strings.Contains(contents, want) {
			t.Fatalf("collect-runner-diagnostics.sh missing %q:\n%s", want, contents)
		}
	}
	for _, forbidden := range []string{"tar ", "upload", "base64"} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("collect-runner-diagnostics.sh contains forbidden raw artifact behavior %q", forbidden)
		}
	}
}

func TestCollectRunnerDiagnosticsValidatesTailLines(t *testing.T) {
	script := filepath.ToSlash(filepath.Join("..", "..", "scripts", "guest", "ubuntu", "collect-runner-diagnostics.sh"))

	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "invalid", value: "many", want: "last 50 lines"},
		{name: "zero", value: "0", want: "last 50 lines"},
		{name: "positive", value: "7", want: "last 7 lines"},
		{name: "capped", value: "999", want: "last 200 lines"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := fmt.Sprintf("EPAR_DIAGNOSTIC_TAIL_LINES=%s exec bash %s", tc.value, script)
			cmd := exec.Command("bash", "-c", command)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("collect-runner-diagnostics.sh failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("diagnostic output for %q missing %q:\n%s", tc.value, tc.want, out)
			}
		})
	}
}

func TestConfigureRunnerSupportsGroupAndNoDefaultLabels(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "guest", "ubuntu", "configure-runner.sh")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, want := range []string{
		`RUNNER_GROUP="${RUNNER_GROUP:-}"`,
		`RUNNER_NO_DEFAULT_LABELS="${RUNNER_NO_DEFAULT_LABELS:-false}"`,
		`args+=(--runnergroup "${RUNNER_GROUP}")`,
		`args+=(--no-default-labels)`,
	} {
		if !strings.Contains(contents, want) {
			t.Fatalf("configure-runner.sh missing %q:\n%s", want, contents)
		}
	}
}
