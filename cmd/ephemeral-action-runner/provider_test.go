package main

import (
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
)

func TestDockerSandboxesSourceMaintenanceCommandsAreRejectedClearly(t *testing.T) {
	manager := &pool.Manager{Config: config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes"}}}
	err := rejectDockerSandboxesImageCommand(manager, "image update-upstream")
	if err == nil || !strings.Contains(err.Error(), "not applicable") || !strings.Contains(err.Error(), "image build") {
		t.Fatalf("image command rejection = %v", err)
	}
}
