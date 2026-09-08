package image

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const expectedDockerSandboxesSSHAgentNormalization = `if [[ "${SSH_AUTH_SOCK:-}" == /run/ssh-agent.sock && -x /run && ! -e /run/ssh-agent.sock && ! -L /run/ssh-agent.sock && -z "${SSH_AUTH_SOCK_GATEWAY:-}" && -z "${SSH_AGENT_PID:-}" ]]; then
  unset SSH_AUTH_SOCK
fi`

type dockerSandboxesSSHAgentBlock struct {
	normalization string
	guard         string
}

type dockerSandboxesSSHAgentCase struct {
	name          string
	endpoint      string
	sock          string
	gateway       string
	pid           string
	setSock       bool
	setGateway    bool
	setPID        bool
	wantSuccess   bool
	wantSockState string
}

func TestDockerSandboxesSSHAgentGuardsAreConsistentAndExecutable(t *testing.T) {
	bash := requireDockerSandboxesSSHAgentBash(t)
	root := dockerSandboxesSSHAgentRepoRoot(t)
	sources := []struct {
		name             string
		path             string
		failureSignature string
	}{
		{name: "direct workspace verification", path: filepath.Join(root, "internal", "provider", "dockersandboxes", "provider.go"), failureSignature: "Docker Sandboxes exposed host SSH-agent forwarding"},
		{name: "template entrypoint", path: filepath.Join(root, "templates", "docker-sandboxes", "guest", "template-entrypoint.sh"), failureSignature: "host SSH-agent forwarding is not permitted"},
		{name: "template verification", path: filepath.Join(root, "templates", "docker-sandboxes", "guest", "verify-template.sh")},
	}

	blocks := make([]dockerSandboxesSSHAgentBlock, 0, len(sources))
	for _, source := range sources {
		content, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		block, err := extractDockerSandboxesSSHAgentBlock(string(content))
		if err != nil {
			t.Fatalf("extract %s: %v", source.name, err)
		}
		if block.normalization != expectedDockerSandboxesSSHAgentNormalization {
			t.Fatalf("%s normalization = %q, want %q", source.name, block.normalization, expectedDockerSandboxesSSHAgentNormalization)
		}
		assertDockerSandboxesSSHAgentGuardContract(t, block.guard)
		blocks = append(blocks, block)
	}
	for index := 1; index < len(blocks); index++ {
		if blocks[index].normalization != blocks[0].normalization {
			t.Fatalf("normalization differs between direct workspace verification and %s", sources[index].name)
		}
	}
	entrypointPredicate := strings.SplitN(blocks[1].guard, "\n", 2)[0]
	verificationPredicate := strings.SplitN(blocks[2].guard, "\n", 2)[0]
	if entrypointPredicate != verificationPredicate {
		t.Fatalf("template entrypoint and template verification SSH-agent predicates differ:\nentrypoint: %s\nverification: %s", entrypointPredicate, verificationPredicate)
	}

	for index, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			dockerSandboxesRunSSHAgentIsolationMatrix(t, bash, blocks[index], source.failureSignature)
		})
	}
}

func dockerSandboxesSSHAgentRepoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Docker Sandboxes SSH-agent test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func extractDockerSandboxesSSHAgentBlock(source string) (dockerSandboxesSSHAgentBlock, error) {
	const normalizationMarker = `if [[ "${SSH_AUTH_SOCK:-}" == /run/ssh-agent.sock`
	normalizationStart, err := uniqueDockerSandboxesSSHAgentMarker(source, normalizationMarker, "normalization")
	if err != nil {
		return dockerSandboxesSSHAgentBlock{}, err
	}
	normalization, normalizationEnd, err := extractDockerSandboxesSSHAgentIfBlock(source, normalizationStart, "normalization")
	if err != nil {
		return dockerSandboxesSSHAgentBlock{}, err
	}
	if !strings.Contains(normalization, "unset SSH_AUTH_SOCK") {
		return dockerSandboxesSSHAgentBlock{}, fmt.Errorf("normalization block omitted SSH_AUTH_SOCK unset")
	}

	guardMarkers := []string{
		`if test -n "${SSH_AUTH_SOCK:-}" || test -n "${SSH_AUTH_SOCK_GATEWAY:-}" || test -n "${SSH_AGENT_PID:-}" || test -e /run/ssh-agent.sock || test -L /run/ssh-agent.sock; then`,
		`if [[ -n "${SSH_AUTH_SOCK:-}" || -n "${SSH_AUTH_SOCK_GATEWAY:-}" || -n "${SSH_AGENT_PID:-}" || -e /run/ssh-agent.sock || -L /run/ssh-agent.sock ]]; then`,
	}
	guardStart, foundIfGuard, err := findDockerSandboxesSSHAgentGuardMarker(source, guardMarkers)
	if err != nil {
		return dockerSandboxesSSHAgentBlock{}, err
	}
	if !foundIfGuard {
		return dockerSandboxesSSHAgentBlock{}, fmt.Errorf("strict SSH-agent guard marker was not found")
	}
	if guardStart <= normalizationEnd {
		return dockerSandboxesSSHAgentBlock{}, fmt.Errorf("strict SSH-agent guard does not follow normalization block")
	}
	guard, _, err := extractDockerSandboxesSSHAgentIfBlock(source, guardStart, "strict guard")
	if err != nil {
		return dockerSandboxesSSHAgentBlock{}, err
	}
	if !strings.Contains(guard, "exit 1") {
		return dockerSandboxesSSHAgentBlock{}, fmt.Errorf("strict SSH-agent guard omitted exit 1")
	}
	return dockerSandboxesSSHAgentBlock{normalization: normalization, guard: guard}, nil
}

func uniqueDockerSandboxesSSHAgentMarker(source, marker, name string) (int, error) {
	if count := strings.Count(source, marker); count != 1 {
		return -1, fmt.Errorf("%s marker count = %d, want exactly one", name, count)
	}
	return strings.Index(source, marker), nil
}

func findDockerSandboxesSSHAgentGuardMarker(source string, markers []string) (int, bool, error) {
	guardStart := -1
	for _, marker := range markers {
		count := strings.Count(source, marker)
		if count > 1 {
			return -1, false, fmt.Errorf("strict SSH-agent guard marker %q appears %d times", marker, count)
		}
		if count == 1 {
			if guardStart >= 0 {
				return -1, false, fmt.Errorf("source contains multiple strict SSH-agent guard syntaxes")
			}
			guardStart = strings.Index(source, marker)
		}
	}
	if guardStart < 0 {
		return -1, false, nil
	}
	return guardStart, true, nil
}

func extractDockerSandboxesSSHAgentIfBlock(source string, start int, name string) (string, int, error) {
	closing := strings.Index(source[start:], "\nfi")
	if closing < 0 {
		return "", -1, fmt.Errorf("%s block has no closing fi", name)
	}
	end := start + closing + len("\nfi")
	if end >= len(source) || source[end] != '\n' {
		return "", -1, fmt.Errorf("%s block closing fi is not line-terminated", name)
	}
	return source[start:end], end, nil
}

func assertDockerSandboxesSSHAgentGuardContract(t *testing.T, guard string) {
	t.Helper()
	for _, required := range []string{
		`"${SSH_AUTH_SOCK:-}"`,
		`"${SSH_AUTH_SOCK_GATEWAY:-}"`,
		`"${SSH_AGENT_PID:-}"`,
		"-e /run/ssh-agent.sock",
		"-L /run/ssh-agent.sock",
	} {
		if !strings.Contains(guard, required) {
			t.Fatalf("strict SSH-agent guard omitted %q: %s", required, guard)
		}
	}
	if !strings.Contains(guard, "exit 1") {
		t.Fatal("strict SSH-agent guard omitted failure exit")
	}
}

func requireDockerSandboxesSSHAgentBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SSH-agent shell regression requires POSIX path and socket semantics")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	return bash
}

func shortDockerSandboxesSSHAgentTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "eparssh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove SSH-agent test directory: %v", err)
		}
	})
	return directory
}

