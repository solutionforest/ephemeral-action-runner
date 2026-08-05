package dockersandboxes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	sandboxfs "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/staging"
)

func TestLiveCrossArchitectureCompose(t *testing.T) {
	if os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_CROSS_ARCH") != "1" {
		t.Skip("set EPAR_LIVE_DOCKER_SANDBOXES_CROSS_ARCH=1 to run the live mixed-architecture Compose proof")
	}
	template := os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_TEMPLATE")
	digest := os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_TEMPLATE_DIGEST")
	stagingRoot := os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_STAGING_ROOT")
	if template == "" || digest == "" || stagingRoot == "" {
		t.Fatal("live Docker Sandboxes template, digest, and absolute staging root are required")
	}
	staging, err := sandboxfs.Open(stagingRoot)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("epar-cross-arch-live-%d", time.Now().UnixNano())
	stagingPath := filepath.Join(staging.Root(), name)
	rootDisk := os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_ROOT_SIZE")
	if rootDisk == "" {
		rootDisk = "30GiB"
	}
	dockerDisk := os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_DOCKER_SIZE")
	if dockerDisk == "" {
		dockerDisk = "50GiB"
	}
	p := New("sbx")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	instance, err := p.Create(ctx, provider.CreateRequest{
		Name: name, Template: template, TemplateDigest: digest, StagingPath: stagingPath,
		CPUs: 4, Memory: "8GiB", RootDisk: rootDisk, DockerDisk: dockerDisk,
	})
	if err != nil {
		if instance.Name != "" && instance.ProviderID != "" && instance.ReceiptVersion != "" {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cleanupCancel()
			_ = p.Stop(cleanupCtx, instance)
			if cleanupErr := p.Delete(cleanupCtx, instance); cleanupErr != nil {
				t.Errorf("clean up partially created cross-architecture sandbox: %v", cleanupErr)
			}
		}
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		if stopErr := p.Stop(cleanupCtx, instance); stopErr != nil {
			t.Errorf("stop exact cross-architecture sandbox: %v", stopErr)
		}
		if deleteErr := p.Delete(cleanupCtx, instance); deleteErr != nil {
			t.Errorf("delete exact cross-architecture sandbox: %v", deleteErr)
		}
		if _, statErr := os.Lstat(stagingPath); !os.IsNotExist(statErr) {
			t.Errorf("cross-architecture staging directory remains after exact deletion: %v", statErr)
		}
	}()
	if _, err := p.Start(ctx, instance, provider.StartOptions{}); err != nil {
		t.Fatal(err)
	}

	result, err := p.Exec(ctx, instance, []string{"bash", "-lc", mixedArchitectureComposeProof}, provider.ExecOptions{})
	if err != nil {
		t.Fatalf("mixed-architecture Compose proof failed: %v\nstdout:\n%s\nstderr:\n%s", err, result.Stdout, result.Stderr)
	}
	t.Log(strings.TrimSpace(result.Stdout))
}

