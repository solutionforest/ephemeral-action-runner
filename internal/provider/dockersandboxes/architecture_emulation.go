package dockersandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const architectureEmulationHelper = "/opt/epar/enable-architecture-emulation"
const nativeArchitectureHelper = "/opt/epar/verify-native-architecture"
const architectureWarningLimit = 4 << 10

const (
	architectureEmulationBestEffort = "best-effort"
	architectureEmulationRequired   = "required"
	architectureEmulationNativeOnly = "native-only"
)

// architectureEmulationEnabler is deliberately target-agnostic. The current
// implementation can require QEMU, attempt it before a verified native
// fallback, or verify an explicitly configured native-only capability.
type architectureEmulationEnabler interface {
	Enable(context.Context, *Provider, provider.Instance) (architectureEmulationResult, error)
}

type architectureEmulationResult struct {
	Mode         string `json:"mode,omitempty"`
	Backend      string `json:"backend"`
	HandlerCount int    `json:"handlerCount"`
	Platform     string `json:"platform,omitempty"`
	Warning      string `json:"-"`
}

type qemuBinfmtEnabler struct{}

func (qemuBinfmtEnabler) Enable(ctx context.Context, sandboxProvider *Provider, instance provider.Instance) (architectureEmulationResult, error) {
	result, err := sandboxProvider.run(ctx, commandRequest{
		args:        []string{"exec", instance.Name, "--", "sudo", "-n", architectureEmulationHelper},
		operation:   "enable Docker Sandboxes architecture emulation",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return architectureEmulationResult{}, err
	}
	var evidence architectureEmulationResult
	if err := decodeStrictJSON([]byte(strings.TrimSpace(result.Stdout)), &evidence); err != nil || evidence.Backend != "qemu" || evidence.HandlerCount < 1 {
		return architectureEmulationResult{}, fmt.Errorf("Docker Sandboxes architecture emulation helper returned unsupported evidence")
	}
	evidence.Mode = architectureEmulationRequired
	return evidence, nil
}

type nativeArchitectureEnabler struct {
	platform      string
	allowHandlers bool
}

func (enabler nativeArchitectureEnabler) Enable(ctx context.Context, sandboxProvider *Provider, instance provider.Instance) (architectureEmulationResult, error) {
	result, err := sandboxProvider.run(ctx, commandRequest{
		args:        []string{"exec", instance.Name, "--", "sudo", "-n", nativeArchitectureHelper, enabler.platform},
		operation:   "verify Docker Sandboxes native architecture",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return architectureEmulationResult{}, err
	}
	var evidence architectureEmulationResult
	if err := decodeStrictJSON([]byte(strings.TrimSpace(result.Stdout)), &evidence); err != nil || evidence.Backend != "native" || evidence.HandlerCount < 0 || (!enabler.allowHandlers && evidence.HandlerCount != 0) || evidence.Platform != enabler.platform {
		return architectureEmulationResult{}, fmt.Errorf("Docker Sandboxes native architecture helper returned unsupported evidence")
	}
	evidence.Mode = architectureEmulationNativeOnly
	return evidence, nil
}

type bestEffortArchitectureEnabler struct {
	platform string
}

func (enabler bestEffortArchitectureEnabler) Enable(ctx context.Context, sandboxProvider *Provider, instance provider.Instance) (architectureEmulationResult, error) {
	qemuCtx, cancelQEMU := architectureAttemptContext(ctx, 2)
	evidence, qemuErr := (qemuBinfmtEnabler{}).Enable(qemuCtx, sandboxProvider, instance)
	cancelQEMU()
	if qemuErr == nil {
		evidence.Mode = architectureEmulationBestEffort
		return evidence, nil
	}
	nativeCtx, cancelNative := architectureAttemptContext(ctx, 1)
	evidence, nativeErr := (nativeArchitectureEnabler{platform: enabler.platform, allowHandlers: true}).Enable(nativeCtx, sandboxProvider, instance)
	cancelNative()
	if nativeErr != nil {
		return architectureEmulationResult{}, fmt.Errorf("Docker Sandboxes QEMU/binfmt activation failed (%w), and native architecture verification also failed: %w", qemuErr, nativeErr)
	}
	evidence.Mode = architectureEmulationBestEffort
	evidence.Warning = truncate(qemuErr.Error(), architectureWarningLimit)
	return evidence, nil
}

// architectureAttemptContext gives each best-effort capability attempt its
// own budget while respecting a caller-wide deadline. Without this split, a
// slow QEMU activation consumes the complete liveness deadline and the native
// fallback receives an already-expired context, producing a misleading
// "QEMU and native both timed out" result.
func architectureAttemptContext(parent context.Context, attemptsRemaining int) (context.Context, context.CancelFunc) {
	timeout := providerReadbackTimeout
	if attemptsRemaining > 0 {
		if deadline, ok := parent.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining < timeout {
				timeout = remaining / time.Duration(attemptsRemaining)
			}
		}
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	return context.WithTimeout(parent, timeout)
}

// parseExperimentalInstanceReceipt accepts only enough of the unreleased v2
// receipt to preserve exact cleanup of sandboxes created from development
// builds. Architecture evidence is intentionally ignored.
func parseExperimentalInstanceReceipt(data json.RawMessage) (instanceReceipt, error) {
	var receipt instanceReceipt
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.SchemaVersion != 2 || receipt.StagingPath == "" || receipt.StagingIdentity == "" || receipt.Template == "" || !validFullTemplateDigest(receipt.TemplateDigest) {
		return instanceReceipt{}, fmt.Errorf("Docker Sandboxes experimental instance receipt is malformed")
	}
	return receipt, nil
}
