package image

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	sandboxcapacity "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/capacity"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

var buildxUsageSizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kMGTPE]?i?B)$`)

const (
	catalogDockerImageKind     = "docker-image"
	catalogSandboxTemplateKind = "sandbox-template"
	catalogWSLArtifactKind     = "provider-image"
	catalogTartImageKind       = "tart-image"
	catalogTemplateStagingKind = "template-staging-directory"
)

func (m *Coordinator) effectiveConfigPath() string {
	if strings.TrimSpace(m.ConfigPath) != "" {
		return m.ConfigPath
	}
	return filepath.Join(m.ProjectRoot, ".local", "config.yml")
}

func (m *Coordinator) hostCatalog() (*storagecatalog.Store, error) {
	return storagecatalog.Open("")
}

func (m *Coordinator) catalogInstallationID(now time.Time) (string, error) {
	store, err := m.hostCatalog()
	if err != nil {
		return "", err
	}
	var installationID string
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		record, err := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if err == nil {
			installationID = record.InstallationID
			err = m.applyCatalogConfigSettings(value, record.ID)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if installationID == "" {
		return "", errors.New("registered EPAR installation identity disappeared")
	}
	return installationID, nil
}

func (m *Coordinator) applyCatalogConfigSettings(value *storagecatalog.Catalog, configID string) error {
	configured := strings.TrimSpace(m.Config.Storage.BuildCacheLimit)
	if configured == "" {
		configured = "20GiB"
	}
	limit, err := config.ParseByteSize(configured)
	if err != nil {
		return err
	}
	for index := range value.Configs {
		if value.Configs[index].ID == configID {
			value.Configs[index].BuildCacheLimitBytes = uint64(limit)
			return nil
		}
	}
	return fmt.Errorf("registered catalog configuration %s disappeared", configID)
}

func (m *Coordinator) dockerBackendID(ctx context.Context) (string, error) {
	id, err := m.runHostOutput(ctx, "docker", "info", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("Docker Engine returned an empty daemon identity")
	}
	return "docker:" + id, nil
}

func (m *Coordinator) acquireDockerBackendLock(ctx context.Context) (string, func(), error) {
	backendID, err := m.dockerBackendID(ctx)
	if err != nil {
		return "", nil, err
	}
	store, err := m.hostCatalog()
	if err != nil {
		return "", nil, err
	}
	lock, err := store.AcquireBackendLock(ctx, backendID)
	if err != nil {
		return "", nil, err
	}
	return backendID, func() {
		if closeErr := lock.Close(); closeErr != nil {
			m.warnf("EPAR Docker backend lock release warning: %v\n", closeErr)
		}
	}, nil
}

func backendPathID(kind, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical := filepath.Clean(absolute)
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%s:%x", kind, sum[:12]), nil
}

func sandboxBackendID() (string, error) {
	root, err := sandboxcapacity.DockerSandboxesStorageRoot()
	if err != nil {
		return "", err
	}
	return backendPathID("sandbox", root)
}

func (m *Coordinator) withSandboxBackendLock(ctx context.Context, operation func() error) error {
	backendID, err := sandboxBackendID()
	if err != nil {
		return err
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	lock, err := store.AcquireBackendLock(ctx, backendID)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			m.warnf("EPAR Docker Sandboxes backend lock release warning: %v\n", closeErr)
		}
	}()
	return operation()
}

func tartBackendID() (string, error) {
	root := strings.TrimSpace(os.Getenv("TART_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".tart")
	}
	return backendPathID("tart", root)
}

func (m *Coordinator) withTartBackendLock(ctx context.Context, operation func() error) error {
	backendID, err := tartBackendID()
	if err != nil {
		return err
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	lock, err := store.AcquireBackendLock(ctx, backendID)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			m.warnf("EPAR Tart backend lock release warning: %v\n", closeErr)
		}
	}()
	return operation()
}

func (m *Coordinator) recordCurrentArtifact(ctx context.Context, manifestHash string) error {
	if m.DryRun {
		return nil
	}
	now := time.Now().UTC()
	var resource storagecatalog.Resource
	switch m.Config.Provider.Type {
	case "docker-container":
		reference := strings.TrimSpace(m.Config.Image.OutputImage)
		identity, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", reference)
		if err != nil {
			return fmt.Errorf("read current Docker Container image identity: %w", err)
		}
		backendID, err := m.dockerBackendID(ctx)
		if err != nil {
			return err
		}
		resource = storagecatalog.Resource{
			BackendID: backendID, Kind: catalogDockerImageKind, Provider: "docker-container", Role: "runtime-image",
			Locator: reference, Identity: strings.TrimSpace(identity), Custody: storagecatalog.CustodyGenerated,
			ManifestHash: manifestHash, IntroducedTags: []string{reference}, State: storagecatalog.StateCurrent,
			CreatedAt: now, LastSeenAt: now,
		}
	case "wsl":
		path := filepath.Clean(configPath(m.ProjectRoot, m.Config.Image.OutputImage))
		target, err := storage.SnapshotFilesystemTarget(path)
		if err != nil {
			return fmt.Errorf("read current WSL artifact identity: %w", err)
		}
		resource = storagecatalog.Resource{
			BackendID: "filesystem:" + filepath.VolumeName(target.Locator), Kind: catalogWSLArtifactKind, Provider: "wsl", Role: "runtime-rootfs",
			Locator: target.Locator, Identity: target.Identity, Fingerprint: target.Fingerprint, Custody: storagecatalog.CustodyGenerated,
			ManifestHash: manifestHash, State: storagecatalog.StateCurrent, CreatedAt: now, LastSeenAt: now,
		}
	case "tart":
		output := strings.TrimSpace(m.Config.Image.OutputImage)
		items, err := m.Lifecycle.Inventory(ctx)
		if err != nil {
			return fmt.Errorf("read current Tart image identity: %w", err)
		}
		var exact provider.Instance
		for _, item := range items {
			if item.Instance.Name == output {
				exact = item.Instance
				break
			}
		}
		if exact.ProviderID == "" {
			return fmt.Errorf("current Tart image %q has no immutable provider identity", output)
		}
		backendID, err := tartBackendID()
		if err != nil {
			return err
		}
		resource = storagecatalog.Resource{
			BackendID: backendID, Kind: catalogTartImageKind, Provider: "tart", Role: "runtime-image",
			Locator: output, Identity: exact.ProviderID, Custody: storagecatalog.CustodyGenerated,
			ManifestHash: manifestHash, State: storagecatalog.StateCurrent, CreatedAt: now, LastSeenAt: now,
		}
	default:
		return nil
	}
	if err := m.registerCurrentCatalogResource(ctx, resource, manifestHash, now); err != nil {
		return err
	}
	return m.releaseCatalogRole("build-source", now)
}

// recordTartStagingImage records an exact temporary Tart image while the
// caller holds the Tart backend lock. Its per-config staging reference protects
// rollback evidence until startup reconciliation has restored or confirmed the
// configured output image.
func (m *Coordinator) recordTartStagingImage(ctx context.Context, name, role string) error {
	items, err := m.Provider.List(ctx)
	if err != nil {
		return err
	}
	exact, found := findTartImage(items, name)
	if !found || exact.ProviderID == "" {
		return fmt.Errorf("Tart image %q has no exact immutable identity", name)
	}
	backendID, err := tartBackendID()
	if err != nil {
		return err
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, err := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if err != nil {
			return err
		}
		if err := m.applyCatalogConfigSettings(value, configRecord.ID); err != nil {
			return err
		}
		resource := storagecatalog.Resource{
			BackendID: backendID, InstallationIDs: []string{configRecord.InstallationID}, Kind: catalogTartImageKind,
			Provider: "tart", Role: role, Locator: name, Identity: exact.ProviderID,
			Custody: storagecatalog.CustodyGenerated, State: storagecatalog.StateStaging,
			CreatedAt: now, LastSeenAt: now,
			References: []storagecatalog.Reference{{
				ConfigID: configRecord.ID, Role: "tart-staging", UpdatedAt: now,
			}},
		}
		return storagecatalog.UpsertResource(value, resource)
	})
	return err
}

func configPath(projectRoot, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(projectRoot, path)
}

func (m *Coordinator) recordCurrentSandboxArtifact(ctx context.Context, artifact provider.TemplateArtifact, manifestHash string, now time.Time) error {
	backendID, err := sandboxBackendID()
	if err != nil {
		return err
	}
	resource := storagecatalog.Resource{
		BackendID: backendID, Kind: catalogSandboxTemplateKind, Provider: "docker-sandboxes", Role: "runtime-template",
		Locator: artifact.Reference, Identity: artifact.CacheID, Fingerprint: artifact.Digest,
		Custody: storagecatalog.CustodyGenerated, ManifestHash: manifestHash, State: storagecatalog.StateCurrent,
		CreatedAt: now, LastSeenAt: now,
	}
	if err := m.registerCurrentCatalogResource(ctx, resource, manifestHash, now); err != nil {
		return err
	}
	return m.releaseCatalogRole("build-source", now)
}

func (m *Coordinator) recordSandboxWorkspace(ctx context.Context, workspacePath, manifestHash string, state storagecatalog.State, now time.Time) error {
	if state != storagecatalog.StateStaging && state != storagecatalog.StateSuperseded {
		return fmt.Errorf("unsupported Docker Sandboxes workspace state %q", state)
	}
	stagingDirectory, err := storage.SnapshotFilesystemTarget(workspacePath)
	if err != nil {
		return fmt.Errorf("read Docker Sandboxes staging directory identity: %w", err)
	}
	resource := storagecatalog.Resource{
		BackendID: "filesystem:" + filepath.VolumeName(stagingDirectory.Locator), Kind: catalogTemplateStagingKind, Provider: "docker-sandboxes", Role: "template-archive-workspace",
		Locator: stagingDirectory.Locator, Identity: stagingDirectory.Identity, Fingerprint: stagingDirectory.Fingerprint, Custody: storagecatalog.CustodyGenerated,
		ManifestHash: manifestHash, State: state, CreatedAt: now, LastSeenAt: now,
	}
	if state == storagecatalog.StateSuperseded {
		supersededAt := now.UTC()
		resource.SupersededAt = &supersededAt
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, err := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if err != nil {
			return err
		}
		if err := m.applyCatalogConfigSettings(value, configRecord.ID); err != nil {
			return err
		}
		resource.InstallationIDs = unionStrings(resource.InstallationIDs, []string{configRecord.InstallationID})
		resource.Key = storagecatalog.ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
		for index := range value.Resources {
			candidate := &value.Resources[index]
			filtered := candidate.References[:0]
			for _, reference := range candidate.References {
				if reference.ConfigID == configRecord.ID && reference.Role == "template-staging" {
					continue
				}
				filtered = append(filtered, reference)
			}
			candidate.References = filtered
			if candidate.Key != resource.Key && len(candidate.References) == 0 && candidate.State == storagecatalog.StateStaging {
				supersededAt := now.UTC()
				candidate.State = storagecatalog.StateSuperseded
				candidate.SupersededAt = &supersededAt
			}
			if candidate.Key == resource.Key {
				resource.References = append(resource.References, candidate.References...)
			}
		}
		if state == storagecatalog.StateStaging {
			resource.References = append(resource.References, storagecatalog.Reference{
				ConfigID: configRecord.ID, ManifestHash: manifestHash, Role: "template-staging", UpdatedAt: now.UTC(),
			})
			resource.SupersededAt = nil
		} else if len(resource.References) != 0 {
			resource.State = storagecatalog.StateStaging
			resource.SupersededAt = nil
		}
		if err := storagecatalog.UpsertResource(value, resource); err != nil {
			return err
		}
		return nil
	})
	return err
}

func (m *Coordinator) recordDockerSourceAcquisition(ctx context.Context, reference, previousID, currentID string, now time.Time) error {
	return m.recordDockerRoleAcquisition(ctx, "build-source", reference, previousID, currentID, now)
}

func (m *Coordinator) recordDockerRoleAcquisition(ctx context.Context, role, reference, previousID, currentID string, now time.Time) error {
	if m.DryRun || currentID == "" {
		return nil
	}
	backendID, err := m.dockerBackendID(ctx)
	if err != nil {
		return err
	}
	tag := reference
	if strings.LastIndex(tag, ":") <= strings.LastIndex(tag, "/") {
		tag += ":latest"
	}
	resource := storagecatalog.Resource{
		BackendID: backendID, Kind: catalogDockerImageKind, Role: role, Locator: tag, Identity: currentID,
		Custody: storagecatalog.CustodyAcquired, State: storagecatalog.StateStaging,
		CreatedAt: now, LastSeenAt: now,
	}
	if previousID == "" {
		resource.IntroducedTags = []string{tag}
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, err := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if err != nil {
			return err
		}
		if err := m.applyCatalogConfigSettings(value, configRecord.ID); err != nil {
			return err
		}
		resource.Key = storagecatalog.ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
		if currentID == previousID {
			found := false
			for _, existing := range value.Resources {
				if existing.Key == resource.Key && existing.Custody == storagecatalog.CustodyAcquired {
					resource = existing
					resource.Role = role
					resource.LastSeenAt = now
					found = true
					break
				}
			}
			if !found {
				completeDockerAcquisitionJournal(value, dockerAcquisitionJournalID(configRecord.ID, backendID, role, tag), now)
				return nil
			}
		}
		resource.InstallationIDs = unionStrings(resource.InstallationIDs, []string{configRecord.InstallationID})
		if err := storagecatalog.UpsertResource(value, resource); err != nil {
			return err
		}
		storagecatalog.ReplaceConfigRoleReferences(value, configRecord.ID, role, map[string]storagecatalog.Reference{
			resource.Key: {},
		}, now)
		completeDockerAcquisitionJournal(value, dockerAcquisitionJournalID(configRecord.ID, backendID, role, tag), now)
		return nil
	})
	return err
}

func (m *Coordinator) beginDockerRoleAcquisition(backendID, role, reference, previousID string, now time.Time) error {
	tag := reference
	if strings.LastIndex(tag, ":") <= strings.LastIndex(tag, "/") {
		tag += ":latest"
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, registerErr := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if registerErr != nil {
			return registerErr
		}
		if settingsErr := m.applyCatalogConfigSettings(value, configRecord.ID); settingsErr != nil {
			return settingsErr
		}
		id := dockerAcquisitionJournalID(configRecord.ID, backendID, role, tag)
		for index := range value.Journals {
			if value.Journals[index].ID == id {
				value.Journals[index].Phase = "acquiring"
				value.Journals[index].PreviousIdentity = previousID
				value.Journals[index].UpdatedAt = now
				value.Journals[index].Error = ""
				return nil
			}
		}
		value.Journals = append(value.Journals, storagecatalog.Journal{
			ID: id, Operation: "docker-image-acquisition", BackendID: backendID, ConfigID: configRecord.ID,
			Role: role, Locator: tag, PreviousIdentity: previousID, Phase: "acquiring", StartedAt: now, UpdatedAt: now,
		})
		return nil
	})
	return err
}

func dockerAcquisitionJournalID(configID, backendID, role, locator string) string {
	sum := sha256.Sum256([]byte(configID + "\x00" + backendID + "\x00" + role + "\x00" + locator))
	return fmt.Sprintf("acquire-%x", sum[:12])
}

func completeDockerAcquisitionJournal(value *storagecatalog.Catalog, id string, now time.Time) {
	for index := range value.Journals {
		if value.Journals[index].ID == id {
			value.Journals[index].Phase = "complete"
			value.Journals[index].UpdatedAt = now
			value.Journals[index].Error = ""
			return
		}
	}
}

func (m *Coordinator) releaseCatalogRole(role string, now time.Time) error {
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, err := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if err != nil {
			return err
		}
		if err := m.applyCatalogConfigSettings(value, configRecord.ID); err != nil {
			return err
		}
		storagecatalog.ReplaceConfigRoleReferences(value, configRecord.ID, role, nil, now)
		return nil
	})
	return err
}

func (m *Coordinator) registerCurrentCatalogResource(ctx context.Context, resource storagecatalog.Resource, manifestHash string, now time.Time) error {
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	backendLock, err := store.AcquireBackendLock(ctx, resource.BackendID)
	if err != nil {
		return err
	}
	defer backendLock.Close()
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, err := storagecatalog.RegisterConfig(value, m.ProjectRoot, m.effectiveConfigPath(), now)
		if err != nil {
			return err
		}
		if err := m.applyCatalogConfigSettings(value, configRecord.ID); err != nil {
			return err
		}
		resource.Key = storagecatalog.ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
		var existingReferences []storagecatalog.Reference
		for _, existing := range value.Resources {
			if existing.Key == resource.Key {
				existingReferences = append(existingReferences, existing.References...)
				if resource.CreatedAt.IsZero() {
					resource.CreatedAt = existing.CreatedAt
				}
				resource.IntroducedTags = unionStrings(existing.IntroducedTags, resource.IntroducedTags)
				resource.InstallationIDs = unionStrings(existing.InstallationIDs, resource.InstallationIDs)
				break
			}
		}
		resource.InstallationIDs = unionStrings(resource.InstallationIDs, []string{configRecord.InstallationID})
		resource.References = existingReferences
		if err := storagecatalog.UpsertResource(value, resource); err != nil {
			return err
		}
		storagecatalog.ReplaceConfigRoleReferences(value, configRecord.ID, "provider-artifact", map[string]storagecatalog.Reference{
			resource.Key: {ManifestHash: manifestHash},
		}, now)
		return nil
	})
	return err
}

func unionStrings(left, right []string) []string {
	set := make(map[string]struct{}, len(left)+len(right))
	for _, value := range append(append([]string(nil), left...), right...) {
		if strings.TrimSpace(value) != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (m *Coordinator) cleanupSupersededCatalog(ctx context.Context) error {
	if m.DryRun || strings.EqualFold(m.Config.Storage.AutomaticHousekeeping, "disabled") {
		return nil
	}
	if err := m.reconcileInterruptedDockerAcquisitions(ctx); err != nil {
		m.warnf("EPAR interrupted Docker acquisition reconciliation deferred: %v\n", err)
	}
	if err := m.reconcileInterruptedTartArtifacts(ctx); err != nil {
		return fmt.Errorf("reconcile interrupted Tart artifact activation: %w", err)
	}
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	if err := m.enforceDedicatedBuildxCache(ctx); err != nil {
		m.warnf("EPAR BuildKit cache housekeeping deferred: %v\n", err)
	}
	now := time.Now().UTC()
	var reconcileWarnings []string
	value, err := store.WithLock(now, func(value *storagecatalog.Catalog) error {
		reconcileWarnings = storagecatalog.Compact(value, now, func(resource storagecatalog.Resource) (bool, error) {
			return m.catalogResourceExists(ctx, resource)
		})
		return nil
	})
	if err != nil {
		return err
	}
	for _, warning := range reconcileWarnings {
		m.warnf("EPAR storage catalog reconciliation: %s\n", warning)
	}
	if m.Config.Storage.KeepPrevious > 0 {
		m.infof("automatic artifact retirement is deferred because storage.keepPrevious=%d; use storage prune to preview the retention policy\n", m.Config.Storage.KeepPrevious)
		return nil
	}
	for _, resource := range value.Resources {
		if len(resource.References) != 0 || (resource.State != storagecatalog.StateSuperseded && resource.State != storagecatalog.StateCleanupPending) {
			continue
		}
		backendLock, lockErr := store.AcquireBackendLock(ctx, resource.BackendID)
		if lockErr != nil {
			m.warnf("EPAR storage cleanup backend lock deferred for %s %s: %v\n", resource.Kind, resource.Identity, lockErr)
			continue
		}
		startedAt := time.Now().UTC()
		removeCandidate := resource
		shouldRemove := false
		if _, journalErr := store.WithLock(startedAt, func(current *storagecatalog.Catalog) error {
			for _, candidate := range current.Resources {
				if candidate.Key != resource.Key {
					continue
				}
				if len(candidate.References) != 0 || (candidate.State != storagecatalog.StateSuperseded && candidate.State != storagecatalog.StateCleanupPending) {
					return nil
				}
				removeCandidate = candidate
				shouldRemove = true
				upsertCleanupJournal(current, candidate, "remove-started", "", startedAt)
				return nil
			}
			return nil
		}); journalErr != nil {
			_ = backendLock.Close()
			return journalErr
		}
		if !shouldRemove {
			if closeErr := backendLock.Close(); closeErr != nil {
				m.warnf("EPAR storage cleanup backend lock release warning: %v\n", closeErr)
			}
			continue
		}
		cleanupLabel := fmt.Sprintf("EPAR superseded %s cleanup for %s", removeCandidate.Kind, removeCandidate.Identity)
		removeErr := m.runProgressOperation(cleanupLabel, nil, func() error {
			return m.removeCatalogResource(ctx, removeCandidate)
		})
		_, updateErr := store.WithLock(time.Now().UTC(), func(current *storagecatalog.Catalog) error {
			for index := range current.Resources {
				if current.Resources[index].Key != removeCandidate.Key {
					continue
				}
				if len(current.Resources[index].References) != 0 {
					return nil
				}
				if removeErr == nil {
					current.Resources = append(current.Resources[:index], current.Resources[index+1:]...)
					upsertCleanupJournal(current, removeCandidate, "complete", "", time.Now().UTC())
				} else {
					current.Resources[index].State = storagecatalog.StateCleanupPending
					current.Resources[index].CleanupError = removeErr.Error()
					upsertCleanupJournal(current, removeCandidate, "cleanup-pending", removeErr.Error(), time.Now().UTC())
				}
				return nil
			}
			return nil
		})
		closeErr := backendLock.Close()
		if updateErr != nil {
			return updateErr
		}
		if closeErr != nil {
			m.warnf("EPAR storage cleanup backend lock release warning: %v\n", closeErr)
		}
		if removeErr != nil {
			m.warnf("EPAR storage cleanup deferred for %s %s: %v\n", removeCandidate.Kind, removeCandidate.Identity, removeErr)
		}
	}
	return nil
}

func (m *Coordinator) reconcileInterruptedTartArtifacts(ctx context.Context) error {
	if m.Config.Provider.Type != "tart" {
		return nil
	}
	return m.withTartBackendLock(ctx, func() error {
		store, err := m.hostCatalog()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		value, err := store.Load(now)
		if err != nil {
			return err
		}
		configID, err := storagecatalog.ConfigID(m.ProjectRoot, m.effectiveConfigPath())
		if err != nil {
			return err
		}
		var staging []storagecatalog.Resource
		for _, resource := range value.Resources {
			if resource.Kind != catalogTartImageKind || resource.State != storagecatalog.StateStaging {
				continue
			}
			for _, reference := range resource.References {
				if reference.ConfigID == configID && reference.Role == "tart-staging" {
					staging = append(staging, resource)
					break
				}
			}
		}
		if len(staging) == 0 {
			return nil
		}
		instances, err := m.Provider.List(ctx)
		if err != nil {
			return err
		}
		outputName := strings.TrimSpace(m.Config.Image.OutputImage)
		if _, outputExists := findTartImage(instances, outputName); !outputExists {
			var rollback *storagecatalog.Resource
			for index := range staging {
				resource := &staging[index]
				if resource.Role != "activation-rollback" {
					continue
				}
				instance, exists := findTartImage(instances, resource.Locator)
				if exists && instance.ProviderID == resource.Identity {
					rollback = resource
					break
				}
			}
			if rollback == nil {
				return fmt.Errorf("configured Tart output %q is missing and no exact rollback image is available", outputName)
			}
			if err := m.Provider.Clone(ctx, rollback.Locator, outputName); err != nil {
				return fmt.Errorf("restore interrupted Tart activation from %q: %w", rollback.Locator, err)
			}
			if err := m.verifyTartImageIdentity(ctx, outputName); err != nil {
				return err
			}
			if m.environment != nil {
				m.warnf("restored Tart output image %s after an interrupted activation; the desired artifact will be reconciled next\n", outputName)
			}
		}
		_, err = store.WithLock(time.Now().UTC(), func(current *storagecatalog.Catalog) error {
			when := time.Now().UTC()
			for index := range current.Resources {
				resource := &current.Resources[index]
				if resource.Kind != catalogTartImageKind || resource.State != storagecatalog.StateStaging {
					continue
				}
				filtered := resource.References[:0]
				removed := false
				for _, reference := range resource.References {
					if reference.ConfigID == configID && reference.Role == "tart-staging" {
						removed = true
						continue
					}
					filtered = append(filtered, reference)
				}
				resource.References = filtered
				if removed && len(resource.References) == 0 {
					resource.State = storagecatalog.StateSuperseded
					resource.SupersededAt = &when
				}
			}
			return nil
		})
		return err
	})
}

func (m *Coordinator) StorageCleanupPending() (bool, error) {
	store, err := m.hostCatalog()
	if err != nil {
		return false, err
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		return false, err
	}
	for _, resource := range value.Resources {
		if resource.State == storagecatalog.StateCleanupPending {
			return true, nil
		}
	}
	for _, journal := range value.Journals {
		if journal.Phase == "cleanup-pending" {
			return true, nil
		}
	}
	return false, nil
}

func (m *Coordinator) reconcileInterruptedDockerAcquisitions(ctx context.Context) error {
	store, err := m.hostCatalog()
	if err != nil {
		return err
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		return err
	}
	for _, journal := range value.Journals {
		if journal.Operation != "docker-image-acquisition" || journal.Phase == "complete" {
			continue
		}
		lock, lockErr := store.AcquireBackendLock(ctx, journal.BackendID)
		if lockErr != nil {
			return lockErr
		}
		currentID, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", journal.Locator)
		if inspectErr != nil && !dockerInspectMeansMissing(inspectErr) {
			_ = lock.Close()
			return inspectErr
		}
		currentID = strings.TrimSpace(currentID)
		now := time.Now().UTC()
		_, updateErr := store.WithLock(now, func(current *storagecatalog.Catalog) error {
			if currentID != "" && currentID != journal.PreviousIdentity {
				installationIDs := []string{}
				for _, configRecord := range current.Configs {
					if configRecord.ID == journal.ConfigID {
						installationIDs = append(installationIDs, configRecord.InstallationID)
						break
					}
				}
				resource := storagecatalog.Resource{
					BackendID: journal.BackendID, InstallationIDs: installationIDs, Kind: catalogDockerImageKind,
					Role: journal.Role, Locator: journal.Locator, Identity: currentID, Custody: storagecatalog.CustodyAcquired,
					State: storagecatalog.StateSuperseded, CreatedAt: now, LastSeenAt: now,
				}
				if journal.PreviousIdentity == "" {
					resource.IntroducedTags = []string{journal.Locator}
				}
				when := now.UTC()
				resource.SupersededAt = &when
				if upsertErr := storagecatalog.UpsertResource(current, resource); upsertErr != nil {
					return upsertErr
				}
			}
			completeDockerAcquisitionJournal(current, journal.ID, now)
			return nil
		})
		closeErr := lock.Close()
		if updateErr != nil {
			return updateErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func upsertCleanupJournal(value *storagecatalog.Catalog, resource storagecatalog.Resource, phase, message string, now time.Time) {
	id := "cleanup-" + resource.Key
	for index := range value.Journals {
		if value.Journals[index].ID == id {
			value.Journals[index].Phase = phase
			value.Journals[index].UpdatedAt = now
			value.Journals[index].Error = message
			return
		}
	}
	value.Journals = append(value.Journals, storagecatalog.Journal{
		ID: id, Operation: "remove-exact-resource", ResourceKey: resource.Key, Phase: phase,
		StartedAt: now, UpdatedAt: now, Error: message,
	})
}

func (m *Coordinator) catalogResourceExists(ctx context.Context, resource storagecatalog.Resource) (bool, error) {
	switch resource.Kind {
	case catalogDockerImageKind:
		identity, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", resource.Identity)
		if err != nil {
			if dockerInspectMeansMissing(err) {
				return false, nil
			}
			return false, err
		}
		return strings.TrimSpace(identity) == resource.Identity, nil
	case catalogSandboxTemplateKind:
		observer, ok := m.Lifecycle.(provider.TemplateArtifactObserver)
		if !ok {
			return true, nil
		}
		return observer.ObserveTemplate(ctx, provider.TemplateArtifact{
			Reference: resource.Locator,
			CacheID:   resource.Identity,
			Digest:    resource.Fingerprint,
		})
	case catalogWSLArtifactKind:
		target, err := storage.SnapshotFilesystemTarget(resource.Locator)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return target.Identity == resource.Identity && target.Fingerprint == resource.Fingerprint, nil
	case catalogTemplateStagingKind:
		target, err := storage.SnapshotFilesystemTarget(resource.Locator)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return target.Identity == resource.Identity && target.Fingerprint == resource.Fingerprint, nil
	case catalogTartImageKind:
		if m.Config.Provider.Type != "tart" {
			return true, nil
		}
		items, err := m.Lifecycle.Inventory(ctx)
		if err != nil {
			return false, err
		}
		for _, item := range items {
			if item.Instance.Name == resource.Locator && item.Instance.ProviderID == resource.Identity {
				return true, nil
			}
		}
		return false, nil
	default:
		return true, nil
	}
}

func (m *Coordinator) enforceDedicatedBuildxCache(ctx context.Context) error {
	metadata, err := LoadBuildxMetadata(m.ProjectRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	limitBytes, err := m.effectiveProjectBuildCacheLimit()
	if err != nil {
		return err
	}
	if _, err := m.runHostOutput(ctx, "docker", "buildx", "inspect", metadata.Builder); err != nil {
		return nil
	}
	usageOutput, err := m.runHostOutput(ctx, "docker", "buildx", "du", "--builder", metadata.Builder, "--format", "json")
	if err != nil {
		return err
	}
	usageBytes, err := parseBuildxUsageBytes([]byte(usageOutput))
	if err != nil {
		return err
	}
	if usageBytes <= limitBytes {
		return nil
	}
	return m.runHostQuiet(ctx, "docker", "buildx", "prune", "--builder", metadata.Builder, "--force", "--max-used-space", strconv.FormatUint(limitBytes, 10)+"B")
}

func (m *Coordinator) effectiveProjectBuildCacheLimit() (uint64, error) {
	configured := strings.TrimSpace(m.Config.Storage.BuildCacheLimit)
	if configured == "" {
		configured = "20GiB"
	}
	current, err := config.ParseByteSize(configured)
	if err != nil {
		return 0, err
	}
	limit := uint64(current)
	store, err := m.hostCatalog()
	if err != nil {
		return 0, err
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		return 0, err
	}
	root, err := filepath.Abs(m.ProjectRoot)
	if err != nil {
		return 0, err
	}
	for _, configRecord := range value.Configs {
		sameRoot := filepath.Clean(configRecord.ProjectRoot) == filepath.Clean(root)
		if runtime.GOOS == "windows" {
			sameRoot = strings.EqualFold(filepath.Clean(configRecord.ProjectRoot), filepath.Clean(root))
		}
		if sameRoot && configRecord.BuildCacheLimitBytes > 0 && configRecord.BuildCacheLimitBytes < limit {
			limit = configRecord.BuildCacheLimitBytes
		}
	}
	return limit, nil
}

func parseBuildxUsageBytes(content []byte) (uint64, error) {
	type record struct {
		ID    string          `json:"ID"`
		Size  json.RawMessage `json:"Size"`
		Total json.RawMessage `json:"Total"`
	}
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return 0, nil
	}
	var records []record
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &records); err != nil {
			return 0, err
		}
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			var value record
			if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &value); err != nil {
				return 0, err
			}
			records = append(records, value)
		}
	}
	parse := func(raw json.RawMessage) (uint64, bool) {
		var numeric uint64
		if err := json.Unmarshal(raw, &numeric); err == nil {
			return numeric, true
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			if parsed, parseErr := config.ParseByteSize(text); parseErr == nil && parsed >= 0 {
				return uint64(parsed), true
			}
			if match := buildxUsageSizePattern.FindStringSubmatch(text); match != nil {
				if parsed, ok := parseBuildxBytes(match[1], match[2]); ok && parsed >= 0 {
					return uint64(parsed), true
				}
			}
		}
		return 0, false
	}
	var sum uint64
	for _, value := range records {
		if total, ok := parse(value.Total); ok {
			return total, nil
		}
		if value.ID == "" {
			continue
		}
		size, ok := parse(value.Size)
		if !ok || sum > ^uint64(0)-size {
			return 0, errors.New("Buildx disk-usage output is incomplete or overflows")
		}
		sum += size
	}
	return sum, nil
}

func (m *Coordinator) removeCatalogResource(ctx context.Context, resource storagecatalog.Resource) error {
	switch resource.Kind {
	case catalogDockerImageKind:
		containers, err := m.runHostOutput(ctx, "docker", "ps", "-a", "--filter", "ancestor="+resource.Identity, "--format", "{{.ID}}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(containers) != "" {
			return errors.New("a Docker container still references the image")
		}
		tagsJSON, err := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{json .RepoTags}}", resource.Identity)
		if err != nil {
			if dockerInspectMeansMissing(err) {
				return nil
			}
			return err
		}
		var tags []string
		if err := json.Unmarshal([]byte(strings.TrimSpace(tagsJSON)), &tags); err != nil {
			return err
		}
		introduced := make(map[string]bool, len(resource.IntroducedTags))
		for _, tag := range resource.IntroducedTags {
			introduced[tag] = true
		}
		for _, tag := range resource.IntroducedTags {
			tagID, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag)
			if inspectErr == nil && strings.TrimSpace(tagID) == resource.Identity {
				if err := m.runHostQuiet(ctx, "docker", "image", "rm", tag); err != nil {
					return err
				}
			}
		}
		remainingTagsJSON, inspectErr := m.runHostOutput(ctx, "docker", "image", "inspect", "--format", "{{json .RepoTags}}", resource.Identity)
		if inspectErr == nil {
			var remaining []string
			if err := json.Unmarshal([]byte(strings.TrimSpace(remainingTagsJSON)), &remaining); err != nil {
				return err
			}
			for _, tag := range remaining {
				if !introduced[tag] {
					return nil
				}
			}
			if len(remaining) != 0 {
				return fmt.Errorf("EPAR-introduced Docker image tags remain after exact removal: %s", strings.Join(remaining, ", "))
			}
			if err := m.runHostQuiet(ctx, "docker", "image", "rm", resource.Identity); err != nil {
				return err
			}
		}
		return nil
	case catalogSandboxTemplateKind:
		if resource.Locator == "docker.io/docker/sandbox-templates:shell-docker" || resource.Locator == "docker/sandbox-templates:shell-docker" {
			return errors.New("the Docker Sandboxes shell-docker base template is protected")
		}
		cleaner, ok := m.Lifecycle.(provider.TemplateArtifactCleaner)
		if !ok {
			return errors.New("provider does not expose exact template cleanup")
		}
		return cleaner.RemoveTemplate(ctx, provider.TemplateArtifact{Reference: resource.Locator, CacheID: resource.Identity, Digest: resource.Fingerprint})
	case catalogWSLArtifactKind:
		absolute, err := filepath.Abs(resource.Locator)
		if err != nil {
			return err
		}
		root, err := filepath.Abs(m.ProjectRoot)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("filesystem artifact belongs to another project and is deferred to its own controller")
		}
		target, err := storage.SnapshotFilesystemTarget(absolute)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if target.Identity != resource.Identity || target.Fingerprint != resource.Fingerprint || target.Kind != storage.TargetFile {
			return errors.New("filesystem artifact identity changed")
		}
		return os.Remove(absolute)
	case catalogTemplateStagingKind:
		executor, err := storage.NewFilesystemExecutor(filepath.Join(m.ProjectRoot, "work", "template-builds", "docker-sandboxes"))
		if err != nil {
			return err
		}
		target := storage.Target{Kind: storage.TargetDirectory, Locator: resource.Locator, Identity: resource.Identity, Fingerprint: resource.Fingerprint, Match: storage.MatchExact}
		observation, err := executor.ObserveExact(ctx, target)
		if err != nil {
			return err
		}
		if !observation.Exists {
			return nil
		}
		if observation.Target != target {
			return errors.New("template staging directory exact identity changed")
		}
		return executor.RemoveExact(ctx, storage.Removal{Target: target})
	case catalogTartImageKind:
		if m.Config.Provider.Type != "tart" {
			return errors.New("Tart image cleanup is deferred to a Tart controller on macOS")
		}
		items, err := m.Lifecycle.Inventory(ctx)
		if err != nil {
			return err
		}
		var exact *provider.Instance
		for index := range items {
			instance := items[index].Instance
			if instance.Source == resource.Locator && instance.Name != resource.Locator {
				return fmt.Errorf("Tart instance %q still references image %q", instance.Name, resource.Locator)
			}
			if instance.Name == resource.Locator {
				if instance.ProviderID != resource.Identity {
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
		return m.Lifecycle.Delete(ctx, *exact)
	default:
		return fmt.Errorf("automatic cleanup is not implemented for catalog resource kind %q", resource.Kind)
	}
}
