package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type ImageBuildOptions struct {
	Replace           bool
	SkipUpstreamCheck bool
	Manifest          *ImageManifest
}

type runnerImagesCopyMode int

const (
	runnerImagesCopyNone runnerImagesCopyMode = iota
	runnerImagesCopySubset
)

type wslExporter interface {
	Export(ctx context.Context, name, outputPath string) error
}

var (
	runHostLoggedCommand = runHostLogged
	runHostOutputCommand = runHostOutput
	runHostQuietCommand  = runHostQuiet
)

func (m *Manager) UpdateUpstream(ctx context.Context) error {
	dir := config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamDir)
	fmt.Printf("updating runner-images checkout at %s\n", dir)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if err := runHost(ctx, "git", "-C", dir, "fetch", "--depth", "1", "origin", "main"); err != nil {
			return err
		}
		if err := runHost(ctx, "git", "-C", dir, "checkout", "FETCH_HEAD"); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
			return err
		}
		if err := runHost(ctx, "git", "clone", "--depth", "1", "https://github.com/actions/runner-images.git", dir); err != nil {
			return err
		}
	}
	commitBytes, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return err
	}
	lockPath := config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamLock)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return err
	}
	commit := strings.TrimSpace(string(commitBytes))
	if err := os.WriteFile(lockPath, []byte(commit+"\n"), 0644); err != nil {
		return err
	}
	fmt.Printf("runner-images pinned at %s\n", commit)
	fmt.Printf("lock file written to %s\n", lockPath)
	return nil
}

func (m *Manager) BuildImage(ctx context.Context, opts ImageBuildOptions) error {
	if _, err := m.trustedCACertificates(); err != nil {
		return err
	}
	upstreamDir := config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamDir)
	copyMode := m.runnerImagesCopyMode()
	if !opts.SkipUpstreamCheck && copyMode != runnerImagesCopyNone {
		for _, name := range m.runnerImageBuildScripts() {
			if _, err := os.Stat(filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "build", name)); err != nil {
				return fmt.Errorf("runner-images checkout missing script %s; run `ephemeral-action-runner image update-upstream` first: %w", name, err)
			}
		}
	}
	switch m.Config.Provider.Type {
	case "tart":
		return m.buildTartImage(ctx, opts, upstreamDir)
	case "wsl":
		return m.buildWSLImage(ctx, opts, upstreamDir)
	case "docker-dind":
		return m.buildDockerDindImage(ctx, opts, upstreamDir)
	default:
		return fmt.Errorf("unsupported provider.type %q", m.Config.Provider.Type)
	}
}

func (m *Manager) buildDockerDindImage(ctx context.Context, opts ImageBuildOptions, upstreamDir string) error {
	buildLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".docker-build.log")
	if err := resetLogs(buildLogPath); err != nil {
		return err
	}
	if !m.DryRun && !opts.Replace {
		if err := exec.CommandContext(ctx, "docker", "image", "inspect", m.Config.Image.OutputImage).Run(); err == nil {
			return fmt.Errorf("docker image %s already exists; rerun with --replace", m.Config.Image.OutputImage)
		}
	}
	if opts.Manifest == nil {
		manifest, err := m.desiredImageManifest(ctx)
		if err != nil {
			return err
		}
		opts.Manifest = &manifest
	}
	manifestContent, manifestHash, err := storedImageManifestContent(*opts.Manifest)
	if err != nil {
		return err
	}
	buildCtx, err := os.MkdirTemp("", "epar-docker-dind-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(buildCtx)
	if err := m.prepareDockerDindBuildContext(buildCtx, upstreamDir, manifestContent); err != nil {
		return err
	}
	fmt.Printf("building Docker-DinD image %s from %s\n", m.Config.Image.OutputImage, m.Config.Image.SourceImage)
	fmt.Printf("log: %s\n", buildLogPath)
	args := []string{"build", "-t", m.Config.Image.OutputImage}
	if m.Config.Provider.Platform != "" {
		args = append(args, "--platform", m.Config.Provider.Platform)
	}
	args = append(args,
		"--build-arg", "BASE_IMAGE="+m.Config.Image.SourceImage,
		"--build-arg", "RUNNER_VERSION="+m.Config.Image.RunnerVersion,
		"--build-arg", "EPAR_IMAGE_MANIFEST_SHA256="+manifestHash,
		buildCtx,
	)
	if m.DryRun {
		fmt.Printf("[dry-run] docker %s\n", strings.Join(args, " "))
		fmt.Printf("image build dry run complete: %s\n", m.Config.Image.OutputImage)
		return nil
	}
	if err := runHostLogged(ctx, buildLogPath, "docker", args...); err != nil {
		return err
	}
	fmt.Printf("image build complete: %s is available in `docker image ls`\n", m.Config.Image.OutputImage)
	return nil
}

