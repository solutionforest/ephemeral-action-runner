package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes"
	tartprovider "github.com/solutionforest/ephemeral-action-runner/internal/provider/tart"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage/inventory"
)

const legacyOwnershipEvidence = "operator-selected legacy preview; exact identity readback is still required at execution"

func addCatalogStorage(snapshot *inventory.Snapshot, providerFilter string, now time.Time) (storagecatalog.Catalog, error) {
	store, err := storagecatalog.Open("")
	if err != nil {
		return storagecatalog.Catalog{}, err
	}
	value, err := store.Load(now)
	if err != nil {
		return storagecatalog.Catalog{}, err
	}
	surfaces := make(map[string]bool, len(snapshot.Surfaces))
	for _, surface := range snapshot.Surfaces {
		surfaces[surface.ID] = true
	}
	for _, resource := range value.Resources {
		if !storageProviderMatches(providerFilter, resource.Provider) {
			continue
		}
		artifact, ok := catalogStorageArtifact(value, resource, now)
		if !ok {
			continue
		}
		if !surfaces[artifact.SurfaceID] {
			snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
				ID:       artifact.SurfaceID,
				Provider: artifact.Provider,
				Kind:     storage.SurfaceExternal,
				Location: resource.BackendID,
				Capacity: storage.Capacity{ObservedAt: now},
			})
			surfaces[artifact.SurfaceID] = true
		}
		mergeCatalogStorageArtifact(snapshot, artifact)
	}
	return value, nil
}

func mergeCatalogStorageArtifact(snapshot *inventory.Snapshot, catalogArtifact storage.Artifact) {
	artifacts := snapshot.Artifacts[:0]
	for _, existing := range snapshot.Artifacts {
		if !sameCatalogStorageTarget(existing, catalogArtifact) {
			artifacts = append(artifacts, existing)
			continue
		}
		if existing.SizeBytes > catalogArtifact.SizeBytes {
			catalogArtifact.SizeBytes = existing.SizeBytes
		}
		if catalogArtifact.CreatedAt.IsZero() || (!existing.CreatedAt.IsZero() && existing.CreatedAt.Before(catalogArtifact.CreatedAt)) {
			catalogArtifact.CreatedAt = existing.CreatedAt
		}
		if existing.Current {
			catalogArtifact.Current = true
			for _, protection := range existing.Protections {
				if protection.Kind == storage.ProtectionConfiguration {
					catalogArtifact.Protections = append(catalogArtifact.Protections, protection)
				}
			}
		}
	}
	snapshot.Artifacts = append(artifacts, catalogArtifact)
}

func sameCatalogStorageTarget(left, right storage.Artifact) bool {
	if left.Kind != right.Kind || left.Target.Kind != right.Target.Kind || left.Target.Identity == "" || left.Target.Identity != right.Target.Identity {
		return false
	}
	leftLocator := strings.TrimSpace(left.Target.Locator)
	rightLocator := strings.TrimSpace(right.Target.Locator)
	if left.Target.Kind == storage.TargetSandboxTemplate {
		leftLocator = strings.TrimPrefix(strings.ToLower(leftLocator), "docker.io/library/")
		rightLocator = strings.TrimPrefix(strings.ToLower(rightLocator), "docker.io/library/")
		return leftLocator == rightLocator
	}
	if left.Target.Kind == storage.TargetFile || left.Target.Kind == storage.TargetDirectory {
		leftLocator = filepath.Clean(leftLocator)
		rightLocator = filepath.Clean(rightLocator)
		if runtime.GOOS == "windows" {
			return strings.EqualFold(leftLocator, rightLocator)
		}
	}
	return leftLocator == rightLocator
}

