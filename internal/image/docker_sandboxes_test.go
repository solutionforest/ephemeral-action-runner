package image

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

func TestDockerSandboxesHelperChecksumsMatchGuestScripts(t *testing.T) {
	templateRoot := filepath.Join("..", "..", "templates", "docker-sandboxes")
	content, err := os.ReadFile(filepath.Join(templateRoot, "helpers.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 || !strings.HasPrefix(fields[1], "./") {
			t.Fatalf("helpers.sha256 line %d is malformed: %q", lineNumber+1, line)
		}
		guestPath := filepath.Join(templateRoot, "guest", strings.TrimPrefix(fields[1], "./"))
		guestContent, err := os.ReadFile(guestPath)
		if err != nil {
			t.Fatalf("read helper on line %d: %v", lineNumber+1, err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(guestContent)); got != fields[0] {
			t.Fatalf("helpers.sha256 line %d digest = %s, want %s for %s", lineNumber+1, fields[0], got, guestPath)
		}
	}
}

func TestNormalizeCatthehackerSourceProfilesAndCustomTag(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"", "ghcr.io/catthehacker/ubuntu:full-latest"},
		{"full", "ghcr.io/catthehacker/ubuntu:full-latest"},
		{"act", "ghcr.io/catthehacker/ubuntu:act-latest"},
		{"dotnet", "ghcr.io/catthehacker/ubuntu:dotnet-latest"},
		{"js", "ghcr.io/catthehacker/ubuntu:js-latest"},
		{"go-24.04", "ghcr.io/catthehacker/ubuntu:go-24.04"},
		{"ghcr.io/catthehacker/ubuntu:go-24.04", "ghcr.io/catthehacker/ubuntu:go-24.04"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeCatthehackerSource(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("NormalizeCatthehackerSource(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestDockerSandboxesDockerfileUsesVerifiedLocalDownloadsAndInstallsTrustBeforeCustomization(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "templates", "docker-sandboxes", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, "ADD --") || strings.Contains(text, "ACTIONS_RUNNER_URL") || strings.Contains(text, "TINI_URL") {
		t.Fatalf("Docker Sandboxes Dockerfile still delegates remote HTTPS downloads to BuildKit:\n%s", text)
	}
	for _, want := range []string{
		"COPY --chmod=0755 inputs/tini /usr/local/bin/tini",
		"COPY inputs/actions-runner.tar.gz /tmp/actions-runner.tar.gz",
		`echo "${TINI_SHA256#sha256:}  /usr/local/bin/tini" | sha256sum --check -`,
		`echo "${ACTIONS_RUNNER_SHA256#sha256:}  /tmp/actions-runner.tar.gz" | sha256sum --check -`,
		"RUNNER_TOOL_CACHE=/opt/actions-runner/_work/_tool",
		"AGENT_TOOLSDIRECTORY=/opt/actions-runner/_work/_tool",
		"DOTNET_INSTALL_DIR=/opt/actions-runner/_work/_tool/dotnet",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Docker Sandboxes Dockerfile omitted %q", want)
		}
	}
	trustInstall := strings.Index(text, "/opt/epar/install-trusted-ca-certificates.sh")
	customInstall := strings.Index(text, "/opt/epar/custom-install/run.sh")
	if trustInstall < 0 || customInstall < 0 || trustInstall >= customInstall {
		t.Fatalf("runner trust must be installed before custom scripts:\n%s", text)
	}
	if strings.Count(text, "/opt/epar/install-trusted-ca-certificates.sh") != 2 || strings.LastIndex(text, "/opt/epar/install-trusted-ca-certificates.sh") <= customInstall {
		t.Fatal("runner trust bundle must be refreshed after custom scripts")
	}
	for _, required := range []string{"AS egress-bridge-builder", "ARG EGRESS_BRIDGE_SHA256=", "COPY egress-bridge/main.go /src/main.go", "/out/epar-egress-bridge /opt/epar/epar-egress-bridge"} {
		if !strings.Contains(text, required) {
			t.Fatalf("Docker Sandboxes Dockerfile omitted egress bridge build contract %q", required)
		}
	}
	runnerScript, err := os.ReadFile(filepath.Join("..", "..", "templates", "docker-sandboxes", "guest", "run-runner.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`tool_cache="${EPAR_RUNNER_TOOL_CACHE:-${runner_dir}/_work/_tool}"`,
		`"RUNNER_TOOL_CACHE=${tool_cache}"`,
		`"AGENT_TOOLSDIRECTORY=${tool_cache}"`,
		`"DOTNET_INSTALL_DIR=${tool_cache}/dotnet"`,
	} {
		if !strings.Contains(string(runnerScript), want) {
			t.Fatalf("Docker Sandboxes runner script omitted %q", want)
		}
	}
}

func TestDockerSandboxesRunnerIdentityAndCredentialHygieneContract(t *testing.T) {
	templateRoot := filepath.Join("..", "..", "templates", "docker-sandboxes")
	readTemplateFile := func(t *testing.T, relativePath string) string {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(templateRoot, relativePath))
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}

	t.Run("environment normalization", func(t *testing.T) {
		dockerfile := readTemplateFile(t, "Dockerfile")
		for _, required := range []string{
			"HOME=/home/agent",
			"USER=agent",
			"LOGNAME=agent",
			"SSH_AUTH_SOCK=",
			"SSH_AUTH_SOCK_GATEWAY=",
			"SSH_AGENT_PID=",
			"XDG_CONFIG_HOME=/home/agent/.config",
			"XDG_CACHE_HOME=/home/agent/.cache",
			"XDG_DATA_HOME=/home/agent/.local/share",
			"XDG_STATE_HOME=/home/agent/.local/state",
			"XDG_RUNTIME_DIR=/run/user/1000",
			"DOCKER_CONFIG=/home/agent/.docker",
		} {
			if !strings.Contains(dockerfile, required) {
				t.Fatalf("Docker Sandboxes Dockerfile omitted normalized environment %q", required)
			}
		}

		runner := readTemplateFile(t, filepath.Join("guest", "run-runner.sh"))
		for _, required := range []string{
			"unset SSH_AUTH_SOCK SSH_AUTH_SOCK_GATEWAY SSH_AGENT_PID",
			"env -i",
			`"HOME=${agent_home}"`,
			`"USER=agent"`,
			`"LOGNAME=agent"`,
			`"XDG_CONFIG_HOME=${agent_home}/.config"`,
			`"XDG_CACHE_HOME=${agent_home}/.cache"`,
			`"XDG_DATA_HOME=${agent_home}/.local/share"`,
			`"XDG_STATE_HOME=${agent_home}/.local/state"`,
			`"XDG_RUNTIME_DIR=${agent_runtime_dir}"`,
			`"DOCKER_CONFIG=${agent_home}/.docker"`,
			`sandbox_forward_proxy="http://gateway.docker.internal:3128"`,
			`"HTTP_PROXY=${sandbox_forward_proxy}"`,
			`"HTTPS_PROXY=${sandbox_forward_proxy}"`,
			`"ALL_PROXY=${sandbox_forward_proxy}"`,
			`"http_proxy=${sandbox_forward_proxy}"`,
			`"https_proxy=${sandbox_forward_proxy}"`,
			`"all_proxy=${sandbox_forward_proxy}"`,
			`"SSL_CERT_FILE=/opt/epar/trust/ca-bundle.pem"`,
			`"NODE_EXTRA_CA_CERTS=/opt/epar/trust/ca-bundle.pem"`,
			`"REQUESTS_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"`,
			`"PIP_CERT=/opt/epar/trust/ca-bundle.pem"`,
			`"CURL_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"`,
			`"GIT_SSL_CAINFO=/opt/epar/trust/ca-bundle.pem"`,
			`"AWS_CA_BUNDLE=/opt/epar/trust/ca-bundle.pem"`,
			`ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/epar/prepare-job-start.sh`,
		} {
			if !strings.Contains(runner, required) {
				t.Fatalf("Docker Sandboxes listener environment omitted %q", required)
			}
		}
		if strings.Contains(runner, `"NO_PROXY=`) || strings.Contains(runner, `"no_proxy=`) {
			t.Fatal("Docker Sandboxes listener must not bypass its canonical forward proxy with NO_PROXY")
		}

		configure := readTemplateFile(t, filepath.Join("guest", "configure-runner.sh"))
		for _, required := range []string{
			`sandbox_forward_proxy="http://gateway.docker.internal:3128"`,
			`"HTTP_PROXY=${sandbox_forward_proxy}"`,
			`"HTTPS_PROXY=${sandbox_forward_proxy}"`,
			`"ALL_PROXY=${sandbox_forward_proxy}"`,
			`"http_proxy=${sandbox_forward_proxy}"`,
			`"https_proxy=${sandbox_forward_proxy}"`,
			`"all_proxy=${sandbox_forward_proxy}"`,
			`/opt/epar/scrub-docker-auth.sh --runtime`,
			`"SSL_CERT_FILE=/opt/epar/trust/ca-bundle.pem"`,
			`"NODE_EXTRA_CA_CERTS=/opt/epar/trust/ca-bundle.pem"`,
		} {
			if !strings.Contains(configure, required) {
				t.Fatalf("Docker Sandboxes registration omitted split-egress or runtime scrub contract %q", required)
			}
		}
		if strings.Contains(configure, `"NO_PROXY=`) || strings.Contains(configure, `"no_proxy=`) {
			t.Fatal("Docker Sandboxes registration must not bypass its canonical forward proxy with NO_PROXY")
		}

		entrypoint := readTemplateFile(t, filepath.Join("guest", "template-entrypoint.sh"))
		for _, required := range []string{
			`-n "${SSH_AUTH_SOCK:-}"`,
			`-n "${SSH_AUTH_SOCK_GATEWAY:-}"`,
			"-e /run/ssh-agent.sock",
			"host SSH-agent forwarding is not permitted",
			"unset http_proxy https_proxy no_proxy HTTP_PROXY HTTPS_PROXY NO_PROXY",
			`docker info --format '{{.NoProxy}}'`,
			"policy-enforced transparent egress",
			"/opt/epar/epar-egress-bridge",
			"/run/epar/egress-bridge.pid",
			"cleanup_bridge_on_failure",
			`sudo -n kill -TERM "${cleanup_bridge_pids[0]}"`,
		} {
			if !strings.Contains(entrypoint, required) {
				t.Fatalf("Docker Sandboxes entrypoint omitted SSH-agent isolation contract %q", required)
			}
		}
		verify := readTemplateFile(t, filepath.Join("guest", "verify-template.sh"))
		for _, required := range []string{`[[ -z "${SSH_AUTH_SOCK:-}" ]]`, `[[ -z "${SSH_AUTH_SOCK_GATEWAY:-}" ]]`, `[[ -z "${SSH_AGENT_PID:-}" ]]`, `[[ ! -e /run/ssh-agent.sock && ! -L /run/ssh-agent.sock ]]`} {
			if !strings.Contains(verify, required) {
				t.Fatalf("Docker Sandboxes template verification omitted SSH-agent isolation contract %q", required)
			}
		}
	})

	t.Run("private directory ownership", func(t *testing.T) {
		prepare := readTemplateFile(t, filepath.Join("guest", "prepare-template.sh"))
		for _, required := range []string{
			"install -d -m 0700 -o agent -g agent",
			"/home/agent/.docker",
			"/home/agent/.config",
			"/home/agent/.cache",
			"/home/agent/.local/share",
			"/home/agent/.local/state",
			"/run/user/1000",
		} {
			if !strings.Contains(prepare, required) {
				t.Fatalf("Docker Sandboxes template preparation omitted private-directory contract %q", required)
			}
		}

		verify := readTemplateFile(t, filepath.Join("guest", "verify-template.sh"))
		if !strings.Contains(verify, `stat -c '%U:%G:%a'`) || !strings.Contains(verify, `"agent:agent:700"`) {
			t.Fatal("Docker Sandboxes template verification does not enforce restrictive agent directory ownership and mode")
		}
		entrypoint := readTemplateFile(t, filepath.Join("guest", "template-entrypoint.sh"))
		if !strings.Contains(entrypoint, "sudo -n install -d -m 0700 -o agent -g agent /run/user/1000") {
			t.Fatal("Docker Sandboxes entrypoint does not recreate the agent runtime directory after a boot-time /run reset")
		}
		configure := readTemplateFile(t, filepath.Join("guest", "configure-runner.sh"))
		if !strings.Contains(configure, "install -d -m 0700 -o agent -g agent") || !strings.Contains(configure, "/run/user/1000") {
			t.Fatal("Docker Sandboxes runner configuration does not ensure the agent runtime directory exists")
		}
	})

	t.Run("source credential scrubbing", func(t *testing.T) {
		prepare := readTemplateFile(t, filepath.Join("guest", "prepare-template.sh"))
		scrubber := readTemplateFile(t, filepath.Join("guest", "scrub-docker-auth.sh"))
		if !strings.Contains(prepare, `/opt/epar/scrub-docker-auth.sh --build`) {
			t.Fatal("Docker Sandboxes template preparation does not invoke the shared build-time credential scrub")
		}
		for _, required := range []string{
			`passwd_entries="$(getent passwd)"`,
			`[[ -z "${passwd_entries}" ]]`,
			`done <<<"${passwd_entries}"`,
			"for root_home_docker_config in /.docker /.dockercfg",
			`rm -rf -- "${credential_home}/.docker"`,
			`rm -f -- "${credential_home}/.dockercfg"`,
			"failed to scrub Docker client authentication",
		} {
			if !strings.Contains(scrubber, required) {
				t.Fatalf("Docker Sandboxes template preparation omitted credential-scrubbing contract %q", required)
			}
		}
		for _, forbidden := range []string{"cat /root/.docker", "cat /home/runner/.docker", "cat /home/agent/.docker", "cp /root/.docker", "cp /home/runner/.docker"} {
			if strings.Contains(scrubber, forbidden) {
				t.Fatalf("Docker Sandboxes credential hygiene exposes or copies source credential material via %q", forbidden)
			}
		}
		configure := readTemplateFile(t, filepath.Join("guest", "configure-runner.sh"))
		if !strings.Contains(configure, `/opt/epar/scrub-docker-auth.sh --runtime`) {
			t.Fatal("Docker Sandboxes registration does not invoke the narrow runtime credential scrub")
		}
		for _, required := range []string{
			`docker_config_dir="${credential_home}/.docker"`,
			`[[ -L "${docker_config_dir}" ]]`,
			`rm -f -- /.docker/config.json /.dockercfg`,
			`runtime_config="${credential_home}/.docker/config.json"`,
			`rm -f -- "${runtime_config}"`,
			`preserve .docker/sandbox/locks`,
		} {
			if !strings.Contains(scrubber+configure, required) {
				t.Fatalf("Docker Sandboxes runtime credential scrub omitted %q", required)
			}
		}
		verify := readTemplateFile(t, filepath.Join("guest", "verify-template.sh"))
		for _, required := range []string{
			`passwd_entries="$(getent passwd)"`,
			`done <<<"${passwd_entries}"`,
			`sudo -n test ! -e "${normalized_home}/.dockercfg"`,
			`sudo -n test ! -e "${normalized_home}/.docker"`,
		} {
			if !strings.Contains(verify, required) {
				t.Fatalf("Docker Sandboxes template verification omitted foreign-home credential check %q", required)
			}
		}
	})

	t.Run("foreign runner config rejection", func(t *testing.T) {
		for _, relativePath := range []string{
			filepath.Join("guest", "template-entrypoint.sh"),
			filepath.Join("guest", "run-runner.sh"),
			filepath.Join("guest", "verify-template.sh"),
		} {
			content := readTemplateFile(t, relativePath)
			if !strings.Contains(content, "/home/runner/.docker") {
				t.Fatalf("%s does not reject stale foreign Docker client configuration", relativePath)
			}
		}
	})

	t.Run("job-start split-egress boundary", func(t *testing.T) {
		hook := readTemplateFile(t, filepath.Join("guest", "prepare-job-start.sh"))
		for _, required := range []string{
			`/opt/epar/check-host-trust-generation.sh`,
			`GITHUB_ENV`,
			`_runner_file_commands`,
			`readlink -f -- "${GITHUB_ENV}"`,
			`[[ -L "${GITHUB_ENV}" || ! -f "${GITHUB_ENV}" ]]`,
			`stat -c '%u:%g'`,
			`printf 'HTTP_PROXY=\n'`,
			`printf 'HTTPS_PROXY=%s\n' "${workflow_https_proxy}"`,
			`printf 'ALL_PROXY=\n'`,
			`printf 'http_proxy=\n'`,
			`printf 'https_proxy=%s\n' "${workflow_https_proxy}"`,
			`printf 'all_proxy=\n'`,
			`printf 'NO_PROXY=%s\n' "${workflow_no_proxy}"`,
			`printf 'no_proxy=%s\n' "${workflow_no_proxy}"`,
			`workflow_https_proxy="http://127.0.0.1:3130"`,
			`/run/epar/egress-relay-active`,
			`if sudo -n test -e /run/epar/egress-relay-active`,
			`sudo -n stat -c '%U:%G:%a' /run/epar/egress-relay-active`,
			`http://127.0.0.1:3129/health`,
			`marker_mode="$(jq -er '.mode | strings' /opt/epar/host-trust-generation.json)"`,
			`relay_required=true`,
			`Windows host-trust overlay requires the authenticated relay`,
		} {
			if !strings.Contains(hook, required) {
				t.Fatalf("Docker Sandboxes job-start hook omitted %q", required)
			}
		}
		launcher := readTemplateFile(t, filepath.Join("hook-launcher", "main.go"))
		for _, required := range []string{`hookPath = "/opt/epar/prepare-job-start.sh"`, `"GITHUB_ENV": true`, `"LANG":`} {
			if !strings.Contains(launcher, required) {
				t.Fatalf("Docker Sandboxes hook launcher omitted %q", required)
			}
		}
	})
}

func TestDockerSandboxesDockerDaemonBootstrapsThenUsesAuthenticatedHostTrustRelay(t *testing.T) {
	templateRoot := filepath.Join("..", "..", "templates", "docker-sandboxes")
	content, err := os.ReadFile(filepath.Join(templateRoot, "guest", "docker-daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var configuration struct {
		Proxies struct {
			HTTPProxy  string `json:"http-proxy"`
			HTTPSProxy string `json:"https-proxy"`
			NoProxy    string `json:"no-proxy"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatalf("parse Docker daemon proxy configuration: %v", err)
	}
	if configuration.Proxies.HTTPProxy != "http://gateway.docker.internal:3128" || configuration.Proxies.HTTPSProxy != "http://gateway.docker.internal:3128" || configuration.Proxies.NoProxy != "*" {
		t.Fatalf("Docker daemon proxy configuration = %#v, want the bootstrap Sandbox proxy isolated by daemon-wide no-proxy", configuration.Proxies)
	}
	prepareContent, err := os.ReadFile(filepath.Join(templateRoot, "guest", "prepare-template.sh"))
	if err != nil {
		t.Fatal(err)
	}
	prepare := string(prepareContent)
	for _, required := range []string{
		"pinned source image unexpectedly supplies /etc/docker/daemon.json",
		"install -m 0644 -o root -g root /opt/epar/docker-daemon.json /etc/docker/daemon.json",
		"cmp -s /opt/epar/docker-daemon.json /etc/docker/daemon.json",
		"rm -f /etc/sudoers.d/epar-proxy",
	} {
		if !strings.Contains(prepare, required) {
			t.Fatalf("Docker Sandboxes template preparation omitted daemon proxy contract %q", required)
		}
	}
	verifyContent, err := os.ReadFile(filepath.Join(templateRoot, "guest", "verify-template.sh"))
	if err != nil {
		t.Fatal(err)
	}
	verify := string(verifyContent)
	for _, required := range []string{
		"test ! -L /etc/docker/daemon.json",
		`stat -c '%U:%G:%a' /etc/docker/daemon.json`,
		`"root:root:644"`,
		`.proxies == {`,
		`(keys - ["proxies", "registry-mirrors"])`,
		`has("registry-mirrors")`,
		`docker info --format '{{.NoProxy}}'`,
		`http://127.0.0.1:3129`,
		`/run/epar/egress-relay.json`,
		`/run/epar/egress-relay-active`,
		`if sudo -n test -e /run/epar/egress-relay-active`,
		`marker_host_os="$(jq -er '.hostOS | strings | ascii_downcase' /opt/epar/host-trust-generation.json)"`,
		`Windows host-trust overlay requires the authenticated relay`,
	} {
		if !strings.Contains(verify, required) {
			t.Fatalf("Docker Sandboxes template verification omitted daemon proxy contract %q", required)
		}
	}
	activationContent, err := os.ReadFile(filepath.Join(templateRoot, "guest", "configure-egress-relay.sh"))
	if err != nil {
		t.Fatal(err)
	}
	activation := string(activationContent)
	for _, required := range []string{
		`bridge_proxy="http://127.0.0.1:3129"`,
		`install -d -m 0755 -o root -g root "${config_dir}"`,
		`relay_ca_source="${config_dir}/egress-relay-ca.crt"`,
		`relay_ca_key="${config_dir}/egress-relay-ca.key"`,
		`relay_ca_trust="/usr/local/share/ca-certificates/epar/epar-egress-relay.crt"`,
		`GODEBUG=tlsmlkem=0,tlssecpmlkem=0`,
		`remove_relay_ca_trust()`,
		`set(value) != {"schemaVersion", "relayAddress", "token"}`,
		`daemon_already_configured=false`,
		`if [[ "${daemon_already_configured}" != "true" ]]`,
		`((keys - ["proxies", "registry-mirrors"]) | length) == 0 and .proxies == {"https-proxy": $proxy, "no-proxy": $no_proxy}`,
		`daemon_backup="${config_dir}/docker-daemon.pre-relay.json"`,
		`rollback_daemon()`,
		`if [[ "${mode}" == "--commit" ]]`,
		`if [[ "${mode}" == "--rollback" ]]`,
		`mv -f "${daemon_config}.rollback.new" "${daemon_config}"`,
		`if [[ "${status}" != "0" ]]`,
		`[[ "${#dockerd_pids[@]}" != "1" ]]`,
		`if [[ -n "$(docker ps -aq)" ]]`,
		`kill -TERM "${dockerd_pid}"`,
		`env -i HOME=/root`,
		`--proxy "${bridge_proxy}" --noproxy '' --cacert "${relay_ca_trust}"`,
		`if [[ "${registry_status}" != "401" ]]`,
	} {
		if !strings.Contains(activation, required) {
			t.Fatalf("Docker Sandboxes runtime relay activation omitted %q", required)
		}
	}
}

func TestDockerSandboxesDisabledTrustPolicyIsExplicit(t *testing.T) {
	root := t.TempDir()
	coordinator := &Coordinator{ProjectRoot: root}
	if err := coordinator.prepareDockerSandboxesTrustPolicy(root, hosttrust.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "host-trust-metadata", "host-trust-generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		SchemaVersion    int      `json:"schemaVersion"`
		Generation       string   `json:"generation"`
		HostOS           string   `json:"hostOS"`
		Mode             string   `json:"mode"`
		Scopes           []string `json:"scopes"`
		CertificateCount int      `json:"certificateCount"`
	}
	if err := json.Unmarshal(content, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.SchemaVersion != 1 || marker.Generation != "disabled" || marker.HostOS != "" || marker.Mode != hosttrust.ModeDisabled || len(marker.Scopes) != 0 || marker.CertificateCount != 0 {
		t.Fatalf("disabled policy marker = %+v", marker)
	}
}

func TestReadVerifiedBuildEvidenceRequiresStableBoundedRegularFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "evidence.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := readVerifiedBuildEvidence(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != `{"ok":true}` {
		t.Fatalf("evidence = %q", content)
	}
	if _, err := readVerifiedBuildEvidence(path, 4); err == nil {
		t.Fatal("oversized evidence was accepted")
	}
	link := filepath.Join(root, "evidence-link.json")
	if err := os.Symlink(path, link); err == nil {
		if _, err := readVerifiedBuildEvidence(link, 64); err == nil {
			t.Fatal("symlinked evidence was accepted")
		}
	}
}

func TestProvenanceValidatorsRequireMaxBuildxAndInTotoSLSAContracts(t *testing.T) {
	buildx := []byte(`{"buildType":"https://mobyproject.org/buildkit@v1","materials":[{"uri":"pkg:docker/example"}],"invocation":{"parameters":{"frontend":"gateway.v0"}}}`)
	if err := validateBuildxMaxProvenance(buildx); err != nil {
		t.Fatal(err)
	}
	if err := validateBuildxMaxProvenance([]byte(`{"buildType":"buildkit","materials":[],"invocation":{}}`)); err == nil {
		t.Fatal("incomplete Buildx provenance was accepted")
	}
	statement := []byte(`{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://slsa.dev/provenance/v1","subject":[{"name":"software-inventory.txt"}],"predicate":{"buildDefinition":{}}}`)
	if err := validateInTotoProvenance(statement); err != nil {
		t.Fatal(err)
	}
	if err := validateInTotoProvenance([]byte(`{"_type":"https://in-toto.io/Statement/v1","predicateType":"unknown","subject":[],"predicate":{}}`)); err == nil {
		t.Fatal("invalid in-toto provenance was accepted")
	}
}

func TestValidateInTotoSPDXStreamsAndRejectsMalformedPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sbom.intoto.json")
	valid := `{"_type":"https://in-toto.io/Statement/v1","subject":[],"predicateType":"https://spdx.dev/Document","predicate":{"SPDXID":"SPDXRef-DOCUMENT","packages":[{"name":"example"}]}}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateInTotoSPDX(path); err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(valid, "https://spdx.dev/Document", "https://example.invalid/Unknown", 1)
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateInTotoSPDX(path); err == nil {
		t.Fatal("unknown predicate type was accepted")
	}
}

func TestNormalizeCatthehackerSourceRejectsOtherRepositoriesAndInvalidTags(t *testing.T) {
	for _, input := range []string{
		"ubuntu:latest",
		"ghcr.io/other/ubuntu:full-latest",
		"ghcr.io/catthehacker/ubuntu@sha256:" + strings.Repeat("a", 64),
		"tag with spaces",
		"-leading-dash",
	} {
		if _, err := NormalizeCatthehackerSource(input); err == nil {
			t.Fatalf("NormalizeCatthehackerSource(%q) succeeded", input)
		}
	}
}

func TestParseResolvedDockerSourceSelectsExactNativeManifestAndSize(t *testing.T) {
	index := dockerManifestDocument{MediaType: "application/vnd.oci.image.index.v1+json"}
	amd64 := dockerManifestDescriptor{Digest: "sha256:" + strings.Repeat("a", 64)}
	amd64.Platform.OS = "linux"
	amd64.Platform.Architecture = "amd64"
	arm64 := dockerManifestDescriptor{Digest: "sha256:" + strings.Repeat("b", 64)}
	arm64.Platform.OS = "linux"
	arm64.Platform.Architecture = "arm64"
	index.Manifests = []dockerManifestDescriptor{amd64, arm64}
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw := []byte(`{"layers":[{"size":100},{"size":23}]}`)
	resolved, err := parseResolvedDockerSource("ghcr.io/catthehacker/ubuntu:full-latest", "linux/arm64", indexRaw, manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.PlatformDigest != arm64.Digest || resolved.CompressedLayerBytes != 123 || !strings.HasPrefix(resolved.ImmutableReference, "ghcr.io/catthehacker/ubuntu@sha256:") {
		t.Fatalf("resolved source = %+v", resolved)
	}
	withCommandNewline, err := parseResolvedDockerSource("ghcr.io/catthehacker/ubuntu:full-latest", "linux/arm64", append(append([]byte(nil), indexRaw...), '\n'), append(append([]byte(nil), manifestRaw...), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if withCommandNewline.IndexDigest != resolved.IndexDigest {
		t.Fatalf("index digest changed because the CLI appended a newline: %s != %s", withCommandNewline.IndexDigest, resolved.IndexDigest)
	}
}

func TestDockerSandboxesArtifactIdentityChangesWithEveryFreshnessInput(t *testing.T) {
	base := Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		ProviderType:         "docker-sandboxes",
		ProviderPlatform:     "linux/arm64",
		SourceType:           "docker-image",
		SourceImage:          "ghcr.io/catthehacker/ubuntu:full-latest",
		SourceDigest:         "sha256:" + strings.Repeat("a", 64),
		SourcePlatformDigest: "sha256:" + strings.Repeat("b", 64),
		RunnerVersion:        "2.332.0",
		TemplateInputs:       []FileDigest{{Path: "Dockerfile", SHA256: strings.Repeat("c", 64)}},
		CustomInstallScripts: []FileDigest{{Path: "custom.sh", SHA256: strings.Repeat("d", 64)}},
	}
	baseHash, err := ManifestHash(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Manifest){
		func(value *Manifest) { value.SourceDigest = "sha256:" + strings.Repeat("e", 64) },
		func(value *Manifest) { value.SourcePlatformDigest = "sha256:" + strings.Repeat("f", 64) },
		func(value *Manifest) { value.RunnerVersion = "2.333.0" },
		func(value *Manifest) { value.TemplateInputs[0].SHA256 = strings.Repeat("1", 64) },
		func(value *Manifest) { value.CustomInstallScripts[0].SHA256 = strings.Repeat("2", 64) },
		func(value *Manifest) {
			value.HostTrust = &HostTrustMetadata{Generation: "sha256:" + strings.Repeat("3", 64)}
		},
	}
	for index, mutate := range mutations {
		changed := base
		changed.TemplateInputs = append([]FileDigest(nil), base.TemplateInputs...)
		changed.CustomInstallScripts = append([]FileDigest(nil), base.CustomInstallScripts...)
		mutate(&changed)
		hash, err := ManifestHash(changed)
		if err != nil {
			t.Fatal(err)
		}
		if hash == baseHash {
			t.Fatalf("freshness mutation %d did not change artifact identity", index)
		}
	}
}

func TestVerifiedDockerSandboxesBuildArtifactAcceptsOnlyCompleteExactEvidence(t *testing.T) {
	root := t.TempDir()
	manifestHash := strings.Repeat("a", 64)
	source := ResolvedDockerSource{
		Reference:            "ghcr.io/catthehacker/ubuntu:full-latest",
		ImmutableReference:   "ghcr.io/catthehacker/ubuntu@sha256:" + strings.Repeat("b", 64),
		IndexDigest:          "sha256:" + strings.Repeat("b", 64),
		PlatformDigest:       "sha256:" + strings.Repeat("c", 64),
		Platform:             "linux/amd64",
		CompressedLayerBytes: 123,
	}
	fixturePath, templateDigest, _ := writeDockerArchiveFixture(t, false, false)
	archivePath := filepath.Join(root, "runner-template.tar")
	fixtureContent, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, fixtureContent, 0o600); err != nil {
		t.Fatal(err)
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := dockerSandboxesTemplateMetadata{
		SchemaVersion: dockerSandboxesMetadataSchema,
		Profile:       "full-latest",
		Platform:      source.Platform,
		ManifestHash:  manifestHash,
		Source:        source,
		Artifacts:     make(map[string]artifactEvidence),
	}
	metadata.Template.Tag = "docker.io/library/epar-template:test-amd64"
	metadata.Template.Digest = templateDigest
	metadata.Template.CacheID = strings.TrimPrefix(templateDigest, "sha256:")[:12]
	metadata.Template.RootDisk = "90GiB"
	metadata.Template.Archive = filepath.Base(archivePath)
	metadata.Template.ArchiveSHA256 = archiveSHA
	metadata.Template.ArchiveBytes = archiveBytes
	metadata.Compatibility.TemplateSchemaVersion = 2
	metadata.Compatibility.RunnerExecution = "direct-actions-listener"
	metadata.Compatibility.DockerDaemonOwner = "docker-sandboxes-runtime"
	metadata.Compatibility.ExpectedDockerDaemonCount = 1
	metadata.Compatibility.EmulationBackend = "qemu"
	metadata.Compatibility.EmulationPolicy = "configured-best-effort-required-or-native-only"
	metadata.Compatibility.EmulationRelease = "qemu-v10.2.3-68"
	metadata.Compatibility.EmulationSourceDigest = "sha256:" + strings.Repeat("d", 64)
	metadata.Compatibility.EmulationManifestDigest = "sha256:" + strings.Repeat("e", 64)
	metadata.Compatibility.QEMUVersion = "10.2.3"
	if err := writeJSONFile(filepath.Join(root, "buildMetadata.json"), dockerSandboxesBuildMetadata{
		ImageDigest: templateDigest,
		Provenance:  json.RawMessage(`{}`),
		BuildRef:    strings.Repeat("b", 12),
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"buildMetadata", "attestationMetadata", "provenance", "sbom", "softwareInventory", "compatibility"} {
		path := filepath.Join(root, name+".json")
		if name != "buildMetadata" {
			if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		digest, _, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		metadata.Artifacts[name] = artifactEvidence{Path: filepath.Base(path), SHA256: digest}
	}
	metadataPath := filepath.Join(root, "template-metadata.json")
	if err := writeJSONFile(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}
	_, artifact, _, _, valid, err := verifiedDockerSandboxesBuildArtifact(root, metadataPath, archivePath, manifestHash, source)
	if err != nil {
		t.Fatal(err)
	}
	if !valid || artifact.Digest != templateDigest || artifact.Platform != "linux/amd64" || artifact.RootDisk != "90GiB" {
		t.Fatalf("verified artifact = %+v, valid=%t", artifact, valid)
	}

	if err := os.WriteFile(filepath.Join(root, "sbom.json"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, valid, err = verifiedDockerSandboxesBuildArtifact(root, metadataPath, archivePath, manifestHash, source)
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("corrupted evidence was accepted for interrupted-build resume")
	}
}

func TestDockerSandboxesEmulationLockAndTemplateAssetsAreExact(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	expected := map[string]struct {
		digest          string
		compressedBytes uint64
	}{
		"linux/amd64": {digest: "sha256:465d3fdd28d0f2b871ba4b4ec98bd183292e96167f00d9fd40bd249f8632d705", compressedBytes: 32675086},
		"linux/arm64": {digest: "sha256:b4c6a09270133b3c5b4dff94f83067df4dd27eced195fc6a1dbad102999e24dd", compressedBytes: 31024752},
	}
	for platform, want := range expected {
		t.Run(platform, func(t *testing.T) {
			lock, err := loadDockerSandboxesSourceLock(projectRoot, platform)
			if err != nil {
				t.Fatal(err)
			}
			if lock.SchemaVersion != 3 || lock.Emulation.SchemaVersion != 1 || lock.Emulation.Backend != "qemu" || lock.Emulation.Source.Release != "qemu-v10.2.3-68" || lock.Emulation.Source.QEMUVersion != "10.2.3" || lock.Emulation.Source.IndexDigest != "sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0" {
				t.Fatalf("unexpected emulation source lock: %+v", lock.Emulation.Source)
			}
			platformLock := lock.Emulation.Platforms[platform]
			if platformLock.ManifestDigest != want.digest || platformLock.CompressedLayerBytes != want.compressedBytes || platformLock.SourceReference != "docker.io/tonistiigi/binfmt:qemu-v10.2.3-68@"+want.digest {
				t.Fatalf("%s emulation platform lock = %+v", platform, platformLock)
			}
		})
	}
	templateRoot := filepath.Join(projectRoot, "templates", "docker-sandboxes")
	dockerfile, err := os.ReadFile(filepath.Join(templateRoot, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"/usr/bin/binfmt /usr/bin/qemu-* /opt/epar/emulation/", "enable-architecture-emulation.sh /opt/epar/enable-architecture-emulation", "verify-native-architecture.sh /opt/epar/verify-native-architecture"} {
		if !strings.Contains(string(dockerfile), required) {
			t.Fatalf("Dockerfile omits %q", required)
		}
	}
	for _, forbidden := range []string{"emulation-probe", "emulation-manifest", "emulation-interpreters", "mixed-compose", "EMULATION_PROBE_SHA256"} {
		if strings.Contains(string(dockerfile), forbidden) {
			t.Fatalf("Dockerfile retains removed per-target asset %q", forbidden)
		}
	}
	helper, err := os.ReadFile(filepath.Join(templateRoot, "guest", "enable-architecture-emulation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(helper), "--install all") || !strings.Contains(string(helper), "modprobe binfmt_misc") || !strings.Contains(string(helper), "/proc/sys/fs/binfmt_misc") || !strings.Contains(string(helper), "sandbox kernel/module set does not provide usable binfmt_misc support") {
		t.Fatal("architecture emulation helper does not install and structurally verify binfmt handlers")
	}
	nativeHelper, err := os.ReadFile(filepath.Join(templateRoot, "guest", "verify-native-architecture.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"linux/amd64", "linux/arm64", "docker info --format", "/opt/epar/emulation/qemu-", `"backend":"native"`, `"handlerCount":%d`, "epar_handler_count"} {
		if !strings.Contains(string(nativeHelper), required) {
			t.Fatalf("native architecture helper omits %q", required)
		}
	}
}

func TestDockerSandboxesEmulationStorageEstimateIncludesLockedAssets(t *testing.T) {
	coordinator := Coordinator{ProjectRoot: filepath.Join("..", "..")}
	source := ResolvedDockerSource{Platform: "linux/arm64", CompressedLayerBytes: 1024}
	estimate, err := coordinator.dockerSandboxesSourceEstimate(source)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := loadDockerSandboxesSourceLock(coordinator.ProjectRoot, source.Platform)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.CompressedBytes != source.CompressedLayerBytes+lock.Emulation.Platforms[source.Platform].CompressedLayerBytes {
		t.Fatalf("compressed estimate = %d", estimate.CompressedBytes)
	}
	if estimate.ExpandedBytes != source.CompressedLayerBytes*ExpandedSizeFallbackMultiplier+dockerSandboxesExpandedEmulationAllowanceBytes || estimate.Confidence != EstimateFallback {
		t.Fatalf("expanded emulation estimate = %+v", estimate)
	}
}

func TestDockerSandboxesArchitectureAssetsNeverInjectDefaultPlatform(t *testing.T) {
	root := filepath.Join("..", "..", "templates", "docker-sandboxes")
	inputs, err := fileDigestsRecursive(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(input.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "DOCKER_DEFAULT_PLATFORM=") || strings.Contains(string(content), "export DOCKER_DEFAULT_PLATFORM") {
			t.Fatalf("template input %s injects DOCKER_DEFAULT_PLATFORM", input.Path)
		}
	}
}

func TestDockerSandboxesBuildUsesDirectArchiveAndInventoryTargets(t *testing.T) {
	sourcePath := filepath.Join("docker_sandboxes.go")
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, required := range []string{
		`"--target", "runner-template", "--output", "type=docker,dest=" + partialArchivePath`,
		`"--provenance=false", "--sbom=false"`,
		`"--target", "software-inventory-export", "--output", "type=local,dest=" + evidenceExportRoot`,
		`"--provenance", "mode=max", "--sbom", "generator=" + platformLock.SBOMGeneratorReference`,
		`m.stopBuildxBuilder(ctx, builder, "release archive-build memory before full-image SBOM generation")`,
		`"-attestation.docker-build.log"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Docker Sandboxes build path omitted %q", required)
		}
	}
	archiveBuild := strings.Index(text, `m.runHostBuildxLogged(ctx, buildLogPath, "docker", args...)`)
	memoryRelease := strings.Index(text, `m.stopBuildxBuilder(ctx, builder, "release archive-build memory before full-image SBOM generation")`)
	evidenceBuild := strings.Index(text, `m.runHostBuildxLogged(ctx, attestationLogPath, "docker", attestationArgs...)`)
	if archiveBuild < 0 || memoryRelease <= archiveBuild || evidenceBuild <= memoryRelease {
		t.Fatalf("Docker Sandboxes build does not release its exact BuildKit worker between archive and evidence phases")
	}
	for _, forbidden := range []string{`"--load"`, `"image", "save"`, `"image", "load"`, `"image", "inspect"`, `"type=image,push=false"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Docker Sandboxes build path retained forbidden Docker staging operation %q", forbidden)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "templates", "docker-sandboxes", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"AS runner-template", "ARG BUILDKIT_SBOM_SCAN_STAGE=true", "AS software-inventory-export"} {
		if !strings.Contains(string(dockerfile), required) {
			t.Fatalf("Dockerfile omitted %q", required)
		}
	}
}

func TestDockerSandboxesEvidenceBuildErrorExplainsSyftExit137(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "attestation.log")
	if err := os.WriteFile(logPath, []byte("starting syft scanner\nERROR: process generating sbom did not complete successfully: exit code: 137\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("buildx failed")
	err := dockerSandboxesEvidenceBuildError(cause, logPath)
	if !errors.Is(err, cause) {
		t.Fatalf("evidence error does not preserve cause: %v", err)
	}
	for _, required := range []string{"full-image SBOM scanner", "exit code 137", "exhausted memory", "other Docker workloads", "increase the VM memory allocation"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("evidence error omitted %q: %v", required, err)
		}
	}
}

func TestDockerSandboxesEvidenceBuildErrorDoesNotMisdiagnoseGenericFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "attestation.log")
	if err := os.WriteFile(logPath, []byte("ERROR: registry request failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := dockerSandboxesEvidenceBuildError(errors.New("buildx failed"), logPath)
	if strings.Contains(err.Error(), "exhausted memory") {
		t.Fatalf("generic evidence failure was misdiagnosed as memory exhaustion: %v", err)
	}
}
