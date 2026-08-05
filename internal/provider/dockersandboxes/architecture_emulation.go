package dockersandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const architectureEmulationHelper = "/opt/epar/enable-architecture-emulation"

// architectureEmulationEnabler is deliberately target-agnostic. The current
// implementation enables every handler bundled by the locked QEMU artifact;
// a future implementation can compose an authoritative accelerator with QEMU
// without adding provider configuration or changing the sandbox lifecycle.
type architectureEmulationEnabler interface {
	Enable(context.Context, *Provider, provider.Instance) (architectureEmulationResult, error)
}

type architectureEmulationResult struct {
	Backend      string `json:"backend"`
	HandlerCount int    `json:"handlerCount"`
}

type qemuBinfmtEnabler struct{}

func (qemuBinfmtEnabler) Enable(ctx context.Context, sandboxProvider *Provider, instance provider.Instance) (architectureEmulationResult, error) {
	result, err := sandboxProvider.run(ctx, commandRequest{
		args:        []string{"exec", instance.Name, "--", "sudo", "-n", architectureEmulationHelper},
		operation:   "enable Docker Sandboxes architecture emulation",
		outputLimit: diagnosticOutputLimit,
	})
	if err != nil {
		return architectureEmulationResult{}, err
	}
	var evidence architectureEmulationResult
	if err := decodeStrictJSON([]byte(strings.TrimSpace(result.Stdout)), &evidence); err != nil || evidence.Backend != "qemu" || evidence.HandlerCount < 1 {
		return architectureEmulationResult{}, fmt.Errorf("Docker Sandboxes architecture emulation helper returned unsupported evidence")
	}
	return evidence, nil
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
