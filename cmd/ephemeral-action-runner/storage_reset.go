package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/invocation"
	"github.com/solutionforest/ephemeral-action-runner/internal/pool"
	"github.com/solutionforest/ephemeral-action-runner/internal/projectlayout"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

type storageResetReport struct {
	ProjectRoot      string                   `json:"projectRoot"`
	ConfigPath       string                   `json:"configPath"`
	ConfigID         string                   `json:"configId"`
	ApprovalHash     string                   `json:"approvalHash"`
	Plan             storage.Plan             `json:"plan"`
	SharedResources  []storageResetShared     `json:"sharedResources,omitempty"`
	MissingResources []storageResetMissing    `json:"missingResources,omitempty"`
	Execution        *storage.ExecutionReport `json:"execution,omitempty"`
}

type storageResetShared struct {
	Kind       string   `json:"kind"`
	Locator    string   `json:"locator"`
	Identity   string   `json:"identity"`
	Referenced []string `json:"referencedBy"`
}

type storageResetMissing struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Locator  string `json:"locator"`
	Identity string `json:"identity"`
}

func runStorageReset(args []string) error {
	fs := flag.NewFlagSet("storage reset", flag.ContinueOnError)
	cwd, _ := os.Getwd()
	projectRootFlag := fs.String("project-root", cwd, "project root containing EPAR state and artifacts")
	configPathFlag := fs.String("config", "", "configuration whose exact EPAR resources and generated state should be reset")
	jsonOutput := fs.Bool("json", false, "write the complete reset plan as JSON")
	execute := fs.Bool("execute", false, "execute the exact hash-approved reset plan")
	approvedPlan := fs.String("plan", "", "exact reset preview hash")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("storage reset does not accept positional arguments")
	}
	if strings.TrimSpace(*configPathFlag) == "" {
		return fmt.Errorf("storage reset requires --config so EPAR can identify exact ownership")
	}
	if *execute && strings.TrimSpace(*approvedPlan) == "" {
		return fmt.Errorf("storage reset execution requires the exact preview hash: storage reset --config <path> --execute --plan <hash>")
	}
	if !*execute && strings.TrimSpace(*approvedPlan) != "" {
		return fmt.Errorf("--plan is valid only with storage reset --execute")
	}

	projectRoot, cfg, configPath, _, err := loadStorageConfig(*projectRootFlag, *configPathFlag)
	if err != nil {
		return err
	}
	if configPath == "" {
		return fmt.Errorf("storage reset requires an existing configuration file")
	}
	var controllerLock, hostTrustLock io.Closer
	if *execute {
		manager := &pool.Manager{ProjectRoot: projectRoot, ConfigPath: configPath, Config: cfg}
		controllerLock, err = manager.AcquirePoolControllerLock()
		if err != nil {
			return fmt.Errorf("storage reset requires the controller to be stopped: %w", err)
		}
		defer controllerLock.Close()
		hostTrustLock, err = manager.AcquireHostTrustControllerLock()
		if err != nil {
			return fmt.Errorf("storage reset requires the host-trust wrapper to be stopped: %w", err)
		}
		defer hostTrustLock.Close()
		backendLocks, lockErr := acquireStorageResetBackendLocks(projectRoot, configPath)
		if lockErr != nil {
			return lockErr
		}
		defer func() {
			for index := len(backendLocks) - 1; index >= 0; index-- {
				_ = backendLocks[index].Close()
			}
		}()
	}
	now := time.Now().UTC()
	report, err := buildStorageResetReport(projectRoot, configPath, now)
	if err != nil {
		return err
	}
	if *execute {
		if strings.TrimSpace(*approvedPlan) != report.ApprovalHash {
			return fmt.Errorf("storage reset plan changed; review the new preview and approve hash %s", report.ApprovalHash)
		}
		executor, err := newHostStorageExecutor(projectRoot)
		if err != nil && report.Plan.RemovalCount != 0 {
			return err
		}
		execution := storage.ExecutionReport{PlanHash: report.Plan.Hash}
		if report.Plan.RemovalCount != 0 {
			execution, err = storage.Execute(context.Background(), report.Plan, report.Plan.Hash, executor)
			if err != nil {
				report.Execution = &execution
				if *jsonOutput {
					_ = json.NewEncoder(os.Stdout).Encode(report)
				}
				return err
			}
		}
		report.Execution = &execution
		if err := finalizeStorageResetCatalog(report.ConfigID, report.MissingResources, execution, time.Now().UTC()); err != nil {
			return fmt.Errorf("reset resources were removed but catalog reconciliation failed: %w", err)
		}
	}
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	printStorageResetReport(report)
	return nil
}

