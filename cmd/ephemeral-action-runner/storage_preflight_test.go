package main

import (
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestPreflightControllerStorageRejectsInsufficientSurface(t *testing.T) {
	t.Setenv("EPAR_INVOCATION", "start")
	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	cfg.Storage.MinimumFree = "9223372036854775807B"

	err := preflightControllerStorage(t.TempDir(), cfg)
	if err == nil {
		t.Fatal("preflightControllerStorage() error = nil, want insufficient capacity")
	}
	for _, want := range []string{
		"not enough disk space to initialize the EPAR controller",
		"Estimated operation growth: 0 bytes",
		"Free-space reserve: 8.00 EiB",
		"./start storage prune --provider docker-container",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("preflightControllerStorage() error = %q, want %q", err, want)
		}
	}
}