func (m *Manager) buildTartImage(ctx context.Context, opts ImageBuildOptions, upstreamDir string) error {
	if opts.Manifest == nil {
		manifest, err := m.desiredImageManifest(ctx)
		if err != nil {
			return err
		}
		opts.Manifest = &manifest
	}
	buildLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".build.log")
	guestLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".guest.log")
	if err := resetLogs(buildLogPath, guestLogPath); err != nil {
		return err
	}
	fmt.Printf("building Tart image %s from %s\n", m.Config.Image.OutputImage, m.Config.Image.SourceImage)
	fmt.Printf("logs: %s, %s\n", buildLogPath, guestLogPath)
	if opts.Replace {
		fmt.Printf("replacing existing Tart image %s if present\n", m.Config.Image.OutputImage)
		_ = m.Provider.Stop(ctx, m.Config.Image.OutputImage)
		_ = m.Provider.Delete(ctx, m.Config.Image.OutputImage)
	}
	fmt.Printf("cloning source image\n")
	if err := m.Provider.Clone(ctx, m.Config.Image.SourceImage, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("starting image with Tart network mode %q\n", m.Config.Provider.Network)
	if _, err := m.Provider.Start(ctx, m.Config.Image.OutputImage, m.startOptions(buildLogPath)); err != nil {
		return err
	}
	ip, err := m.Provider.IP(ctx, m.Config.Image.OutputImage, m.Config.Timeouts.BootSeconds)
	if err != nil {
		return err
	}
	fmt.Printf("guest reachable at %s\n", ip)
	fmt.Printf("copying guest scripts\n")
	if err := m.installGuestScripts(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	if err := m.installTrustedCACertificates(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	if err := m.installImageManifest(ctx, m.Config.Image.OutputImage, *opts.Manifest); err != nil {
		return err
	}
	switch m.runnerImagesCopyMode() {
	case runnerImagesCopySubset:
		fmt.Printf("copying runner-images script subset\n")
		if err := m.copyRunnerImagesSubset(ctx, m.Config.Image.OutputImage, upstreamDir); err != nil {
			return err
		}
	case runnerImagesCopyNone:
		fmt.Printf("skipping runner-images script subset; no selected install script requires it\n")
	}
	fmt.Printf("installing base runner runtime\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/install-base.sh", "/opt/epar/upstream/runner-images"}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("installing GitHub Actions runner\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/install-runner.sh", m.Config.Image.RunnerVersion}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.installRosettaSupport(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	if err := m.installCustomInstallScripts(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("validating runner runtime inside the instance\n")
	if err := m.validateRuntime(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("finalizing image for clean Tart clones\n")
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("stopping image\n")
	if err := m.Provider.Stop(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("image build complete: %s is available in `tart list`\n", m.Config.Image.OutputImage)
	return nil
}

func (m *Manager) buildWSLImage(ctx context.Context, opts ImageBuildOptions, upstreamDir string) error {
	exporter, ok := m.Provider.(wslExporter)
	if !ok {
		return fmt.Errorf("provider.type=wsl requires provider export support")
	}
	outputPath := config.ProjectPath(m.ProjectRoot, m.Config.Image.OutputImage)
	sourceType := m.Config.Image.SourceType
	if sourceType == "" {
		sourceType = config.ImageSourceRootFSTar
	}
	buildName := RunnerName(m.Config.Pool.NamePrefix+"-image", 1, time.Now())
	buildLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".wsl-build.log")
	guestLogPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), buildName+".guest.log")
	if err := resetLogs(buildLogPath, guestLogPath); err != nil {
		return err
	}
	if !m.DryRun {
		if _, err := os.Stat(outputPath); err == nil && !opts.Replace {
			return fmt.Errorf("wsl output image %s already exists; rerun with --replace", outputPath)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if opts.Replace {
			if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Remove(wslImageManifestSidecarPath(outputPath)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if opts.Manifest == nil {
		manifest, err := m.desiredImageManifest(ctx)
		if err != nil {
			return err
		}
		opts.Manifest = &manifest
	}

	sourceForClone := m.Config.Image.SourceImage
	sourcePath := config.ProjectPath(m.ProjectRoot, sourceForClone)
	sourceEnv := ""
	switch sourceType {
	case config.ImageSourceDockerImage:
		rootfsPath, env, err := m.prepareWSLDockerSourceRootfs(ctx, outputPath, buildLogPath, *opts.Manifest)
		if err != nil {
			return err
		}
		sourceForClone = rootfsPath
		sourcePath = rootfsPath
		sourceEnv = env
	case config.ImageSourceRootFSTar:
		if !m.DryRun {
			if _, err := os.Stat(sourcePath); err != nil {
				return fmt.Errorf("wsl source image %s: %w", sourcePath, err)
			}
		}
	default:
		return fmt.Errorf("unsupported WSL image.sourceType %q", sourceType)
	}

	fmt.Printf("building WSL image %s from %s using temporary distro %s\n", outputPath, sourcePath, buildName)
	fmt.Printf("logs: %s, %s\n", buildLogPath, guestLogPath)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = m.Provider.Stop(cleanupCtx, buildName)
		_ = m.Provider.Delete(cleanupCtx, buildName)
	}()
	fmt.Printf("importing source rootfs\n")
	if err := m.Provider.Clone(ctx, sourceForClone, buildName); err != nil {
		return err
	}
	if sourceType == config.ImageSourceDockerImage {
		fmt.Printf("preparing Docker image rootfs for WSL systemd\n")
		if err := m.prepareWSLDockerSourceGuest(ctx, buildName); err != nil {
			return err
		}
	}
	fmt.Printf("enabling WSL systemd\n")
	if err := m.enableWSLSystemd(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("restarting temporary distro for systemd\n")
	if err := m.Provider.Stop(ctx, buildName); err != nil {
		return err
	}
	if _, err := m.Provider.Start(ctx, buildName, m.startOptions(buildLogPath)); err != nil {
		return err
	}
	ip, err := m.Provider.IP(ctx, buildName, m.Config.Timeouts.BootSeconds)
	if err != nil {
		return err
	}
	fmt.Printf("guest reachable at %s\n", ip)
	if err := m.waitForSystemd(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("copying guest scripts\n")
	if err := m.installGuestScripts(ctx, buildName); err != nil {
		return err
	}
	if err := m.installTrustedCACertificates(ctx, buildName); err != nil {
		return err
	}
	if err := m.installImageManifest(ctx, buildName, *opts.Manifest); err != nil {
		return err
	}
	if sourceEnv != "" {
		fmt.Printf("installing source image environment metadata\n")
		if err := m.installSourceImageEnv(ctx, buildName, sourceEnv); err != nil {
			return err
		}
	}
	switch m.runnerImagesCopyMode() {
	case runnerImagesCopySubset:
		fmt.Printf("copying runner-images script subset\n")
		if err := m.copyRunnerImagesSubset(ctx, buildName, upstreamDir); err != nil {
			return err
		}
	case runnerImagesCopyNone:
		fmt.Printf("skipping runner-images script subset; no selected install script requires it\n")
	}
	fmt.Printf("installing base runner runtime\n")
	if _, err := m.execGuest(ctx, buildName, []string{"sudo", "bash", "/opt/epar/install-base.sh", "/opt/epar/upstream/runner-images"}, provider.ExecOptions{}); err != nil {
		return err
	}
	fmt.Printf("installing GitHub Actions runner\n")
	if _, err := m.execGuest(ctx, buildName, []string{"sudo", "bash", "/opt/epar/install-runner.sh", m.Config.Image.RunnerVersion}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.installWSLDockerEngine(ctx, buildName); err != nil {
		return err
	}
	if err := m.installCustomInstallScripts(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("validating runner runtime inside the distro\n")
	if err := m.validateRuntime(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("finalizing image for clean WSL imports\n")
	if _, err := m.execGuest(ctx, buildName, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.Provider.Stop(ctx, buildName); err != nil {
		return err
	}
	fmt.Printf("exporting reusable WSL image to %s\n", outputPath)
	if err := exporter.Export(ctx, buildName, m.Config.Image.OutputImage); err != nil {
		return err
	}
	if !m.DryRun {
		if err := writeStoredImageManifest(wslImageManifestSidecarPath(outputPath), *opts.Manifest); err != nil {
			return err
		}
	}
	fmt.Printf("image build complete: %s is available for WSL imports\n", outputPath)
	return nil
}

func (m *Manager) prepareWSLDockerSourceRootfs(ctx context.Context, outputPath, buildLogPath string, manifest ImageManifest) (string, string, error) {
	image := strings.TrimSpace(m.Config.Image.SourceImage)
	if image == "" {
		return "", "", fmt.Errorf("image.sourceImage is required when image.sourceType=docker-image")
	}
	rootfsPath := wslDockerSourceRootfsPath(outputPath)
	envCachePath := rootfsPath + ".env"
	sourceCachePath := sourceCacheManifestPath(rootfsPath)
	tmpPath := rootfsPath + ".tmp"
	platform := strings.TrimSpace(m.Config.Image.SourcePlatform)
	sourceCache := sourceCacheManifest{SourceImage: image, SourcePlatform: platform, SourceDigest: manifest.SourceDigest}
	containerName := wslDockerSourceContainerName()
	pullArgs := []string{"pull"}
	createArgs := []string{"create"}
	if platform != "" {
		pullArgs = append(pullArgs, "--platform", platform)
		createArgs = append(createArgs, "--platform", platform)
	}
	pullArgs = append(pullArgs, image)
	createArgs = append(createArgs, "--name", containerName, image)
	exportArgs := []string{"export", "-o", tmpPath, containerName}

	if m.DryRun {
		fmt.Printf("[dry-run] docker %s\n", strings.Join(pullArgs, " "))
		fmt.Printf("[dry-run] docker %s\n", strings.Join(createArgs, " "))
		fmt.Printf("[dry-run] docker container inspect --format {{json .Config.Env}} %s\n", containerName)
		fmt.Printf("[dry-run] docker %s\n", strings.Join(exportArgs, " "))
		fmt.Printf("[dry-run] docker rm -f %s\n", containerName)
		return rootfsPath, "", nil
	}

	if info, err := os.Stat(rootfsPath); err == nil && info.Size() > 0 && sourceCacheMatches(sourceCachePath, sourceCache) {
		envContent, err := os.ReadFile(envCachePath)
		if err != nil {
			fmt.Printf("using cached WSL source rootfs at %s; source image env cache missing, refreshing metadata from Docker image\n", rootfsPath)
			refreshedEnv, refreshErr := m.dockerImageEnvContent(ctx, image)
			if refreshErr != nil {
				fmt.Printf("warning: could not refresh WSL source image env metadata: %v\n", refreshErr)
				return rootfsPath, "", nil
			}
			if writeErr := os.WriteFile(envCachePath, []byte(refreshedEnv), 0644); writeErr != nil {
				return "", "", writeErr
			}
			return rootfsPath, refreshedEnv, nil
		}
		fmt.Printf("using cached WSL source rootfs at %s\n", rootfsPath)
		return rootfsPath, string(envContent), nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	} else if err == nil {
		fmt.Printf("cached WSL source rootfs is missing source metadata or no longer matches; reconverting %s\n", image)
		if err := os.Remove(rootfsPath); err != nil && !os.IsNotExist(err) {
			return "", "", err
		}
		if err := os.Remove(envCachePath); err != nil && !os.IsNotExist(err) {
			return "", "", err
		}
		if err := os.Remove(sourceCachePath); err != nil && !os.IsNotExist(err) {
			return "", "", err
		}
	}

	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0755); err != nil {
		return "", "", err
	}
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	fmt.Printf("preparing WSL source rootfs from Docker image %s\n", image)
	if err := runHostLoggedCommand(ctx, buildLogPath, "docker", pullArgs...); err != nil {
		return "", "", fmt.Errorf("wsl image.sourceType=docker-image requires Docker Desktop, Docker Engine, or another reachable Docker daemon; alternatively set image.sourceType=rootfs-tar and provide a prepared rootfs tar: %w", err)
	}
	if err := runHostLoggedCommand(ctx, buildLogPath, "docker", createArgs...); err != nil {
		return "", "", err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = runHostQuietCommand(cleanupCtx, "docker", "rm", "-f", containerName)
	}()
	envJSON, err := runHostOutputCommand(ctx, "docker", "container", "inspect", "--format", "{{json .Config.Env}}", containerName)
	if err != nil {
		return "", "", err
	}
	var sourceEnv []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(envJSON)), &sourceEnv); err != nil {
		return "", "", fmt.Errorf("parse Docker source image environment: %w", err)
	}
	envContent := sourceImageEnvContent(sourceEnv)
	if err := runHostLoggedCommand(ctx, buildLogPath, "docker", exportArgs...); err != nil {
		return "", "", err
	}
	if err := os.Remove(rootfsPath); err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	if err := os.Rename(tmpPath, rootfsPath); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(envCachePath, []byte(envContent), 0644); err != nil {
		return "", "", err
	}
	if err := writeSourceCacheManifest(sourceCachePath, sourceCache); err != nil {
		return "", "", err
	}
	fmt.Printf("WSL source rootfs exported to %s\n", rootfsPath)
	return rootfsPath, envContent, nil
}

func (m *Manager) dockerImageEnvContent(ctx context.Context, image string) (string, error) {
	envJSON, err := runHostOutputCommand(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Env}}", image)
	if err != nil {
		return "", err
	}
	var sourceEnv []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(envJSON)), &sourceEnv); err != nil {
		return "", fmt.Errorf("parse Docker source image environment: %w", err)
	}
	return sourceImageEnvContent(sourceEnv), nil
}

func wslDockerSourceRootfsPath(outputPath string) string {
	switch {
	case strings.HasSuffix(outputPath, ".tar.gz"):
		return strings.TrimSuffix(outputPath, ".tar.gz") + ".source.rootfs.tar"
	case strings.HasSuffix(outputPath, ".tgz"):
		return strings.TrimSuffix(outputPath, ".tgz") + ".source.rootfs.tar"
	case strings.HasSuffix(outputPath, ".tar"):
		return strings.TrimSuffix(outputPath, ".tar") + ".source.rootfs.tar"
	default:
		return outputPath + ".source.rootfs.tar"
	}
}

func wslDockerSourceContainerName() string {
	return fmt.Sprintf("epar-wsl-source-%d-%d", os.Getpid(), time.Now().UnixNano())
}

func (m *Manager) installSourceImageEnv(ctx context.Context, vmName, content string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	return provider.CopyText(ctx, m.Provider, vmName, "/opt/epar/source-image.env", "0644", content)
}

func (m *Manager) prepareWSLDockerSourceGuest(ctx context.Context, vmName string) error {
	script := `set -euo pipefail
cat >/etc/fstab <<'FSTAB'
# EPAR: Docker image rootfs prepared for WSL imports.
FSTAB

install -d /etc/skel/.cargo
if [[ ! -e /etc/skel/.cargo/env ]]; then
  if [[ -r /home/runner/.cargo/env ]]; then
    cp /home/runner/.cargo/env /etc/skel/.cargo/env
  else
    : >/etc/skel/.cargo/env
  fi
fi
chmod 0644 /etc/skel/.cargo/env

if [[ -d /etc/cloud ]]; then
  touch /etc/cloud/cloud-init.disabled
fi

install -d /etc/systemd/system
for unit in \
  cloud-config.service \
  cloud-final.service \
  cloud-init-local.service \
  cloud-init.service \
  hv-kvp-daemon.service \
  walinuxagent-network-setup.service \
  walinuxagent.service; do
  ln -sf /dev/null "/etc/systemd/system/${unit}"
done
`
	_, err := m.Provider.Exec(ctx, vmName, []string{"bash", "-c", script}, provider.ExecOptions{})
	return err
}

func (m *Manager) installWSLDockerEngine(ctx context.Context, vmName string) error {
	if m.Config.Provider.Type != "wsl" || m.Config.Image.SourceType != config.ImageSourceDockerImage {
		return nil
	}
	fmt.Printf("validating Docker Engine from WSL Docker source image\n")
	_, err := m.execGuest(ctx, vmName, []string{"sudo", "-E", "bash", "/opt/epar/install-docker-engine.sh", "/opt/epar/upstream/runner-images"}, provider.ExecOptions{
		Env: map[string]string{"EPAR_REQUIRE_BASE_DOCKER_ENGINE": "true"},
	})
	return err
}

func sourceImageEnvContent(env []string) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Generated by EPAR from Docker source image metadata.\n")
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok || !validShellEnvName(key) {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", key, shellQuote(value))
	}
	return b.String()
}

func validShellEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
		if i == 0 && r >= '0' && r <= '9' {
			return false
		}
	}
	return true
}

func (m *Manager) RefreshScripts(ctx context.Context) error {
	switch m.Config.Provider.Type {
	case "tart":
		return m.refreshTartScripts(ctx)
	case "wsl":
		return m.refreshWSLScripts(ctx)
	case "docker-dind":
		return m.buildDockerDindImage(ctx, ImageBuildOptions{Replace: true}, config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamDir))
	default:
		return fmt.Errorf("unsupported provider.type %q", m.Config.Provider.Type)
	}
}

func (m *Manager) refreshTartScripts(ctx context.Context) error {
	logPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".refresh.log")
	fmt.Printf("refreshing guest scripts in Tart image %s\n", m.Config.Image.OutputImage)
	fmt.Printf("log: %s\n", logPath)
	if _, err := m.Provider.Start(ctx, m.Config.Image.OutputImage, m.startOptions(logPath)); err != nil {
		return err
	}
	shouldStop := true
	defer func() {
		if shouldStop {
			stopCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_ = m.Provider.Stop(stopCtx, m.Config.Image.OutputImage)
		}
	}()
	if _, err := m.Provider.IP(ctx, m.Config.Image.OutputImage, m.Config.Timeouts.BootSeconds); err != nil {
		return err
	}
	if err := m.installGuestScripts(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	if _, err := m.execGuest(ctx, m.Config.Image.OutputImage, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	if _, err := m.Provider.Exec(ctx, m.Config.Image.OutputImage, provider.ShellCommand("sync"), provider.ExecOptions{LogPath: logPath}); err != nil {
		return err
	}
	shouldStop = false
	if err := m.Provider.Stop(ctx, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("script refresh complete: %s is available in `tart list`\n", m.Config.Image.OutputImage)
	return nil
}

func (m *Manager) refreshWSLScripts(ctx context.Context) error {
	exporter, ok := m.Provider.(wslExporter)
	if !ok {
		return fmt.Errorf("provider.type=wsl requires provider export support")
	}
	imagePath := config.ProjectPath(m.ProjectRoot, m.Config.Image.OutputImage)
	if !m.DryRun {
		if _, err := os.Stat(imagePath); err != nil {
			return fmt.Errorf("wsl image %s: %w", imagePath, err)
		}
	}
	name := RunnerName(m.Config.Pool.NamePrefix+"-refresh", 1, time.Now())
	logPath := filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), imageLogStem(m.Config.Image.OutputImage)+".wsl-refresh.log")
	fmt.Printf("refreshing guest scripts in WSL image %s using temporary distro %s\n", imagePath, name)
	fmt.Printf("log: %s\n", logPath)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = m.Provider.Stop(cleanupCtx, name)
		_ = m.Provider.Delete(cleanupCtx, name)
	}()
	if err := m.Provider.Clone(ctx, m.Config.Image.OutputImage, name); err != nil {
		return err
	}
	if _, err := m.Provider.Start(ctx, name, m.startOptions(logPath)); err != nil {
		return err
	}
	if _, err := m.Provider.IP(ctx, name, m.Config.Timeouts.BootSeconds); err != nil {
		return err
	}
	if err := m.waitForSystemd(ctx, name); err != nil {
		return err
	}
	if err := m.installGuestScripts(ctx, name); err != nil {
		return err
	}
	if _, err := m.execGuest(ctx, name, []string{"sudo", "bash", "/opt/epar/finalize-image.sh"}, provider.ExecOptions{}); err != nil {
		return err
	}
	if err := m.Provider.Stop(ctx, name); err != nil {
		return err
	}
	if err := exporter.Export(ctx, name, m.Config.Image.OutputImage); err != nil {
		return err
	}
	fmt.Printf("script refresh complete: %s is available for WSL imports\n", imagePath)
	return nil
}

func (m *Manager) installGuestScripts(ctx context.Context, vmName string) error {
	scriptDir := filepath.Join(m.ProjectRoot, "scripts", "guest", "ubuntu")
	entries, err := os.ReadDir(scriptDir)
	if err != nil {
		return err
	}
	if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand("if command -v sudo >/dev/null 2>&1; then sudo mkdir -p /opt/epar; else mkdir -p /opt/epar; fi"), provider.ExecOptions{}); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sh") {
			continue
		}
		path := filepath.Join(scriptDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := provider.CopyText(ctx, m.Provider, vmName, "/opt/epar/"+entry.Name(), "0755", guestText(content)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) startOptions(logPath string) provider.StartOptions {
	return provider.StartOptions{
		Network:    m.Config.Provider.Network,
		RosettaTag: m.Config.Provider.RosettaTag,
		LogPath:    logPath,
	}
}

func (m *Manager) installRosettaSupport(ctx context.Context, vmName string) error {
	if m.Config.Provider.Type != "tart" || strings.TrimSpace(m.Config.Provider.RosettaTag) == "" {
		return nil
	}
	fmt.Printf("installing Tart Rosetta amd64 support with tag %q\n", m.Config.Provider.RosettaTag)
	_, err := m.execGuest(ctx, vmName, []string{"sudo", "-E", "bash", "/opt/epar/install-rosetta.sh"}, provider.ExecOptions{
		Env: map[string]string{"EPAR_ROSETTA_TAG": m.Config.Provider.RosettaTag},
	})
	return err
}

func (m *Manager) copyRunnerImagesSubset(ctx context.Context, vmName, upstreamDir string) error {
	type copyRoot struct {
		host  string
		guest string
	}
	roots := []copyRoot{
		{
			host:  filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "helpers"),
			guest: "/opt/epar/upstream/runner-images/images/ubuntu/scripts/helpers",
		},
		{
			host:  filepath.Join(upstreamDir, "images", "ubuntu", "toolsets"),
			guest: "/opt/epar/upstream/runner-images/images/ubuntu/toolsets",
		},
	}
	for _, root := range roots {
		if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand(mkdirGuestCommand(root.guest)), provider.ExecOptions{}); err != nil {
			return err
		}
		if _, err := os.Stat(root.host); err != nil {
			if m.DryRun && os.IsNotExist(err) {
				fmt.Printf("[dry-run] skipping missing runner-images path %s\n", root.host)
				continue
			}
			return err
		}
		if err := filepath.WalkDir(root.host, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if root.guest != "" && strings.Contains(path, "/.git/") {
				return nil
			}
			rel, err := filepath.Rel(root.host, path)
			if err != nil {
				return err
			}
			guestPath := filepath.ToSlash(filepath.Join(root.guest, rel))
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand(mkdirGuestCommand(filepath.ToSlash(filepath.Dir(guestPath)))), provider.ExecOptions{}); err != nil {
				return err
			}
			return provider.CopyText(ctx, m.Provider, vmName, guestPath, "0644", guestText(content))
		}); err != nil {
			return err
		}
	}
	buildGuestDir := "/opt/epar/upstream/runner-images/images/ubuntu/scripts/build"
	if _, err := m.Provider.Exec(ctx, vmName, provider.ShellCommand(mkdirGuestCommand(buildGuestDir)), provider.ExecOptions{}); err != nil {
		return err
	}
	for _, name := range m.runnerImageBuildScripts() {
		hostPath := filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "build", name)
		content, err := os.ReadFile(hostPath)
		if err != nil {
			if m.DryRun && os.IsNotExist(err) {
				fmt.Printf("[dry-run] skipping missing runner-images file %s\n", hostPath)
				continue
			}
			return err
		}
		if err := provider.CopyText(ctx, m.Provider, vmName, buildGuestDir+"/"+name, "0644", guestText(content)); err != nil {
			return err
		}
	}
	if err := m.copyRunnerImagesCommitToGuest(ctx, vmName); err != nil {
		return err
	}
	return nil
}

