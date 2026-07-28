package main

import (
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
)

func TestDockerSandboxesImageCommandsAreRejectedClearly(t *testing.T) {
	manager := &pool.Manager{Config: config.Config{Provider: config.ProviderConfig{Type: "docker-sandboxes"}}}
	err := rejectDockerSandboxesImageCommand(manager, "image build")
	if err == nil || !strings.Contains(err.Error(), "not supported") || !strings.Contains(err.Error(), "scripts/docker-sandboxes") {
		t.Fatalf("image command rejection = %v", err)
	}
}