func catalogStorageArtifact(value storagecatalog.Catalog, resource storagecatalog.Resource, now time.Time) (storage.Artifact, bool) {
	ownerID := value.InstallationID
	if len(resource.InstallationIDs) != 0 {
		ownerID = strings.Join(resource.InstallationIDs, ",")
	}
	artifact := storage.Artifact{
		ID:             "catalog-" + resource.Key,
		Provider:       resource.Provider,
		SurfaceID:      catalogSurfaceID(resource),
		RetentionGroup: resource.BackendID + "/" + resource.Provider + "/" + resource.Role,
		Ownership: storage.Ownership{
			Kind:     storage.OwnershipExact,
			OwnerID:  ownerID,
			Evidence: "EPAR host resource catalog " + resource.Key,
		},
		CreatedAt:      resource.CreatedAt,
		LastUsedAt:     resource.LastSeenAt,
		SupersededAt:   resource.SupersededAt,
		BackendID:      resource.BackendID,
		Custody:        string(resource.Custody),
		LifecycleState: string(resource.State),
		CleanupError:   resource.CleanupError,
	}
	for _, reference := range resource.References {
		artifact.ConfigRefs = append(artifact.ConfigRefs, reference.ConfigID)
	}
	sort.Strings(artifact.ConfigRefs)
	if resource.LeaseExpiresAt != nil {
		artifact.Lease = &storage.Lease{ID: "catalog-resource", ExpiresAt: *resource.LeaseExpiresAt}
	}
	if len(resource.References) != 0 || resource.State == storagecatalog.StateCurrent {
		artifact.Current = true
		artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionConfiguration, Detail: "referenced by registered EPAR configuration"})
	}
	switch resource.Kind {
	case "docker-image":
		artifact.Kind = storage.ArtifactDockerImage
		artifact.Target = storage.Target{Kind: storage.TargetDockerImageTag, Locator: resource.Locator, Identity: resource.Identity, Fingerprint: resource.Fingerprint, Match: storage.MatchExact}
		if resource.Custody == storagecatalog.CustodyAcquired && len(resource.IntroducedTags) == 0 {
			artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "the Docker tag existed before EPAR acquisition; it remains shared and report-only"})
		}
	case "sandbox-template":
		artifact.Kind = storage.ArtifactSandboxTemplate
		artifact.Target = storage.Target{Kind: storage.TargetSandboxTemplate, Locator: resource.Locator, Identity: resource.Identity, Fingerprint: resource.Fingerprint, Match: storage.MatchExact}
	case "provider-image":
		target, err := storage.SnapshotFilesystemTarget(resource.Locator)
		if err == nil {
			artifact.Kind = storage.ArtifactProviderImage
			artifact.Target = target
			if info, statErr := os.Lstat(target.Locator); statErr == nil {
				artifact.SizeBytes = uint64(maxInt64(info.Size(), 0))
			}
		} else {
			artifact.Kind = storage.ArtifactProviderImage
			artifact.Target = storage.Target{Kind: storage.TargetExternal, Locator: resource.Locator, Identity: resource.Identity, Fingerprint: resource.Fingerprint, Match: storage.MatchExact}
			artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "catalog filesystem target cannot be read exactly"})
		}
	case "template-staging-directory":
		target, err := storage.SnapshotFilesystemTarget(resource.Locator)
		artifact.Kind = storage.ArtifactOther
		if err == nil {
			artifact.Target = target
			if info, statErr := os.Lstat(target.Locator); statErr == nil {
				artifact.SizeBytes = uint64(maxInt64(info.Size(), 0))
			}
		} else {
			artifact.Target = storage.Target{Kind: storage.TargetExternal, Locator: resource.Locator, Identity: resource.Identity, Fingerprint: resource.Fingerprint, Match: storage.MatchExact}
			artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "catalog template staging directory cannot be read exactly"})
		}
	case "tart-image":
		artifact.Kind = storage.ArtifactProviderImage
		artifact.Target = storage.Target{Kind: storage.TargetExternal, Locator: "tart-image:" + resource.Locator, Identity: resource.Identity, Fingerprint: resource.Fingerprint, Match: storage.MatchExact}
	default:
		return storage.Artifact{}, false
	}
	if artifact.SupersededAt == nil && (resource.State == storagecatalog.StateSuperseded || resource.State == storagecatalog.StateCleanupPending) {
		supersededAt := now.UTC()
		artifact.SupersededAt = &supersededAt
	}
	return artifact, true
}

func catalogSurfaceID(resource storagecatalog.Resource) string {
	switch resource.Kind {
	case "docker-image":
		return "docker-engine"
	case "sandbox-template":
		return "docker-sandboxes-template-cache"
	case "template-staging-directory":
		return inventory.ProjectSurfaceID
	case "tart-image":
		return "tart-images"
	default:
		return "catalog-" + resource.Key[:12]
	}
}

