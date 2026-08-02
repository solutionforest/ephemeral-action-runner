package registry

import (
	"context"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockercontainer"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/tart"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/wsl"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestNewAdaptsEstablishedLegacyProviders(t *testing.T) {
	tests := []struct {
		providerType string
		matches      func(any) bool
	}{
		{providerType: "tart", matches: func(value any) bool { _, ok := value.(*tart.Provider); return ok }},
		{providerType: "wsl", matches: func(value any) bool { _, ok := value.(*wsl.Provider); return ok }},
	}
	for _, test := range tests {
		t.Run(test.providerType, func(t *testing.T) {
			cfg := config.Default()
			cfg.Provider.Type = test.providerType

			providerRuntime, err := New(cfg, t.TempDir(), true)
			if err != nil {
				t.Fatal(err)
			}
			if !test.matches(providerRuntime.Legacy) {
				t.Fatalf("New() legacy type = %T", providerRuntime.Legacy)
			}
			if providerRuntime.Lifecycle == nil {
				t.Fatal("New() did not adapt the legacy provider to Lifecycle")
			}
			if providerRuntime.Storage == nil {
				t.Fatal("New() did not register provider storage behavior")
			}
			if providerRuntime.PolicyManager != nil {
				t.Fatalf("New() policy manager = %T, want nil", providerRuntime.PolicyManager)
			}
		})
	}
}

func TestNewWiresDockerContainerOptions(t *testing.T) {
	cfg := config.Default()
	cfg.Provider.Type = "docker-container"
	cfg.Provider.Platform = "linux/amd64"
	cfg.Docker.HTTPProxy = "http://host.docker.internal:3128"
	cfg.Docker.HTTPSProxy = "http://host.docker.internal:3128"
	cfg.Docker.NoProxy = "localhost,127.0.0.1"

	runtime, err := New(cfg, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	dockerContainer, ok := runtime.Legacy.(*dockercontainer.Provider)
	if !ok {
		t.Fatalf("New() legacy type = %T, want Docker Container provider", runtime.Legacy)
	}
	if runtime.Lifecycle == nil {
		t.Fatal("New() did not adapt the legacy provider to Lifecycle")
	}
	if runtime.Storage == nil {
		t.Fatal("New() storage contribution is nil")
	}
	if !dockerContainer.HostGateway {
		t.Fatal("host.docker.internal proxy did not enable host gateway")
	}
	for key, want := range map[string]string{
		"HTTP_PROXY":  cfg.Docker.HTTPProxy,
		"HTTPS_PROXY": cfg.Docker.HTTPSProxy,
		"NO_PROXY":    cfg.Docker.NoProxy,
	} {
		if got := dockerContainer.Environment[key]; got != want {
			t.Errorf("provider environment %s = %q, want %q", key, got, want)
		}
	}
}

func TestNewWiresDockerSandboxesCapabilitiesWithoutLegacyAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"

	runtime, err := New(cfg, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Legacy != nil {
		t.Fatalf("New() legacy = %T, want nil", runtime.Legacy)
	}
	if runtime.Lifecycle == nil {
		t.Fatal("New() lifecycle is nil")
	}
	if runtime.PolicyManager == nil {
		t.Fatal("New() policy manager is nil")
	}
	if runtime.Storage == nil {
		t.Fatal("New() storage contribution is nil")
	}
}

func TestDockerSandboxesStorageRoutesOperationsToTheirBackingSurfaces(t *testing.T) {
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	cfg.DockerSandboxes.RootDisk = "30GiB"
	cfg.DockerSandboxes.DockerDisk = "100GiB"
	contribution := providerStorage(cfg, t.TempDir())

	create, err := contribution.StorageSnapshot(context.Background(), provider.StorageRequest{
		Operation:        "instance-create",
		Now:              time.Now(),
		PeakBytes:        10 << 30,
		MinimumFreeBytes: 50 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	createRequirements := map[string]uint64{}
	for _, requirement := range create.Requirements {
		createRequirements[requirement.SurfaceID] = requirement.PeakBytes
	}
	if got, want := createRequirements["docker-sandboxes-backing"], uint64(10<<30); got != want {
		t.Fatalf("sandbox backing create expansion = %d, want %d", got, want)
	}
	if _, found := createRequirements["docker-engine-backing"]; found {
		t.Fatalf("sandbox create incorrectly reserved Docker Engine storage: %v", createRequirements)
	}
	createSurfaces := map[string]storage.SurfaceKind{}
	for _, surface := range create.Surfaces {
		createSurfaces[surface.ID] = surface.Kind
	}
	if got, want := createSurfaces["docker-engine-backing"], storage.SurfaceDockerEngine; got != want {
		t.Fatalf("Docker Engine surface kind = %q, want %q", got, want)
	}
	if got, want := createSurfaces["docker-sandboxes-backing"], storage.SurfaceSandboxCache; got != want {
		t.Fatalf("Docker Sandboxes surface kind = %q, want %q", got, want)
	}

	pull, err := contribution.StorageSnapshot(context.Background(), provider.StorageRequest{
		Operation:        "image-pull",
		Now:              time.Now(),
		PeakBytes:        20 << 30,
		MinimumFreeBytes: 50 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	pullRequirements := map[string]uint64{}
	for _, requirement := range pull.Requirements {
		pullRequirements[requirement.SurfaceID] = requirement.PeakBytes
	}
	if _, found := pullRequirements["docker-sandboxes-backing"]; found {
		t.Fatalf("Docker image pull incorrectly reserved sandbox instance storage: %v", pullRequirements)
	}
	if got, want := pullRequirements["docker-engine-backing"], uint64(20<<30); got != want {
		t.Fatalf("Docker Engine pull expansion = %d, want %d", got, want)
	}
}

func TestNewRejectsUnsupportedProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Provider.Type = "unknown-provider"

	_, err := New(cfg, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), `unsupported provider.type "unknown-provider"`) {
		t.Fatalf("New() error = %v", err)
	}
}

func TestPoolImportsOnlyNeutralProviderContracts(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate registry test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	poolFiles, err := filepath.Glob(filepath.Join(repositoryRoot, "internal", "pool", "*.go"))
	if err != nil {
		t.Fatalf("list pool Go files: %v", err)
	}
	if len(poolFiles) == 0 {
		t.Fatal("no pool Go files found")
	}

	concreteProviders := []string{
		"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockercontainer",
		"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes",
		"github.com/solutionforest/ephemeral-action-runner/internal/provider/tart",
		"github.com/solutionforest/ephemeral-action-runner/internal/provider/wsl",
	}
	for _, path := range poolFiles {
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse imports in %s: %v", path, parseErr)
		}
		for _, imported := range parsed.Imports {
			importPath, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				t.Fatalf("unquote import %s in %s: %v", imported.Path.Value, path, unquoteErr)
			}
			for _, concrete := range concreteProviders {
				if importPath == concrete || strings.HasPrefix(importPath, concrete+"/") {
					t.Errorf("%s imports concrete provider %q; pool must use internal/provider contracts", path, importPath)
				}
			}
		}
	}
}