const mixedArchitectureComposeProof = `set -euo pipefail
if env | grep -q '^DOCKER_DEFAULT_PLATFORM='; then echo 'DOCKER_DEFAULT_PLATFORM must be absent' >&2; exit 1; fi
native_arch="$(uname -m)"
case "${native_arch}" in
  aarch64|arm64)
    native_platform='linux/arm64'; native_expected='aarch64'
    native_ref='docker.io/library/alpine@sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37'
    foreign_platform='linux/amd64'; foreign_expected='x86_64'; foreign_handler='qemu-x86_64'
    foreign_ref='docker.io/library/alpine@sha256:7c8cb692ae09657cbc4a3f3cbd0e8d5a2690ba38386aaaf252dbb060bf5eb2e6'
    ;;
  x86_64|amd64)
    native_platform='linux/amd64'; native_expected='x86_64'
    native_ref='docker.io/library/alpine@sha256:7c8cb692ae09657cbc4a3f3cbd0e8d5a2690ba38386aaaf252dbb060bf5eb2e6'
    foreign_platform='linux/arm64'; foreign_expected='aarch64'; foreign_handler='qemu-aarch64'
    foreign_ref='docker.io/library/alpine@sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37'
    ;;
  *) echo "unsupported native live-test architecture: ${native_arch}" >&2; exit 1 ;;
esac
project='epar-cross-architecture-live'
workdir="$(mktemp -d)"
native_image='local/epar-cross-arch-native:live'
foreign_image='local/epar-cross-arch-foreign:live'
cleanup() {
  env -u DOCKER_DEFAULT_PLATFORM docker compose --project-name "${project}" --file "${workdir}/compose.yml" down --remove-orphans --volumes >/dev/null 2>&1 || true
  docker image rm -f "${native_image}" "${foreign_image}" >/dev/null 2>&1 || true
  rm -rf -- "${workdir}"
}
trap cleanup EXIT
docker pull --platform "${native_platform}" "${native_ref}" >/dev/null
docker tag "${native_ref}" "${native_image}"
docker pull --platform "${foreign_platform}" "${foreign_ref}" >/dev/null
docker tag "${foreign_ref}" "${foreign_image}"
[[ "$(docker run --rm --network none "${native_image}" uname -m)" == "${native_expected}" ]]
[[ "$(docker run --rm --network none --platform "${foreign_platform}" "${foreign_image}" uname -m)" == "${foreign_expected}" ]]
cat >"${workdir}/compose.yml" <<YAML
services:
  native-server:
    image: ${native_image}
    pull_policy: never
    command: ["sh", "-ec", "uname -m >/arch; printf '#!/bin/sh\\ncat /arch\\n' >/serve; chmod 555 /serve; exec nc -lk -p 8080 -e /serve"]
    healthcheck: {test: ["CMD", "nc", "-z", "127.0.0.1", "8080"], interval: 1s, timeout: 2s, retries: 30}
  foreign-auto:
    image: ${foreign_image}
    pull_policy: never
    command: ["sh", "-ec", "uname -m >/arch; printf '#!/bin/sh\\ncat /arch\\n' >/serve; chmod 555 /serve; exec nc -lk -p 8080 -e /serve"]
    healthcheck: {test: ["CMD", "nc", "-z", "127.0.0.1", "8080"], interval: 1s, timeout: 2s, retries: 30}
  foreign-explicit:
    image: ${foreign_image}
    pull_policy: never
    platform: ${foreign_platform}
    command: ["sh", "-ec", "uname -m >/arch; printf '#!/bin/sh\\ncat /arch\\n' >/serve; chmod 555 /serve; exec nc -lk -p 8080 -e /serve"]
    healthcheck: {test: ["CMD", "nc", "-z", "127.0.0.1", "8080"], interval: 1s, timeout: 2s, retries: 30}
  native-verifier:
    image: ${native_image}
    pull_policy: never
    depends_on:
      native-server: {condition: service_healthy}
      foreign-auto: {condition: service_healthy}
      foreign-explicit: {condition: service_healthy}
    command: ["sh", "-ec", "test \"\$(uname -m)\" = ${native_expected}; test \"\$(nc native-server 8080)\" = ${native_expected}; test \"\$(nc foreign-auto 8080)\" = ${foreign_expected}; test \"\$(nc foreign-explicit 8080)\" = ${foreign_expected}; touch /tmp/verified; exec sleep 300"]
    healthcheck: {test: ["CMD", "test", "-f", "/tmp/verified"], interval: 1s, timeout: 2s, retries: 30}
YAML
env -u DOCKER_DEFAULT_PLATFORM docker compose --project-name "${project}" --file "${workdir}/compose.yml" up --detach --wait
grep -Fx enabled "/proc/sys/fs/binfmt_misc/${foreign_handler}" >/dev/null
grep -Fx "interpreter /opt/epar/emulation/${foreign_handler}" "/proc/sys/fs/binfmt_misc/${foreign_handler}" >/dev/null
[[ "$(docker exec "$(env -u DOCKER_DEFAULT_PLATFORM docker compose --project-name "${project}" --file "${workdir}/compose.yml" ps -q native-server)" uname -m)" == "${native_expected}" ]]
[[ "$(docker exec "$(env -u DOCKER_DEFAULT_PLATFORM docker compose --project-name "${project}" --file "${workdir}/compose.yml" ps -q foreign-auto)" uname -m)" == "${foreign_expected}" ]]
env -u DOCKER_DEFAULT_PLATFORM docker compose --project-name "${project}" --file "${workdir}/compose.yml" down --remove-orphans --volumes
[[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=${project}")" ]]
[[ -z "$(docker network ls -q --filter "label=com.docker.compose.project=${project}")" ]]
trap - EXIT
docker image rm -f "${native_image}" "${foreign_image}" >/dev/null
rm -rf -- "${workdir}"
printf 'native=%s foreign=%s backend=qemu mixed-compose=passed\n' "${native_platform}" "${foreign_platform}"
`