func selectLegacyStorage(snapshot *inventory.Snapshot, catalogValue storagecatalog.Catalog, now time.Time) {
	known := make(map[string]bool)
	for _, resource := range catalogValue.Resources {
		known[string(resource.Kind)+"\x00"+resource.Identity] = true
		known[string(resource.Kind)+"\x00"+resource.Locator] = true
	}
	for index := range snapshot.Artifacts {
		artifact := &snapshot.Artifacts[index]
		if artifact.Ownership.Kind != storage.OwnershipUnknown || !legacyEPARArtifact(*artifact) {
			continue
		}
		catalogKind := ""
		switch artifact.Kind {
		case storage.ArtifactDockerImage:
			catalogKind = "docker-image"
		case storage.ArtifactSandboxTemplate:
			catalogKind = "sandbox-template"
		case storage.ArtifactDockerVolume:
			catalogKind = "docker-volume"
		default:
			continue
		}
		if known[catalogKind+"\x00"+artifact.Target.Identity] || known[catalogKind+"\x00"+artifact.Target.Locator] {
			artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionConfiguration, Detail: "exact identity is present in the host resource catalog"})
			continue
		}
		artifact.Ownership = storage.Ownership{Kind: storage.OwnershipExact, OwnerID: "legacy-preview", Evidence: legacyOwnershipEvidence}
		artifact.LifecycleState = string(storagecatalog.StateSuperseded)
		artifact.RetentionGroup = "legacy/" + string(artifact.Kind)
		supersededAt := now.UTC()
		artifact.SupersededAt = &supersededAt
		artifact.Protections = removeUncertainProtections(artifact.Protections)
		switch artifact.Kind {
		case storage.ArtifactDockerImage:
			if blockers, err := dockerImageContainerBlockers(artifact.Target.Identity); err != nil {
				artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "container references could not be checked: " + err.Error()})
			} else if len(blockers) != 0 {
				artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionActive, Detail: "Docker containers reference this image: " + strings.Join(blockers, ",")})
			}
		case storage.ArtifactSandboxTemplate:
			if active, err := activeSandboxCount(); err != nil {
				artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "active Sandboxes could not be checked: " + err.Error()})
			} else if active != 0 {
				artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionActive, Detail: fmt.Sprintf("%d live Docker Sandbox instance(s) protect template cleanup", active)})
			}
		case storage.ArtifactDockerVolume:
			artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "prefix-era volumes require stronger role evidence than a name"})
		}
	}
}

func legacyEPARArtifact(artifact storage.Artifact) bool {
	value := strings.ToLower(artifact.Target.Locator)
	if artifact.Kind == storage.ArtifactSandboxTemplate && (value == "docker/sandbox-templates:shell-docker" || value == "docker.io/docker/sandbox-templates:shell-docker") {
		return false
	}
	switch artifact.Kind {
	case storage.ArtifactDockerImage:
		repository := value
		if cut := strings.LastIndex(repository, ":"); cut > strings.LastIndex(repository, "/") {
			repository = repository[:cut]
		}
		repository = strings.TrimPrefix(repository, "docker.io/library/")
		return strings.HasPrefix(repository, "epar-")
	case storage.ArtifactSandboxTemplate:
		return strings.HasPrefix(strings.TrimPrefix(value, "docker.io/library/"), "epar-")
	case storage.ArtifactDockerVolume:
		return strings.HasPrefix(value, "epar-")
	default:
		return false
	}
}

func removeUncertainProtections(values []storage.Protection) []storage.Protection {
	out := values[:0]
	for _, value := range values {
		if value.Kind != storage.ProtectionUncertain {
			out = append(out, value)
		}
	}
	return out
}

func dockerImageContainerBlockers(identity string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "ancestor="+identity, "--format", "{{.ID}}").Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(output)), nil
}

func activeSandboxCount() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	items, err := dockersandboxes.New("").Inventory(ctx)
	return len(items), err
}

type hostStorageExecutor struct {
	filesystem storage.ExactExecutor
	sandboxes  provider.TemplateArtifactCleaner
}

func newHostStorageExecutor(projectRoot string) (*hostStorageExecutor, error) {
	filesystem, err := storage.NewFilesystemExecutor(
		filepath.Join(projectRoot, ".local", "bin"),
		filepath.Join(projectRoot, "work", "template-builds", "docker-sandboxes"),
	)
	if err != nil {
		return nil, err
	}
	return &hostStorageExecutor{filesystem: filesystem, sandboxes: dockersandboxes.New("")}, nil
}

func (e *hostStorageExecutor) ObserveExact(ctx context.Context, target storage.Target) (storage.Observation, error) {
	switch target.Kind {
	case storage.TargetFile, storage.TargetDirectory:
		return e.filesystem.ObserveExact(ctx, target)
	case storage.TargetDockerImageTag:
		return observeDockerImageTarget(ctx, target)
	case storage.TargetSandboxTemplate:
		templates, err := dockersandboxes.New("").CachedTemplates(ctx)
		if err != nil {
			return storage.Observation{}, err
		}
		for _, item := range templates {
			if item.CacheID == target.Identity && item.Reference == target.Locator {
				return storage.Observation{Exists: true, Target: target}, nil
			}
		}
		return storage.Observation{Exists: false, Target: target}, nil
	case storage.TargetExternal:
		name, ok := strings.CutPrefix(target.Locator, "tart-image:")
		if !ok || strings.TrimSpace(name) == "" {
			return storage.Observation{}, fmt.Errorf("unsupported exact external storage target %q", target.Locator)
		}
		instances, err := tartprovider.New("", false).List(ctx)
		if err != nil {
			return storage.Observation{}, err
		}
		for _, instance := range instances {
			if instance.Name != name {
				continue
			}
			observed := target
			observed.Identity = instance.ProviderID
			return storage.Observation{Exists: true, Target: observed}, nil
		}
		return storage.Observation{Exists: false, Target: target}, nil
	default:
		return storage.Observation{}, fmt.Errorf("exact storage executor does not support %s", target.Kind)
	}
}

