package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	imageartifact "github.com/solutionforest/ephemeral-action-runner/internal/image"
	sandboxpromotion "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/promotion"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/tart"
)

var initResolveTartSource = imageartifact.ResolveTartSource

var initTartPrerequisites = func(ctx context.Context, platform sandboxpromotion.Platform) (bool, string) {
	if string(platform) != config.TartMacOSPlatform {
		return false, "UNAVAILABLE — Apple Silicon macOS and Tart are required"
	}
	binary, err := exec.LookPath("tart")
	if err != nil {
		return false, "UNAVAILABLE — install Tart on this Apple Silicon Mac"
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	version, err := exec.CommandContext(checkCtx, binary, "--version").Output()
	if err != nil {
		return false, fmt.Sprintf("UNAVAILABLE — tart --version failed: %v", err)
	}
	if err := tart.ValidateMacOSVersion(string(version)); err != nil {
		return false, "UNAVAILABLE — " + err.Error()
	}
	return true, "READY — Apple Silicon macOS, Tart " + strings.TrimSpace(string(version))
}

func promptTartImageWizard(ctx context.Context, projectRoot string, existing *initDockerSandboxesProfile, out io.Writer, reader *bufio.Reader) (initWizardResult[*initDockerSandboxesProfile], *initArtifactEstimate, error) {
	input := config.DefaultTartMacOSImage
	if existing != nil && existing.Provider == "tart" {
		input = existing.SourceImage
	}
	fmt.Fprintln(out, "\nTart macOS native image setup (preview):")
	fmt.Fprintln(out, "  "+tart.MacOSDockerWarning)
	fmt.Fprintln(out, "  Choose a macOS ARM64 Tart OCI image with the Tart Guest Agent; Xcode images include SDKs.")
	fmt.Fprintln(out, "  The first image download can be large; later runners clone the prepared local image.")
	fmt.Fprintln(out, "  Custom install scripts start empty. See configs/tart.macos-mobile.example.yml for Android tool paths.")
	fmt.Fprintln(out, "  0. Back")
	for {
		value, hitEOF, err := promptDefault(out, reader, "Tart OCI image", input)
		if err != nil {
			return initWizardResult[*initDockerSandboxesProfile]{}, nil, err
		}
		if value == "0" || strings.EqualFold(value, "back") || value == "/back" {
			return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardBack}, nil, nil
		}
		source, err := initResolveTartSource(ctx, value, config.TartMacOSPlatform)
		if err != nil {
			fmt.Fprintf(out, "Cannot use Tart image: %v\n", err)
			if hitEOF {
				return initWizardResult[*initDockerSandboxesProfile]{}, nil, err
			}
			continue
		}
		plan, err := imageartifact.PlanArtifactStorage("tart", imageartifact.SourceSizeEstimate{CompressedBytes: source.CompressedBytes, ExpandedBytes: source.ExpandedBytes, Confidence: imageartifact.EstimateExact}, false, 0)
		if err != nil {
			return initWizardResult[*initDockerSandboxesProfile]{}, nil, err
		}
		estimate := &initArtifactEstimate{Source: source.Reference, Platform: source.Platform, DownloadBytes: source.CompressedBytes, ExpandedBytes: source.ExpandedBytes}
		populateInitArtifactCapacityEstimate(ctx, projectRoot, "tart", plan.OperationPlan, estimate)
		return initWizardResult[*initDockerSandboxesProfile]{Action: initWizardNext, Value: &initDockerSandboxesProfile{Provider: "tart", GuestPlatform: config.TartMacOSPlatform, SourceImage: source.Reference}}, estimate, nil
	}
}

func defaultTartMacOSConfig(draft initWizardDraft) string {
	policy := draft.RunnerGroup.Policy
	return fmt.Sprintf(`# Tart macOS preview. %s
github:
  appId: %d
  organization: %s
  privateKeyPath: %s
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com

image:
  sourceType: tart-image
  sourceImage: %s
  outputImage: epar-tart-macos-mobile
  runnerVersion: latest
  updateFrequency: %s
  updateTime: %s
  hostTrustMode: disabled
  customInstallScripts:

pool:
  instances: 1
  namePrefix: %s

runner:
  group: %s
  labels: [macOS, ARM64, epar-tart-macos-mobile]
  noDefaultLabels: true
  includeHostLabel: true
  ephemeral: true

security:
  runnerGroup:
    enforcement: %s
    requireExplicitGroup: %t
    requireNonDefaultGroup: %t
    requiredRepositoryAccess: %s
    requirePublicRepositoriesDisabled: %t

provider:
  type: tart
  platform: darwin/arm64
  sourceImage: epar-tart-macos-mobile
  network: default

timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
`, tart.MacOSDockerWarning, draft.AppID, strconv.Quote(draft.Organization), strconv.Quote(draft.PrivateKeyPath), strconv.Quote(draft.Profile.SourceImage), draft.UpdatePolicy.Frequency, strconv.Quote(draft.UpdatePolicy.Time), strconv.Quote(draft.PoolNamePrefix), strconv.Quote(draft.RunnerGroup.Group.Name), policy.Enforcement, policy.RequireExplicitGroup, policy.RequireNonDefaultGroup, policy.RequiredRepositoryAccess, policy.RequirePublicRepositoriesDisabled)
}