func acquireStorageResetBackendLocks(projectRoot, configPath string) ([]io.Closer, error) {
	configID, err := storagecatalog.ConfigID(projectRoot, configPath)
	if err != nil {
		return nil, err
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		return nil, err
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	backendSet := make(map[string]struct{})
	for _, resource := range value.Resources {
		for _, reference := range resource.References {
			if reference.ConfigID == configID && strings.TrimSpace(resource.BackendID) != "" {
				backendSet[resource.BackendID] = struct{}{}
				break
			}
		}
	}
	backends := make([]string, 0, len(backendSet))
	for backendID := range backendSet {
		backends = append(backends, backendID)
	}
	sort.Strings(backends)
	locks := make([]io.Closer, 0, len(backends))
	for _, backendID := range backends {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		lock, lockErr := store.AcquireBackendLock(ctx, backendID)
		cancel()
		if lockErr != nil {
			for index := len(locks) - 1; index >= 0; index-- {
				_ = locks[index].Close()
			}
			return nil, fmt.Errorf("acquire exact storage reset backend lock for %s: %w", backendID, lockErr)
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func buildStorageResetReport(projectRoot, configPath string, now time.Time) (storageResetReport, error) {
	configID, err := storagecatalog.ConfigID(projectRoot, configPath)
	if err != nil {
		return storageResetReport{}, err
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		return storageResetReport{}, err
	}
	catalogValue, err := store.Load(now)
	if err != nil {
		return storageResetReport{}, err
	}
	for _, record := range catalogValue.Configs {
		if record.ID == configID && record.ControllerLeaseUntil != nil && record.ControllerLeaseUntil.After(now) {
			return storageResetReport{}, fmt.Errorf("configuration %s has an active controller lease until %s; stop EPAR before reset", configPath, record.ControllerLeaseUntil.UTC().Format(time.RFC3339))
		}
	}

	localArtifacts, localRoots, err := storageResetLocalArtifacts(projectRoot, configPath, configID)
	if err != nil {
		return storageResetReport{}, err
	}
	artifacts := append([]storage.Artifact(nil), localArtifacts...)
	report := storageResetReport{ProjectRoot: projectRoot, ConfigPath: configPath, ConfigID: configID}
	executor, err := newHostStorageExecutor(projectRoot)
	if err != nil {
		return storageResetReport{}, err
	}
	for _, resource := range catalogValue.Resources {
		selected := false
		var otherReferences []string
		for _, reference := range resource.References {
			if reference.ConfigID == configID {
				selected = true
			} else {
				otherReferences = append(otherReferences, reference.ConfigID)
			}
		}
		if !selected {
			continue
		}
		if len(otherReferences) != 0 {
			sort.Strings(otherReferences)
			report.SharedResources = append(report.SharedResources, storageResetShared{Kind: resource.Kind, Locator: resource.Locator, Identity: resource.Identity, Referenced: otherReferences})
			continue
		}
		if resource.Kind == "docker-output-tag-claim" {
			continue
		}
		if (resource.Kind == "provider-image" || resource.Kind == "template-staging-directory" || resource.Kind == "prebuilt-package-archive") && filepath.IsAbs(resource.Locator) {
			if _, statErr := os.Lstat(resource.Locator); errors.Is(statErr, os.ErrNotExist) {
				report.MissingResources = append(report.MissingResources, storageResetMissing{Key: resource.Key, Kind: resource.Kind, Locator: resource.Locator, Identity: resource.Identity})
				continue
			}
		}
		artifact, ok := catalogStorageArtifact(catalogValue, resource, now)
		if !ok || artifact.Ownership.Kind != storage.OwnershipExact || artifact.Target.Match != storage.MatchExact {
			return storageResetReport{}, fmt.Errorf("catalog resource %s cannot be reset through an exact executor", resource.Key)
		}
		if storageTargetInsideAnyRoot(artifact.Target, localRoots) {
			continue
		}
		observation, observeErr := executor.ObserveExact(context.Background(), artifact.Target)
		if observeErr != nil {
			return storageResetReport{}, fmt.Errorf("observe exact reset resource %s: %w", resource.Key, observeErr)
		}
		if !observation.Exists {
			report.MissingResources = append(report.MissingResources, storageResetMissing{Key: resource.Key, Kind: resource.Kind, Locator: resource.Locator, Identity: resource.Identity})
			continue
		}
		if observation.Target != artifact.Target {
			return storageResetReport{}, fmt.Errorf("catalog resource %s changed identity; run storage status and do not reset it", resource.Key)
		}
		artifact.ID = "reset-external-" + resource.Key
		artifacts = append(artifacts, artifact)
	}
	plan, err := storage.ExplicitRemovalPlan(now, artifacts, "operator-requested exact configuration reset")
	if err != nil {
		return storageResetReport{}, err
	}
	report.Plan = plan
	report.ApprovalHash, err = storageResetApprovalHash(report)
	if err != nil {
		return storageResetReport{}, err
	}
	return report, nil
}

func storageResetApprovalHash(report storageResetReport) (string, error) {
	removals := make([]storage.Removal, 0, report.Plan.RemovalCount)
	for _, decision := range report.Plan.Decisions {
		if decision.Action == storage.ActionRemove {
			removals = append(removals, storage.Removal{ArtifactID: decision.Artifact.ID, Target: decision.Artifact.Target, SizeBytes: decision.Artifact.SizeBytes})
		}
	}
	approval := struct {
		ConfigID string                `json:"configId"`
		Removals []storage.Removal     `json:"removals,omitempty"`
		Shared   []storageResetShared  `json:"sharedResources,omitempty"`
		Missing  []storageResetMissing `json:"missingResources,omitempty"`
	}{ConfigID: report.ConfigID, Removals: removals, Shared: report.SharedResources, Missing: report.MissingResources}
	encoded, err := json.Marshal(approval)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func storageResetLocalArtifacts(projectRoot, configPath, configID string) ([]storage.Artifact, []string, error) {
	canonicalConfig, err := storagecatalog.CanonicalPath(configPath)
	if err != nil {
		return nil, nil, err
	}
	poolSum := sha256.Sum256([]byte(canonicalConfig))
	poolNamespace := hex.EncodeToString(poolSum[:8])
	paths := []string{
		filepath.Join(projectlayout.CacheRoot(projectRoot), "image", configID),
		filepath.Join(projectlayout.StateRoot(projectRoot), "image", configID),
		filepath.Join(projectlayout.StateRoot(projectRoot), "supervision", configID),
		filepath.Join(projectlayout.StateRoot(projectRoot), "pools", poolNamespace),
		filepath.Join(projectlayout.WorkRoot(projectRoot), "template-builds", "docker-sandboxes", configID),
	}
	var artifacts []storage.Artifact
	var roots []string
	for index, path := range paths {
		target, err := storage.SnapshotFilesystemTarget(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("inspect generated reset path %s: %w", path, err)
		}
		if target.Kind != storage.TargetDirectory {
			return nil, nil, fmt.Errorf("generated reset path is not a real directory: %s", path)
		}
		sizeBytes, err := storageResetDirectorySize(target.Locator)
		if err != nil {
			return nil, nil, err
		}
		artifacts = append(artifacts, storage.Artifact{ID: fmt.Sprintf("reset-local-%02d", index), Provider: "local", SurfaceID: "project-filesystem", Kind: storage.ArtifactOther, Target: target, Ownership: storage.Ownership{Kind: storage.OwnershipExact, OwnerID: configID, Evidence: "fixed EPAR generated config-scoped directory"}, SizeBytes: sizeBytes})
		roots = append(roots, target.Locator)
	}
	return artifacts, roots, nil
}

func storageResetDirectorySize(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("generated reset path contains a redirected or special entry: %s", path)
		}
		if info.Mode().IsRegular() && info.Size() > 0 {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

func storageTargetInsideAnyRoot(target storage.Target, roots []string) bool {
	if target.Kind != storage.TargetFile && target.Kind != storage.TargetDirectory {
		return false
	}
	for _, root := range roots {
		relative, err := filepath.Rel(root, target.Locator)
		if err == nil && (relative == "." || (relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
			return true
		}
	}
	return false
}

func finalizeStorageResetCatalog(configID string, missing []storageResetMissing, execution storage.ExecutionReport, now time.Time) error {
	removed := make(map[string]bool)
	missingKeys := make(map[string]bool, len(missing))
	for _, resource := range missing {
		missingKeys[resource.Key] = true
	}
	var removedDirectories []string
	for _, entry := range execution.Entries {
		if entry.Status == storage.ExecutionRemoved {
			removed[string(entry.Removal.Target.Kind)+"\x00"+entry.Removal.Target.Identity] = true
			if entry.Removal.Target.Kind == storage.TargetDirectory {
				removedDirectories = append(removedDirectories, entry.Removal.Target.Locator)
			}
		}
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		return err
	}
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		for _, record := range value.Configs {
			if record.ID == configID && record.ControllerLeaseUntil != nil && record.ControllerLeaseUntil.After(now) {
				return fmt.Errorf("configuration %s acquired a controller lease during reset", configID)
			}
		}
		storagecatalog.ReplaceConfigReferences(value, configID, nil, now)
		resources := value.Resources[:0]
		for _, resource := range value.Resources {
			if missingKeys[resource.Key] && len(resource.References) == 0 {
				continue
			}
			removedResource := removed[string(storage.TargetExternal)+"\x00"+resource.Identity] || removed[string(storage.TargetFile)+"\x00"+resource.Identity] || removed[string(storage.TargetDirectory)+"\x00"+resource.Identity] || removed[string(storage.TargetDockerImageTag)+"\x00"+resource.Identity] || removed[string(storage.TargetSandboxTemplate)+"\x00"+resource.Identity]
			if !removedResource && filepath.IsAbs(resource.Locator) {
				removedResource = storagePathInsideAnyRoot(resource.Locator, removedDirectories)
			}
			if removedResource && len(resource.References) == 0 {
				continue
			}
			resources = append(resources, resource)
		}
		value.Resources = resources
		return nil
	})
	return err
}

func storagePathInsideAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err == nil && (relative == "." || (relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
			return true
		}
	}
	return false
}

func printStorageResetReport(report storageResetReport) {
	fmt.Fprintf(os.Stdout, "Storage reset plan: %s\n", report.ApprovalHash)
	fmt.Fprintf(os.Stdout, "Configuration: %s (%s)\n", report.ConfigPath, report.ConfigID)
	for _, decision := range report.Plan.Decisions {
		fmt.Fprintf(os.Stdout, "Remove exact kind=%s identity=%s target=%s\n", decision.Artifact.Target.Kind, decision.Artifact.Target.Identity, decision.Artifact.Target.Locator)
	}
	for _, shared := range report.SharedResources {
		fmt.Fprintf(os.Stdout, "Keep shared kind=%s identity=%s target=%s referencedBy=%s\n", shared.Kind, shared.Identity, shared.Locator, strings.Join(shared.Referenced, ","))
	}
	for _, missing := range report.MissingResources {
		fmt.Fprintf(os.Stdout, "Forget already-missing kind=%s identity=%s target=%s\n", missing.Kind, missing.Identity, missing.Locator)
	}
	if report.Execution == nil {
		fmt.Fprintf(os.Stdout, "Preview only. Stop EPAR, review every exact target, then execute:\n  %s\n", invocation.Command("storage", "reset", "--config", report.ConfigPath, "--execute", "--plan", report.ApprovalHash))
		return
	}
	fmt.Fprintf(os.Stdout, "Reset complete: removed=%d reclaimed=%s. The configuration and user-owned keys, certificates, and scripts were preserved; the next start will reacquire required artifacts.\n", report.Execution.RemovedCount, formatStorageBytes(report.Execution.ReclaimedBytes))
}