func (m *Manager) copyRunnerImagesCommitToGuest(ctx context.Context, vmName string) error {
	commit, err := m.runnerImagesCommit()
	if err != nil {
		return err
	}
	if commit == "" {
		return nil
	}
	return provider.CopyText(ctx, m.Provider, vmName, "/opt/epar/upstream/runner-images/epar-commit", "0644", commit+"\n")
}

func (m *Manager) runnerImageBuildScripts() []string {
	return []string{"install-docker.sh", "install-google-chrome.sh", "install-nodejs.sh"}
}

func (m *Manager) runnerImagesCopyMode() runnerImagesCopyMode {
	for _, script := range m.Config.Image.CustomInstallScripts {
		normalized := m.normalizedCustomInstallScript(script)
		switch normalized {
		case "scripts/guest/ubuntu/install-docker-browser.sh",
			"scripts/guest/ubuntu/install-web-e2e.sh":
			return runnerImagesCopySubset
		}
	}
	return runnerImagesCopyNone
}

func (m *Manager) prepareDockerDindBuildContext(buildCtx, upstreamDir, manifestContent string) error {
	if err := copyDir(filepath.Join(m.ProjectRoot, "scripts", "guest", "ubuntu"), filepath.Join(buildCtx, "scripts", "guest", "ubuntu")); err != nil {
		return err
	}
	if err := copyDir(filepath.Join(m.ProjectRoot, "scripts", "container", "ubuntu"), filepath.Join(buildCtx, "scripts", "container", "ubuntu")); err != nil {
		return err
	}
	upstreamDest := filepath.Join(buildCtx, "upstream", "runner-images")
	if err := os.MkdirAll(upstreamDest, 0755); err != nil {
		return err
	}
	switch m.runnerImagesCopyMode() {
	case runnerImagesCopySubset:
		fmt.Printf("preparing Docker-DinD build context with runner-images script subset\n")
		if err := copyRunnerImagesSubsetToDir(upstreamDir, upstreamDest, m.runnerImageBuildScripts()); err != nil {
			return err
		}
		if err := m.writeRunnerImagesCommitFile(upstreamDest); err != nil {
			return err
		}
	case runnerImagesCopyNone:
		fmt.Printf("preparing Docker-DinD build context without runner-images resources\n")
	}
	customDir := filepath.Join(buildCtx, "custom-install")
	if err := os.MkdirAll(customDir, 0755); err != nil {
		return err
	}
	if err := m.copyTrustedCACertificatesToDir(filepath.Join(buildCtx, "trusted-ca-certificates")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildCtx, "image-manifest.json"), []byte(manifestContent), 0644); err != nil {
		return err
	}
	var customRuns strings.Builder
	for i, script := range m.Config.Image.CustomInstallScripts {
		hostPath, err := m.customInstallScriptHostPath(script)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%03d-%s", i+1, guestScriptName(filepath.Base(hostPath)))
		if err := copyFile(hostPath, filepath.Join(customDir, name), 0755); err != nil {
			return err
		}
		fmt.Fprintf(&customRuns, "RUN EPAR_CONTAINER_IMAGE_BUILD=true bash /opt/epar/custom-install/%s\n", name)
	}
	dockerfile := fmt.Sprintf(`ARG BASE_IMAGE=gitea/runner-images:ubuntu-latest-full
FROM ${BASE_IMAGE}
USER root
ARG RUNNER_VERSION=latest
ARG EPAR_IMAGE_MANIFEST_SHA256
ARG OCI_SOURCE=https://github.com/solutionforest/ephemeral-action-runner
ARG OCI_DESCRIPTION="EPAR Docker-DinD runner image"
ARG OCI_LICENSES=MIT
LABEL org.opencontainers.image.source="${OCI_SOURCE}"
LABEL org.opencontainers.image.description="${OCI_DESCRIPTION}"
LABEL org.opencontainers.image.licenses="${OCI_LICENSES}"
LABEL `+imageManifestLabel+`="${EPAR_IMAGE_MANIFEST_SHA256}"
ENV DEBIAN_FRONTEND=noninteractive
ENV NEEDRESTART_MODE=l
ENV NEEDRESTART_SUSPEND=1
COPY scripts/guest/ubuntu/ /opt/epar/
COPY scripts/container/ubuntu/entrypoint.sh /opt/epar/container-entrypoint.sh
COPY upstream/runner-images/ /opt/epar/upstream/runner-images/
COPY custom-install/ /opt/epar/custom-install/
COPY trusted-ca-certificates/ `+trustedCAGuestDir+`/
COPY image-manifest.json /opt/epar/image-manifest.json
RUN chmod +x /opt/epar/*.sh /opt/epar/container-entrypoint.sh /opt/epar/custom-install/*.sh 2>/dev/null || true
RUN bash /opt/epar/install-trusted-ca-certificates.sh
RUN bash /opt/epar/install-base.sh /opt/epar/upstream/runner-images
RUN bash /opt/epar/install-runner.sh "${RUNNER_VERSION}"
RUN EPAR_CONTAINER_IMAGE_BUILD=true bash /opt/epar/install-docker-engine.sh /opt/epar/upstream/runner-images
%sRUN EPAR_CONTAINER_IMAGE_BUILD=true bash /opt/epar/validate-runtime.sh
RUN bash /opt/epar/finalize-image.sh
ENTRYPOINT ["/opt/epar/container-entrypoint.sh"]
`, customRuns.String())
	return os.WriteFile(filepath.Join(buildCtx, "Dockerfile"), []byte(dockerfile), 0644)
}

