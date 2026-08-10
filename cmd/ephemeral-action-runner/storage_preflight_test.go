package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestPreflightControllerStorageRejectsInsufficientSurface(t *testing.T) {
	t.Setenv("EPAR_INVOCATION", "start")
	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	cfg.Storage.MinimumFree = "9223372036854775807B"

	projectRoot := t.TempDir()
	err := preflightControllerStorage("config.sbx.yml", projectRoot, cfg)
	if err == nil {
		t.Fatal("preflightControllerStorage() error = nil, want insufficient capacity")
	}
	for _, want := range []string{
		"not enough disk space to initialize the EPAR controller",
		"Estimated operation growth: 0 bytes",
		"Free-space reserve: 8.00 EiB",
		"./start storage prune --provider docker-container --config config.sbx.yml --project-root " + projectRoot,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("preflightControllerStorage() error = %q, want %q", err, want)
		}
	}
}

type controllerStorageSnapshot struct {
	snapshot provider.StorageSnapshot
}

func (fixed controllerStorageSnapshot) StorageSnapshot(context.Context, provider.StorageRequest) (provider.StorageSnapshot, error) {
	return fixed.snapshot, nil
}

func TestPreflightControllerStorageContinuesUnknownCapacityWithWarning(t *testing.T) {
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	projectRoot := t.TempDir()
	contribution := controllerStorageSnapshot{snapshot: provider.StorageSnapshot{
		Surfaces: []storage.Surface{{ID: "project", Provider: cfg.Provider.Type, Role: storage.StorageRoleProject, Kind: storage.SurfaceHostFilesystem, DomainID: "project-unknown", Path: projectRoot, Capacity: storage.Capacity{}}},
		Domains:  []storage.CapacityDomain{{ID: "project-unknown", Kind: storage.SurfaceHostFilesystem, Path: projectRoot, CapacityUnavailableReason: "statfs permission denied", Capacity: storage.Capacity{}}},
	}}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStderr := os.Stderr
	os.Stderr = write
	err = preflightControllerStorage("config.sbx.yml", projectRoot, cfg, contribution)
	os.Stderr = previousStderr
	if closeErr := write.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	output, readErr := io.ReadAll(read)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr := read.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatalf("unknown controller capacity rejected: %v", err)
	}
	warning := string(output)
	for _, want := range []string{"STORAGE CAPACITY UNKNOWN", "operation=controller-bootstrap", "domain=project-unknown", "roles=project", "estimatedBytes=0", "requiredBytes=", "statfs permission denied", "storage status --operation controller-bootstrap"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning = %q, want %q", warning, want)
		}
	}
}
