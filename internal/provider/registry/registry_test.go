package registry

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/storagepath"
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

func TestEveryProviderStorageMapsRolesToConcreteRoots(t *testing.T) {
	root := t.TempDir()
	previousDocker := discoverCurrentDockerStorage
	previousEnvironment := currentStorageEnvironment
	t.Cleanup(func() {
		discoverCurrentDockerStorage = previousDocker
		currentStorageEnvironment = previousEnvironment
	})
	discoverCurrentDockerStorage = func(context.Context) (storagepath.DockerStorage, error) {
		return storagepath.DockerStorage{Roots: []storagepath.Resolution{
			{ID: "engine", Path: filepath.Join(root, "docker"), CapacityPath: root, Provenance: storagepath.ProvenanceDockerInfo, Confidence: storagepath.ConfidenceObserved},
			{ID: "containerd", Path: filepath.Join(root, "containerd"), CapacityPath: root, Provenance: storagepath.ProvenanceContainerdConfig, Confidence: storagepath.ConfidenceDerived},
		}}, nil
	}
	currentStorageEnvironment = func() (storagepath.Environment, error) {
		return storagepath.Environment{
			GOOS:          runtime.GOOS,
			HomeDir:       root,
			LocalAppData:  root,
			AppData:       root,
			XDGStateHome:  filepath.Join(root, "state"),
			XDGCacheHome:  filepath.Join(root, "cache"),
			XDGConfigHome: filepath.Join(root, "config"),
			TartHome:      filepath.Join(root, "tart"),
		}, nil
	}

	tests := []struct {
		providerType string
		wantRoles    []storage.StorageRole
	}{
		{providerType: "docker-container", wantRoles: []storage.StorageRole{storage.StorageRoleProject, storage.StorageRoleDockerEngine, storage.StorageRoleContainerdStore}},
		{providerType: "docker-sandboxes", wantRoles: []storage.StorageRole{storage.StorageRoleProject, storage.StorageRoleDockerEngine, storage.StorageRoleContainerdStore, storage.StorageRoleSandboxRuntime, storage.StorageRoleSandboxTemplateCache}},
		{providerType: "wsl", wantRoles: []storage.StorageRole{storage.StorageRoleProject, storage.StorageRoleDockerEngine, storage.StorageRoleContainerdStore, storage.StorageRoleWSLDistribution}},
		{providerType: "tart", wantRoles: []storage.StorageRole{storage.StorageRoleProject, storage.StorageRoleTartStore}},
	}
	for _, test := range tests {
		t.Run(test.providerType, func(t *testing.T) {
			cfg := config.Default()
			cfg.Provider.Type = test.providerType
			snapshot, err := providerStorage(cfg, root).StorageSnapshot(context.Background(), provider.StorageRequest{Now: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			byRole := make(map[storage.StorageRole]int)
			for _, surface := range snapshot.Surfaces {
				if surface.Role != "" {
					byRole[surface.Role]++
				}
				if surface.DomainID == "" {
					t.Errorf("surface %s has no capacity domain", surface.ID)
				}
			}
			for _, role := range test.wantRoles {
				if byRole[role] != 1 {
					t.Errorf("role %q maps to %d surfaces, want exactly 1; surfaces=%#v", role, byRole[role], snapshot.Surfaces)
				}
			}
			if test.providerType == "docker-sandboxes" {
				for _, surface := range snapshot.Surfaces {
					if surface.ID == "docker-sandboxes-staging" && surface.Role != "" {
						t.Errorf("staging surface has allocation role %q", surface.Role)
					}
				}
			}
		})
	}
}

func TestDockerStorageMapsInactiveContainerdRoleToEngineDomain(t *testing.T) {
	root := t.TempDir()
	previous := discoverCurrentDockerStorage
	t.Cleanup(func() { discoverCurrentDockerStorage = previous })
	discoverCurrentDockerStorage = func(context.Context) (storagepath.DockerStorage, error) {
		return storagepath.DockerStorage{Roots: []storagepath.Resolution{{
			ID: "engine", Path: filepath.Join(root, "docker"), CapacityPath: root, Provenance: storagepath.ProvenanceDockerInfo, Confidence: storagepath.ConfidenceObserved,
		}}}, nil
	}
	roots, err := dockerStorageRoots(context.Background(), provider.StorageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("root count = %d, want engine and image-store alias", len(roots))
	}
	byRole := make(map[storage.StorageRole]provider.StorageRoot)
	for _, root := range roots {
		byRole[root.Role] = root
	}
	engine := byRole[storage.StorageRoleDockerEngine]
	imageStore := byRole[storage.StorageRoleContainerdStore]
	if engine.CapacityPath != imageStore.CapacityPath || engine.Path != imageStore.Path {
		t.Fatalf("inactive containerd alias paths differ: engine=%#v image-store=%#v", engine, imageStore)
	}
}

func TestDockerStoragePreservesUnavailableReasonAndAlias(t *testing.T) {
	previous := discoverCurrentDockerStorage
	t.Cleanup(func() { discoverCurrentDockerStorage = previous })
	discoverCurrentDockerStorage = func(context.Context) (storagepath.DockerStorage, error) {
		return storagepath.DockerStorage{Roots: []storagepath.Resolution{{
			ID: "engine", Path: "/var/lib/docker", CapacityUnavailableReason: "host cannot stat guest path", Provenance: storagepath.ProvenanceDockerInfo, Confidence: storagepath.ConfidenceUnavailable,
		}}}, nil
	}
	roots, err := dockerStorageRoots(context.Background(), provider.StorageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots = %#v, want engine and containerd alias", roots)
	}
	for _, root := range roots {
		if root.CapacityUnavailableReason != "host cannot stat guest path" || root.Confidence != string(storagepath.ConfidenceUnavailable) {
			t.Fatalf("unavailable root = %#v", root)
		}
	}
}

func TestDockerStorageDiscoveryFailurePublishesUnknownRoots(t *testing.T) {
	previous := discoverCurrentDockerStorage
	t.Cleanup(func() { discoverCurrentDockerStorage = previous })
	discoverCurrentDockerStorage = func(context.Context) (storagepath.DockerStorage, error) {
		return storagepath.DockerStorage{}, fmt.Errorf("%w: docker info temporarily unavailable", storagepath.ErrDockerCapacityUnavailable)
	}
	roots, err := dockerStorageRoots(context.Background(), provider.StorageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots = %#v, want two unknown role roots", roots)
	}
	for _, root := range roots {
		if root.CapacityUnavailableReason == "" || root.Confidence != string(storagepath.ConfidenceUnavailable) {
			t.Fatalf("fallback root = %#v", root)
		}
	}
}

func TestDockerStorageRejectsUnsupportedEndpointAndMalformedObservation(t *testing.T) {
	previous := discoverCurrentDockerStorage
	t.Cleanup(func() { discoverCurrentDockerStorage = previous })
	for name, discoveryErr := range map[string]error{
		"remote":       storagepath.ErrRemoteDockerEndpoint,
		"unsupported":  storagepath.ErrUnsupportedDockerTransport,
		"invalid":      storagepath.ErrInvalidDockerStorage,
		"unclassified": errors.New("unclassified Docker discovery failure"),
	} {
		t.Run(name, func(t *testing.T) {
			discoverCurrentDockerStorage = func(context.Context) (storagepath.DockerStorage, error) {
				return storagepath.DockerStorage{}, discoveryErr
			}
			if _, err := dockerStorageRoots(context.Background(), provider.StorageRequest{}); err == nil || !errors.Is(err, discoveryErr) {
				t.Fatalf("dockerStorageRoots() error = %v, want %v", err, discoveryErr)
			}
		})
	}
}

func TestDockerSandboxesOperationRolesExcludeStagingAndRouteMajorGrowth(t *testing.T) {
	root := t.TempDir()
	previousDocker := discoverCurrentDockerStorage
	previousEnvironment := currentStorageEnvironment
	t.Cleanup(func() {
		discoverCurrentDockerStorage = previousDocker
		currentStorageEnvironment = previousEnvironment
	})
	discoverCurrentDockerStorage = func(context.Context) (storagepath.DockerStorage, error) {
		return storagepath.DockerStorage{Roots: []storagepath.Resolution{{ID: "engine", Path: filepath.Join(root, "docker"), CapacityPath: root}}}, nil
	}
	currentStorageEnvironment = func() (storagepath.Environment, error) {
		return storagepath.Environment{GOOS: runtime.GOOS, HomeDir: root, LocalAppData: root, AppData: root, XDGStateHome: filepath.Join(root, "state"), XDGCacheHome: filepath.Join(root, "cache"), XDGConfigHome: filepath.Join(root, "config")}, nil
	}
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	snapshot, err := providerStorage(cfg, root).StorageSnapshot(context.Background(), provider.StorageRequest{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		operation string
		role      storage.StorageRole
		want      string
	}{
		{operation: "image-build", role: storage.StorageRoleContainerdStore, want: "containerd-store-backing"},
		{operation: "template-build", role: storage.StorageRoleSandboxTemplateCache, want: "docker-sandboxes-template-cache"},
		{operation: "instance-create", role: storage.StorageRoleSandboxRuntime, want: "docker-sandboxes-runtime"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			plan := storage.OperationPlan{ID: test.operation, Phases: []storage.OperationPhase{{ID: "growth", Allocations: []storage.Allocation{{ID: "major-growth", Role: test.role, Bytes: 1}}}}}
			resolved, err := storage.ResolveOperationPlan(plan, snapshot.Surfaces, snapshot.Domains)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.Allocations[0].SurfaceID; got != test.want {
				t.Fatalf("resolved surface = %q, want %q", got, test.want)
			}
			if resolved.Allocations[0].SurfaceID == "docker-sandboxes-staging" {
				t.Fatal("major growth resolved to staging")
			}
		})
	}
}

func TestLinuxDockerSandboxesConfigRootIsReportOnly(t *testing.T) {
	previousEnvironment := currentStorageEnvironment
	t.Cleanup(func() { currentStorageEnvironment = previousEnvironment })
	currentStorageEnvironment = func() (storagepath.Environment, error) {
		return storagepath.Environment{GOOS: "linux", HomeDir: "/home/runner", XDGStateHome: "/mnt/state", XDGCacheHome: "/mnt/cache", XDGConfigHome: "/mnt/config"}, nil
	}
	roots, err := dockerSandboxesStorageRoots(context.Background(), provider.StorageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		if root.ID == "docker-sandboxes-config" {
			if !root.ReportOnly {
				t.Fatal("Linux Docker Sandboxes config root is not marked report-only")
			}
			return
		}
	}
	t.Fatal("Linux Docker Sandboxes config surface was not reported")
}

func TestStorageDiscoveryIsScopedToAllocatedRoles(t *testing.T) {
	request := provider.StorageRequest{OperationPlan: storage.OperationPlan{ID: "template-import", Phases: []storage.OperationPhase{{ID: "import", Allocations: []storage.Allocation{{ID: "cache", Role: storage.StorageRoleSandboxTemplateCache, Bytes: storage.GiB}}}}}}
	if storageRequestUsesRole(request, storage.StorageRoleDockerEngine, storage.StorageRoleContainerdStore) {
		t.Fatal("Sandbox cache-only operation requested Docker Engine discovery")
	}
	if !storageRequestUsesRole(request, storage.StorageRoleSandboxRuntime, storage.StorageRoleSandboxTemplateCache) {
		t.Fatal("Sandbox cache-only operation skipped Sandbox storage discovery")
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