func (m *Manager) normalizedCustomInstallScript(script string) string {
	script = strings.TrimSpace(script)
	if script == "" {
		return ""
	}
	if filepath.IsAbs(script) && m.ProjectRoot != "" {
		root, rootErr := filepath.Abs(m.ProjectRoot)
		path, pathErr := filepath.Abs(script)
		if rootErr == nil && pathErr == nil {
			if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				script = rel
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(script))
}

func (m *Manager) installCustomInstallScripts(ctx context.Context, vmName string) error {
	scripts := m.Config.Image.CustomInstallScripts
	if len(scripts) == 0 {
		return nil
	}
	fmt.Printf("running %d image install script(s)\n", len(scripts))
	if _, err := m.execGuest(ctx, vmName, provider.ShellCommand("if command -v sudo >/dev/null 2>&1; then sudo mkdir -p /opt/epar/custom-install; else mkdir -p /opt/epar/custom-install; fi"), provider.ExecOptions{}); err != nil {
		return err
	}
	for i, script := range scripts {
		hostPath, err := m.customInstallScriptHostPath(script)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(hostPath)
		if err != nil {
			return fmt.Errorf("read custom install script %s: %w", hostPath, err)
		}
		guestPath := fmt.Sprintf("/opt/epar/custom-install/%03d-%s", i+1, guestScriptName(filepath.Base(hostPath)))
		if err := provider.CopyText(ctx, m.Provider, vmName, guestPath, "0755", guestText(content)); err != nil {
			return err
		}
		fmt.Printf("running image install script %d/%d: %s\n", i+1, len(scripts), script)
		if _, err := m.execGuest(ctx, vmName, []string{"sudo", "bash", guestPath}, provider.ExecOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) customInstallScriptHostPath(script string) (string, error) {
	script = strings.TrimSpace(script)
	if script == "" {
		return "", fmt.Errorf("custom install script path is empty")
	}
	if filepath.IsAbs(script) {
		return filepath.Clean(script), nil
	}
	root, err := filepath.Abs(m.ProjectRoot)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(root, script))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("relative custom install script %q escapes project root", script)
	}
	return path, nil
}

func guestScriptName(name string) string {
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "install.sh"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "install.sh"
	}
	return b.String()
}

