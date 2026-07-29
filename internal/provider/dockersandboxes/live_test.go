package dockersandboxes

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	sandboxpolicy "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/policy"
	sandboxfs "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/staging"
)

func TestLiveRunnerTemplateIsolation(t *testing.T) {
	if os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES") != "1" {
		t.Skip("set EPAR_LIVE_DOCKER_SANDBOXES=1 to run the destructive live Docker Sandboxes proof")
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
	name := fmt.Sprintf("epar-live-%d", time.Now().UnixNano())
	ownedStaging, err := staging.CreateOwned(name)
	if err != nil {
		t.Fatal(err)
	}
	path := ownedStaging.Path
	rootDisk := os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_ROOT_SIZE")
	if rootDisk == "" {
		rootDisk = "30GiB"
	}
	dockerDisk := os.Getenv("EPAR_LIVE_DOCKER_SANDBOXES_DOCKER_SIZE")
	if dockerDisk == "" {
		dockerDisk = "100GiB"
	}
	p := New("sbx")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	createStarted := time.Now()
	instance, err := p.Create(ctx, provider.CreateRequest{
		Name:           name,
		Template:       template,
		TemplateDigest: digest,
		StagingPath:    path,
		CPUs:           4,
		Memory:         "8GiB",
		RootDisk:       rootDisk,
		DockerDisk:     dockerDisk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cached sandbox create duration=%s", time.Since(createStarted))
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		cleanupStarted := time.Now()
		if stopErr := p.Stop(cleanupCtx, instance); stopErr != nil {
			t.Errorf("stop exact live sandbox: %v", stopErr)
		}
		if deleteErr := p.Delete(cleanupCtx, instance); deleteErr != nil {
			t.Errorf("delete exact live sandbox: %v", deleteErr)
		}
		if _, verifyErr := staging.VerifyOwnedEmpty(name, ownedStaging.Identity); verifyErr != nil {
			t.Errorf("verify live staging remains empty: %v", verifyErr)
			return
		}
		if removeErr := staging.RemoveEmptyOwned(name, ownedStaging.Identity); removeErr != nil {
			t.Errorf("remove exact live staging: %v", removeErr)
		}
		t.Logf("exact stop, force-remove, and staging absence duration=%s", time.Since(cleanupStarted))
	}()
	if _, err := p.Start(ctx, instance, provider.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	runtimeInfo, err := p.VerifyRuntime(ctx, instance)
	if err != nil || !runtimeInfo.Ready || runtimeInfo.Runtime != "docker" || runtimeInfo.Version == "" {
		t.Fatalf("private Docker runtime = %#v, error = %v", runtimeInfo, err)
	}
	daemonManagement, err := p.Exec(ctx, instance, []string{"bash", "-lc", `set -euo pipefail
mapfile -t pids < <(pgrep -x dockerd)
[[ "${#pids[@]}" == "1" ]]
ps -o pid=,ppid=,args= -p "${pids[0]}"
printf 'pid1='
tr '\0' ' ' </proc/1/cmdline
printf '\nsystemctl='
if command -v systemctl >/dev/null 2>&1; then systemctl is-active docker 2>&1 || true; else printf 'missing'; fi
printf '\nservice='
if command -v service >/dev/null 2>&1; then service docker status 2>&1 || true; else printf 'missing'; fi
printf '\n'`}, provider.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("private Docker daemon management:\n%s", strings.TrimSpace(daemonManagement.Stdout))
	if _, err := p.Exec(ctx, instance, []string{"bash", "/opt/epar/verify-template.sh"}, provider.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	diskUsage, err := p.Exec(ctx, instance, []string{"df", "-B1", "--output=used,target", "/", "/var/lib/docker"}, provider.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sandbox disk usage before representative nested workload:\n%s", strings.TrimSpace(diskUsage.Stdout))
	policyRules, err := p.ReadNetworkPolicy(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	globalRules := make([]provider.NetworkPolicyRule, 0, len(policyRules))
	for _, rule := range policyRules {
		if rule.Scope == "global" && rule.AppliesTo == "all" {
			globalRules = append(globalRules, rule)
		}
	}
	policyFingerprint, err := sandboxpolicy.Fingerprint(globalRules)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandboxpolicy.VerifyBaseline(policyFingerprint, name, policyRules); err != nil {
		t.Fatal(err)
	}
	effectiveFingerprint, err := sandboxpolicy.Fingerprint(policyRules)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Docker Sandboxes global policy fingerprint=%s complete effective fingerprint=%s rules=%#v", policyFingerprint, effectiveFingerprint, policyRules)
	containerName := name + "-nested"
	if _, err := p.Exec(ctx, instance, []string{"docker", "run", "--name", containerName, "-d", "docker.io/library/alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce", "sleep", "300"}, provider.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = p.Exec(context.Background(), instance, []string{"docker", "rm", "-f", containerName}, provider.ExecOptions{})
	}()
	hostResult, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").CombinedOutput()
	if err != nil {
		t.Fatalf("read host Docker inventory: %v: %s", err, hostResult)
	}
	for _, hostName := range strings.Fields(string(hostResult)) {
		if hostName == containerName {
			t.Fatalf("nested Docker container %q appeared in the host Docker daemon", containerName)
		}
	}
	guestResult, err := p.Exec(ctx, instance, []string{"docker", "inspect", "--format", "{{.Name}}", containerName}, provider.ExecOptions{})
	if err != nil || strings.TrimSpace(guestResult.Stdout) != "/"+containerName {
		t.Fatalf("nested Docker container was not confined to the guest daemon: result=%q error=%v", guestResult.Stdout, err)
	}
	representativeStarted := time.Now()
	representativeResult, err := p.Exec(ctx, instance, []string{"bash", "-lc", `set -euo pipefail
project="$1"
workdir="$(mktemp -d)"
image="local/${project}-buildx:probe"
cleanup() {
  docker compose --project-name "$project" --file "$workdir/compose.yml" down --remove-orphans --volumes >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf -- "$workdir"
}
trap cleanup EXIT
mkdir -p "$workdir/build"
printf 'FROM scratch\nLABEL org.opencontainers.image.title="EPAR Docker Sandboxes Buildx probe"\n' >"$workdir/build/Dockerfile"
cat >"$workdir/compose.yml" <<'YAML'
services:
  probe:
    image: docker.io/library/alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
    command: ["sleep", "120"]
YAML
docker buildx version
docker buildx inspect default
docker buildx build --builder default --load --tag "$image" "$workdir/build"
docker image inspect "$image" >/dev/null
docker compose version
docker compose --project-name "$project" --file "$workdir/compose.yml" up --detach
[[ "$(docker compose --project-name "$project" --file "$workdir/compose.yml" ps --status running --services)" == "probe" ]]
docker compose --project-name "$project" --file "$workdir/compose.yml" down --remove-orphans --volumes
trap - EXIT
docker image rm "$image" >/dev/null
rm -rf -- "$workdir"
`, "--", name + "-compose"}, provider.ExecOptions{})
	if err != nil {
		t.Fatalf("representative Buildx and Compose workload failed: %v\n%s", err, representativeResult.Stderr)
	}
	t.Logf("representative Buildx and Compose workload duration=%s\n%s", time.Since(representativeStarted), strings.TrimSpace(representativeResult.Stdout))
	diskUsageAfter, err := p.Exec(ctx, instance, []string{"df", "-B1", "--output=used,target", "/", "/var/lib/docker"}, provider.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sandbox disk usage after representative nested workload:\n%s", strings.TrimSpace(diskUsageAfter.Stdout))
	if _, err := os.Lstat(filepath.Join(path, ".runner")); err == nil {
		t.Fatal("runner registration state escaped into the canonical host staging directory")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