func (e *hostStorageExecutor) RemoveExact(ctx context.Context, removal storage.Removal) error {
	switch removal.Target.Kind {
	case storage.TargetFile, storage.TargetDirectory:
		return e.filesystem.RemoveExact(ctx, removal)
	case storage.TargetDockerImageTag:
		blockers, err := dockerImageContainerBlockers(removal.Target.Identity)
		if err != nil {
			return err
		}
		if len(blockers) != 0 {
			return fmt.Errorf("Docker containers still reference image %s", removal.Target.Identity)
		}
		observed, err := observeDockerImageTarget(ctx, removal.Target)
		if err != nil {
			return err
		}
		if !observed.Exists {
			return nil
		}
		return runExactStorageCommand(ctx, "docker", "image", "rm", removal.Target.Locator)
	case storage.TargetSandboxTemplate:
		if count, err := activeSandboxCount(); err != nil {
			return err
		} else if count != 0 {
			return fmt.Errorf("%d live Docker Sandbox instance(s) protect template cleanup", count)
		}
		return e.sandboxes.RemoveTemplate(ctx, provider.TemplateArtifact{Reference: removal.Target.Locator, CacheID: removal.Target.Identity, Digest: removal.Target.Fingerprint})
	case storage.TargetExternal:
		name, ok := strings.CutPrefix(removal.Target.Locator, "tart-image:")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("unsupported exact external storage target %q", removal.Target.Locator)
		}
		tart := tartprovider.New("", false)
		instances, err := tart.List(ctx)
		if err != nil {
			return err
		}
		var exact *provider.Instance
		for index := range instances {
			instance := instances[index]
			if instance.Source == name && instance.Name != name {
				return fmt.Errorf("Tart instance %q still references image %q", instance.Name, name)
			}
			if instance.Name == name {
				if instance.ProviderID != removal.Target.Identity {
					return errors.New("Tart image identity changed")
				}
				copy := instance
				exact = &copy
			}
		}
		if exact == nil {
			return nil
		}
		if strings.EqualFold(exact.State, "running") {
			return errors.New("Tart image is running")
		}
		return tart.Delete(ctx, exact.Name)
	default:
		return fmt.Errorf("exact storage executor does not support %s", removal.Target.Kind)
	}
}

func observeDockerImageTarget(ctx context.Context, target storage.Target) (storage.Observation, error) {
	command := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .}}", target.Locator)
	output, err := command.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		message := strings.ToLower(string(output))
		if errors.As(err, &exitErr) && (strings.Contains(message, "no such image") || strings.Contains(message, "not found")) {
			return storage.Observation{Exists: false, Target: target}, nil
		}
		return storage.Observation{}, fmt.Errorf("docker image inspect %s failed: %w: %s", target.Locator, err, strings.TrimSpace(string(output)))
	}
	var record struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(output, &record); err != nil {
		return storage.Observation{}, err
	}
	if record.ID != target.Identity {
		drifted := target
		drifted.Identity = record.ID
		return storage.Observation{Exists: true, Target: drifted}, nil
	}
	return storage.Observation{Exists: true, Target: target}, nil
}

func runExactStorageCommand(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func removeExecutedCatalogEntries(report storage.ExecutionReport, now time.Time) error {
	if report.RemovedCount == 0 {
		return nil
	}
	removed := make(map[string]bool)
	for _, entry := range report.Entries {
		if entry.Status == storage.ExecutionRemoved {
			removed[string(entry.Removal.Target.Kind)+"\x00"+entry.Removal.Target.Identity] = true
		}
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		return err
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		resources := value.Resources[:0]
		for _, resource := range value.Resources {
			targetKind := storage.TargetExternal
			switch resource.Kind {
			case "docker-image":
				targetKind = storage.TargetDockerImageTag
			case "sandbox-template":
				targetKind = storage.TargetSandboxTemplate
			case "provider-image":
				if runtime.GOOS == "windows" || filepath.IsAbs(resource.Locator) {
					targetKind = storage.TargetFile
				}
			case "template-staging-directory":
				targetKind = storage.TargetDirectory
			}
			if removed[string(targetKind)+"\x00"+resource.Identity] && len(resource.References) == 0 {
				continue
			}
			resources = append(resources, resource)
		}
		value.Resources = resources
		return nil
	})
	return err
}