func (m *Manager) enableWSLSystemd(ctx context.Context, name string) error {
	content := "[boot]\nsystemd=true\n\n[interop]\nappendWindowsPath=false\n\n[user]\ndefault=root\n"
	_, err := m.Provider.Exec(ctx, name, provider.ShellCommand("mkdir -p /etc && cat >/etc/wsl.conf"), provider.ExecOptions{Stdin: content})
	return err
}

func (m *Manager) waitForSystemd(ctx context.Context, name string) error {
	waitSeconds := m.Config.Timeouts.BootSeconds
	if waitSeconds <= 0 {
		waitSeconds = 180
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	var lastErr error
	for {
		result, err := m.Provider.Exec(ctx, name, provider.ShellCommand(`test "$(ps -p 1 -o comm=)" = systemd && state="$(systemctl is-system-running 2>/dev/null || true)" && case "$state" in running|degraded) echo "$state"; exit 0 ;; *) echo "$state"; exit 1 ;; esac`), provider.ExecOptions{
			LogPath: filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.Pool.LogDir), name+".guest.log"),
		})
		if err == nil {
			fmt.Printf("systemd is %s\n", strings.TrimSpace(result.Stdout))
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd did not become ready in WSL distro %s within %d seconds: %w", name, waitSeconds, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func mkdirGuestCommand(path string) string {
	return "if command -v sudo >/dev/null 2>&1; then sudo mkdir -p " + shellQuote(path) + "; else mkdir -p " + shellQuote(path) + "; fi"
}

func imageLogStem(image string) string {
	stem := strings.ReplaceAll(filepath.ToSlash(image), "/", "-")
	stem = strings.ReplaceAll(stem, ":", "")
	if stem == "" {
		return "image"
	}
	return stem
}

func guestText(content []byte) string {
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}

func runHost(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runHostOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func runHostQuiet(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Run()
}

func runHostLogged(ctx context.Context, logPath, name string, args ...string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if logPath != "" {
		cmd.Stdout = io.MultiWriter(os.Stdout, logFile)
		cmd.Stderr = io.MultiWriter(os.Stderr, logFile)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func copyRunnerImagesSubsetToDir(upstreamDir, dest string, buildScripts []string) error {
	roots := []struct {
		src string
		dst string
	}{
		{
			src: filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "helpers"),
			dst: filepath.Join(dest, "images", "ubuntu", "scripts", "helpers"),
		},
		{
			src: filepath.Join(upstreamDir, "images", "ubuntu", "toolsets"),
			dst: filepath.Join(dest, "images", "ubuntu", "toolsets"),
		},
	}
	for _, root := range roots {
		if err := copyDir(root.src, root.dst); err != nil {
			return err
		}
	}
	buildDst := filepath.Join(dest, "images", "ubuntu", "scripts", "build")
	if err := os.MkdirAll(buildDst, 0755); err != nil {
		return err
	}
	for _, name := range buildScripts {
		if err := copyFile(filepath.Join(upstreamDir, "images", "ubuntu", "scripts", "build", name), filepath.Join(buildDst, name), 0644); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) runnerImagesCommit() (string, error) {
	lockPath := config.ProjectPath(m.ProjectRoot, m.Config.Image.UpstreamLock)
	content, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}

func (m *Manager) writeRunnerImagesCommitFile(dest string) error {
	commit, err := m.runnerImagesCommit()
	if err != nil {
		return err
	}
	if commit == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dest, "epar-commit"), []byte(commit+"\n"), 0644)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.WriteFile(dst, content, mode)
}

func resetLogs(paths ...string) error {
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