func dockerSandboxesRunSSHAgentIsolationMatrix(t *testing.T, bash string, block dockerSandboxesSSHAgentBlock, failureSignature string) {
	t.Helper()
	for _, test := range dockerSandboxesSSHAgentCases() {
		t.Run(test.name, func(t *testing.T) {
			runDir := filepath.Join(shortDockerSandboxesSSHAgentTempDir(t), "run")
			if test.endpoint == "no-traversal" {
				if err := os.WriteFile(runDir, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			socketPath := filepath.Join(runDir, "ssh-agent.sock")
			installDockerSandboxesSSHAgentEndpoint(t, socketPath, test.endpoint)

			script := substituteDockerSandboxesSSHAgentRunPaths(block.normalization+"\n"+block.guard, runDir)
			environment := dockerSandboxesSSHAgentEnvironment(test, socketPath)
			output, runErr := runDockerSandboxesSSHAgentGuard(t, bash, script, environment)
			stateOutput, stateErr := runDockerSandboxesSSHAgentStateProbe(t, bash, block.normalization, runDir, environment)
			if stateErr != nil {
				t.Fatalf("normalization state probe failed: %v\n%s", stateErr, stateOutput)
			}
			if test.wantSuccess {
				if runErr != nil {
					t.Fatalf("normalization/guard rejected accepted environment: %v\n%s", runErr, output)
				}
				assertDockerSandboxesSSHAgentSockState(t, stateOutput, test.wantSockState)
				return
			}
			if runErr == nil {
				t.Fatalf("normalization/guard accepted rejected environment\n%s", output)
			}
			requireDockerSandboxesSSHAgentExitStatus(t, runErr, 1)
			if failureSignature != "" && !strings.Contains(output, failureSignature) {
				t.Fatalf("rejected environment output = %q, want SSH-agent diagnostic containing %q", output, failureSignature)
			}
		})
	}
}

func dockerSandboxesSSHAgentCases() []dockerSandboxesSSHAgentCase {
	const defaultSock = "/run/ssh-agent.sock"
	return []dockerSandboxesSSHAgentCase{
		{name: "exact default with absent endpoint", endpoint: "absent", sock: defaultSock, gateway: "", pid: "", setSock: true, setGateway: true, setPID: true, wantSuccess: true, wantSockState: "unset"},
		{name: "all empty values", endpoint: "absent", sock: "", gateway: "", pid: "", setSock: true, setGateway: true, setPID: true, wantSuccess: true, wantSockState: "set:"},
		{name: "all variables unset", endpoint: "absent", wantSuccess: true, wantSockState: "unset"},
		{name: "actual unix socket", endpoint: "socket", sock: defaultSock, setSock: true, setGateway: true, setPID: true},
		{name: "actual unix socket with empty sock", endpoint: "socket", sock: "", setSock: true, setGateway: true, setPID: true},
		{name: "actual unix socket with sock unset", endpoint: "socket", setGateway: true, setPID: true},
		{name: "regular file", endpoint: "file", sock: defaultSock, setSock: true, setGateway: true, setPID: true},
		{name: "directory", endpoint: "directory", sock: defaultSock, setSock: true, setGateway: true, setPID: true},
		{name: "dangling symlink", endpoint: "dangling-symlink", sock: defaultSock, setSock: true, setGateway: true, setPID: true},
		{name: "no traversal of run path", endpoint: "no-traversal", sock: defaultSock, setSock: true, setGateway: true, setPID: true},
		{name: "nondefault absent path", endpoint: "absent", sock: "/run/other-agent.sock", setSock: true, setGateway: true, setPID: true},
		{name: "gateway", endpoint: "absent", gateway: "gateway.example.test:3129", setSock: true, setGateway: true, setPID: true},
		{name: "pid", endpoint: "absent", pid: "4242", setSock: true, setGateway: true, setPID: true},
		{name: "gateway and pid", endpoint: "absent", gateway: "gateway.example.test:3129", pid: "4242", setSock: true, setGateway: true, setPID: true},
		{name: "default and gateway", endpoint: "absent", sock: defaultSock, gateway: "gateway.example.test:3129", setSock: true, setGateway: true, setPID: true},
		{name: "default and pid", endpoint: "absent", sock: defaultSock, pid: "4242", setSock: true, setGateway: true, setPID: true},
		{name: "default gateway and pid", endpoint: "absent", sock: defaultSock, gateway: "gateway.example.test:3129", pid: "4242", setSock: true, setGateway: true, setPID: true},
	}
}

func installDockerSandboxesSSHAgentEndpoint(t *testing.T, socketPath, endpoint string) {
	t.Helper()
	switch endpoint {
	case "absent", "no-traversal":
		return
	case "socket":
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("listen on test SSH-agent socket: %v", err)
		}
		t.Cleanup(func() {
			if err := listener.Close(); err != nil {
				t.Errorf("close test SSH-agent socket: %v", err)
			}
		})
	case "file":
		if err := os.WriteFile(socketPath, []byte("not a socket"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "directory":
		if err := os.Mkdir(socketPath, 0o700); err != nil {
			t.Fatal(err)
		}
	case "dangling-symlink":
		if err := os.Symlink(filepath.Join(filepath.Dir(socketPath), "missing-agent.sock"), socketPath); err != nil {
			t.Fatalf("create dangling SSH-agent symlink: %v", err)
		}
	default:
		t.Fatalf("unknown SSH-agent endpoint fixture %q", endpoint)
	}
}

func substituteDockerSandboxesSSHAgentRunPaths(script, runDir string) string {
	socketPath := filepath.Join(runDir, "ssh-agent.sock")
	const (
		socketPlaceholder = "__EPAR_SSH_AGENT_SOCKET__"
		runPlaceholder    = "__EPAR_SSH_AGENT_RUN__"
	)
	script = strings.ReplaceAll(script, "/run/ssh-agent.sock", socketPlaceholder)
	script = strings.ReplaceAll(script, "/run", runPlaceholder)
	script = strings.ReplaceAll(script, socketPlaceholder, shellQuoteDockerSandboxesSSHAgentPath(socketPath))
	return strings.ReplaceAll(script, runPlaceholder, shellQuoteDockerSandboxesSSHAgentPath(runDir))
}

func shellQuoteDockerSandboxesSSHAgentPath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func dockerSandboxesSSHAgentEnvironment(test dockerSandboxesSSHAgentCase, socketPath string) []string {
	environment := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "SSH_AUTH_SOCK", "SSH_AUTH_SOCK_GATEWAY", "SSH_AGENT_PID":
			continue
		default:
			environment = append(environment, item)
		}
	}
	if test.setSock {
		sock := test.sock
		if strings.HasPrefix(sock, "/run/") {
			sock = filepath.Join(filepath.Dir(socketPath), strings.TrimPrefix(sock, "/run/"))
		}
		environment = append(environment, "SSH_AUTH_SOCK="+sock)
	}
	if test.setGateway {
		environment = append(environment, "SSH_AUTH_SOCK_GATEWAY="+test.gateway)
	}
	if test.setPID {
		environment = append(environment, "SSH_AGENT_PID="+test.pid)
	}
	return environment
}

func runDockerSandboxesSSHAgentGuard(t *testing.T, bash, script string, environment []string) (string, error) {
	t.Helper()
	harness := "set -euo pipefail\n" + script + "\n"
	command := exec.Command(bash, "-c", harness)
	command.Env = environment
	output, err := command.CombinedOutput()
	return string(output), err
}

func runDockerSandboxesSSHAgentStateProbe(t *testing.T, bash, normalization, runDir string, environment []string) (string, error) {
	t.Helper()
	script := substituteDockerSandboxesSSHAgentRunPaths(normalization, runDir)
	harness := "set -euo pipefail\n" + script + `
if test "${SSH_AUTH_SOCK+x}" = x; then
  printf 'EPAR_SSH_AUTH_SOCK_STATE=set:%s\n' "${SSH_AUTH_SOCK}"
else
  printf 'EPAR_SSH_AUTH_SOCK_STATE=unset\n'
fi
`
	command := exec.Command(bash, "-c", harness)
	command.Env = environment
	output, err := command.CombinedOutput()
	return string(output), err
}

func assertDockerSandboxesSSHAgentSockState(t *testing.T, output, want string) {
	t.Helper()
	marker := "EPAR_SSH_AUTH_SOCK_STATE=" + want
	if !strings.Contains(output, marker) {
		t.Fatalf("normalized SSH_AUTH_SOCK state missing %q in output %q", marker, output)
	}
}

func requireDockerSandboxesSSHAgentExitStatus(t *testing.T, err error, want int) {
	t.Helper()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("shell command error = %v, want exit status %d", err, want)
	}
	if got := exitError.ExitCode(); got != want {
		t.Fatalf("shell command exit status = %d, want %d", got, want)
	}
}
