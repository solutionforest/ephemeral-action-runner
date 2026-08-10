package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	artifactimage "github.com/solutionforest/ephemeral-action-runner/internal/image"
	"github.com/solutionforest/ephemeral-action-runner/internal/invocation"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	providerregistry "github.com/solutionforest/ephemeral-action-runner/internal/provider/registry"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage/inventory"
)

type storageCommandReport struct {
	Inventory storageInventorySummary  `json:"inventory"`
	Plan      storage.Plan             `json:"plan"`
	Execution *storage.ExecutionReport `json:"execution,omitempty"`
	Legacy    bool                     `json:"legacy,omitempty"`
}

type storageInventorySummary struct {
	ProjectRoot string   `json:"projectRoot"`
	Provider    string   `json:"provider,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

func runStorage(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("storage requires subcommand: status or prune")
	}
	subcommand := args[0]
	if subcommand == "effective-go-cache-limit" {
		return runEffectiveGoCacheLimit(args[1:])
	}
	if subcommand != "status" && subcommand != "prune" {
		return fmt.Errorf("unknown storage subcommand %q", subcommand)
	}
	fs := flag.NewFlagSet("storage "+subcommand, flag.ContinueOnError)
	cwd, _ := os.Getwd()
	projectRootFlag := fs.String("project-root", cwd, "project root containing EPAR state and artifacts")
	configPathFlag := fs.String("config", "", "config file path; defaults to EPAR_CONFIG or .local/config.yml when present")
	providerFlag := fs.String("provider", "", "limit provider-specific inventory")
	operationFlag := fs.String("operation", "", "show the capacity plan for an operation such as template-build or instance-create")
	jsonOutput := fs.Bool("json", false, "write the complete storage report as JSON")
	execute := fs.Bool("execute", false, "execute the exact policy-selected prune plan")
	legacy := fs.Bool("legacy", false, "include prefix-era EPAR resources in an operator-approved exact preview")
	approvedPlan := fs.String("plan", "", "approved legacy preview plan hash")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("storage %s does not accept positional arguments", subcommand)
	}
	if subcommand == "status" && *execute {
		return fmt.Errorf("storage status does not support --execute")
	}
	if subcommand == "status" && (*legacy || strings.TrimSpace(*approvedPlan) != "") {
		return fmt.Errorf("storage status does not support --legacy or --plan")
	}
	if subcommand == "prune" && strings.TrimSpace(*operationFlag) != "" {
		return fmt.Errorf("storage prune does not support --operation")
	}
	if !*legacy && strings.TrimSpace(*approvedPlan) != "" {
		return fmt.Errorf("--plan is valid only with storage prune --legacy --execute")
	}
	if *legacy && *execute && strings.TrimSpace(*approvedPlan) == "" {
		return fmt.Errorf("legacy cleanup requires the exact preview hash: storage prune --legacy --execute --plan <hash>")
	}
	if *providerFlag != "" {
		if _, found := providerregistry.DescriptorFor(*providerFlag); !found {
			return provider.UnsupportedTypeError(*providerFlag)
		}
	}

	projectRoot, cfg, configPath, configTime, err := loadStorageConfig(*projectRootFlag, *configPathFlag)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if configPath != "" {
		if err := importNativeBootstrapAcquisition(projectRoot, configPath, now); err != nil {
			return err
		}
	}
	var selections []inventory.TemplateSelection
	activeTemplateRootDisk := ""
	legacyTemplateReceiptProtected := false
	staleTemplateReceiptWarning := ""
	if cfg.Provider.Type == "docker-sandboxes" {
		artifact, metadataSHA256, activatedAt, receiptErr := artifactimage.LoadDockerSandboxesReceiptForConfig(projectRoot, configPath)
		if errors.Is(receiptErr, os.ErrNotExist) {
			artifact, metadataSHA256, activatedAt, receiptErr = artifactimage.LoadDockerSandboxesReceipt(projectRoot)
			legacyTemplateReceiptProtected = receiptErr == nil
		}
		if receiptErr == nil {
			activeTemplateRootDisk = artifact.RootDisk
			selections = append(selections, inventory.TemplateSelection{
				Platform:       artifact.Platform,
				Tag:            artifact.Reference,
				TemplateDigest: artifact.Digest,
				MetadataSHA256: metadataSHA256,
				ActivatedAt:    activatedAt,
			})
		} else if !errors.Is(receiptErr, os.ErrNotExist) {
			staleTemplateReceiptWarning = fmt.Sprintf("The unpublished Docker Sandboxes receipt is stale and is not treated as active ownership evidence: %v. Normal startup will rebuild and replace it after exact Sandbox-cache readback.", receiptErr)
		}
	}
	currentExecutable, _ := os.Executable()
	configuredFiles := configuredStorageFiles(cfg, projectRoot, configTime)
	snapshot, err := inventory.Collect(inventory.Options{
		ProjectRoot:         projectRoot,
		Provider:            *providerFlag,
		Now:                 now,
		LogsRoot:            config.ProjectPath(projectRoot, cfg.Logging.Directory),
		NativeRoot:          filepath.Join(projectRoot, ".local", "bin"),
		TemplateRoot:        filepath.Join(projectRoot, "work", "template-builds", "docker-sandboxes"),
		CurrentExecutable:   currentExecutable,
		ConfiguredTemplates: selections,
		ConfiguredFiles:     configuredFiles,
	})
	if err != nil {
		return err
	}
	if legacyTemplateReceiptProtected {
		snapshot.Warnings = append(snapshot.Warnings, "The exact template in the retired shared Docker Sandboxes receipt remains protected for legacy cleanup; normal startup still requires regeneration into per-config state.")
	}
	if staleTemplateReceiptWarning != "" {
		snapshot.Warnings = append(snapshot.Warnings, staleTemplateReceiptWarning)
	}
	collectExternalStorage(&snapshot, *providerFlag, configPath)
	protectConfiguredSandboxTemplates(&snapshot, selections)
	catalogValue, catalogErr := addCatalogStorage(&snapshot, *providerFlag, now)
	if catalogErr != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("EPAR host resource catalog is unavailable; catalog-owned resources remain report-only: %v", catalogErr))
	}
	if *legacy {
		selectLegacyStorage(&snapshot, catalogValue, now)
		snapshot.Warnings = append(snapshot.Warnings, "Legacy preview is limited to resources visible on this host. Unregistered old EPAR checkouts and their intended references cannot be inferred.")
	}
	if cfg.Storage.AutomaticHousekeeping == config.StorageHousekeepingDisabled {
		for index := range snapshot.Artifacts {
			snapshot.Artifacts[index].Protections = append(snapshot.Artifacts[index].Protections, storage.Protection{
				Kind:   storage.ProtectionOperator,
				Detail: "storage.automaticHousekeeping is disabled",
			})
		}
	}
	policy, minimumFree, err := storagePolicyFromConfig(cfg.Storage)
	if err != nil {
		return err
	}
	storageProvider := cfg.Provider.Type
	if *providerFlag != "" {
		storageProvider = *providerFlag
	}
	storageConfig := cfg
	storageConfig.Provider.Type = storageProvider
	effectiveMinimumFree, err := config.EffectiveMinimumFreeBytes(storageConfig)
	if err != nil {
		return err
	}
	if effectiveMinimumFree > minimumFree {
		minimumFree = effectiveMinimumFree
	}
	operationName := strings.TrimSpace(*operationFlag)
	if operationName == "" {
		operationName = "controller-bootstrap"
	}
	operationPlan, err := storageStatusOperationPlan(context.Background(), storageConfig, projectRoot, operationName, minimumFree)
	if err != nil {
		return err
	}
	var capacityDomains []storage.CapacityDomain
	var operationPlans []storage.OperationPlan
	providerRuntime, runtimeErr := providerregistry.New(storageConfig, projectRoot, true)
	if runtimeErr != nil {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Provider storage surfaces are unavailable: %v", runtimeErr))
	} else {
		providerSnapshot, snapshotErr := providerRuntime.Storage.StorageSnapshot(context.Background(), provider.StorageRequest{
			OperationPlan: operationPlan,
			Now:           now,
		})
		if snapshotErr != nil {
			snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Provider storage surfaces are unavailable: %v", snapshotErr))
		} else {
			snapshot.Surfaces = mergeStorageSurfaces(snapshot.Surfaces, providerSnapshot.Surfaces)
			snapshot.Artifacts = append(snapshot.Artifacts, providerSnapshot.Artifacts...)
			snapshot.Warnings = append(snapshot.Warnings, providerSnapshot.Warnings...)
			capacityDomains = append(capacityDomains, providerSnapshot.Domains...)
			operationPlans = append(operationPlans, operationPlan)
		}
	}
	if storageProvider == "docker-sandboxes" {
		rootDisk := cfg.DockerSandboxes.RootDisk
		if rootDisk == config.DockerSandboxesAutomaticRootDisk {
			rootDisk = activeTemplateRootDisk
		}
		appendLogicalSurface := func(id, configured string) {
			if configured == "" && id == "docker-sandboxes-root-logical" {
				snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
					ID:             id,
					Provider:       "docker-sandboxes",
					Kind:           storage.SurfaceExternal,
					Classification: "logical",
					Sparse:         true,
					Confidence:     "pending-artifact-resolution",
					Advisory:       true,
					Capacity:       storage.Capacity{ObservedAt: now},
				})
				return
			}
			parsed, parseErr := config.ParseByteSize(configured)
			if parseErr != nil || parsed <= 0 {
				return
			}
			snapshot.Surfaces = append(snapshot.Surfaces, storage.Surface{
				ID:                  id,
				Provider:            "docker-sandboxes",
				Kind:                storage.SurfaceExternal,
				Classification:      "logical",
				Sparse:              true,
				VirtualMaximumBytes: uint64(parsed),
				Confidence:          "configured-logical-limit",
				Advisory:            true,
				Capacity:            storage.Capacity{ObservedAt: now},
			})
		}
		appendLogicalSurface("docker-sandboxes-root-logical", rootDisk)
		appendLogicalSurface("docker-sandboxes-inner-docker-logical", cfg.DockerSandboxes.DockerDisk)
	}
	previewRequest := snapshot.PreviewRequest(policy, nil)
	previewRequest.CapacityDomains = capacityDomains
	previewRequest.OperationPlans = operationPlans
	plan, err := storage.Preview(previewRequest)
	if err != nil {
		return err
	}
	report := storageCommandReport{
		Inventory: storageInventorySummary{
			ProjectRoot: projectRoot,
			Provider:    *providerFlag,
			Warnings:    snapshot.Warnings,
		},
		Plan:   plan,
		Legacy: *legacy,
	}

	if subcommand == "prune" && *execute {
		for _, decision := range plan.Decisions {
			if decision.Action == storage.ActionRemove {
				fmt.Fprintf(os.Stderr, "storage prune exact identity=%s kind=%s target=%s bytes=%d\n", decision.Artifact.Target.Identity, decision.Artifact.Target.Kind, decision.Artifact.Target.Locator, decision.Artifact.SizeBytes)
			}
		}
		if plan.RemovalCount > 0 {
			executor, err := newHostStorageExecutor(projectRoot)
			if err != nil {
				return err
			}
			planApproval := plan.Hash
			if *legacy {
				planApproval = strings.TrimSpace(*approvedPlan)
			}
			execution, err := storage.Execute(context.Background(), plan, planApproval, executor)
			report.Execution = &execution
			if catalogErr == nil {
				if updateErr := removeExecutedCatalogEntries(execution, time.Now().UTC()); updateErr != nil {
					report.Inventory.Warnings = append(report.Inventory.Warnings, fmt.Sprintf("exact removals completed but the host catalog could not be compacted: %v", updateErr))
				}
			}
			if err != nil {
				if *jsonOutput {
					_ = writeStorageJSON(report)
				}
				return err
			}
		} else {
			execution := storage.ExecutionReport{PlanHash: plan.Hash}
			report.Execution = &execution
		}
	}
	if *jsonOutput {
		return writeStorageJSON(report)
	}
	printStorageReport(subcommand, report)
	if configPath == "" {
		fmt.Fprintln(os.Stdout, "Configuration: defaults (no config file found)")
	} else {
		fmt.Fprintln(os.Stdout, "Configuration:", configPath)
	}
	return nil
}

func mergeStorageSurfaces(existing, measured []storage.Surface) []storage.Surface {
	result := append([]storage.Surface(nil), existing...)
	indices := make(map[string]int, len(result))
	for index, surface := range result {
		indices[surface.ID] = index
	}
	for _, surface := range measured {
		if index, found := indices[surface.ID]; found {
			result[index] = surface
			continue
		}
		indices[surface.ID] = len(result)
		result = append(result, surface)
	}
	return result
}

func storageStatusOperationPlan(ctx context.Context, cfg config.Config, projectRoot, operation string, minimumFree uint64) (storage.OperationPlan, error) {
	projectOnly := func(id string, bytes uint64) storage.OperationPlan {
		return storage.OperationPlan{
			ID:               id,
			Provider:         cfg.Provider.Type,
			MinimumFreeBytes: minimumFree,
			Phases: []storage.OperationPhase{{
				ID: id,
				Allocations: []storage.Allocation{{
					ID: "project-" + id, Role: storage.StorageRoleProject, Bytes: bytes,
				}},
			}},
		}
	}
	switch operation {
	case "controller-bootstrap", "storage-status":
		return projectOnly(operation, 0), nil
	case "source-update", "image-update", "image-update-upstream":
		return projectOnly(operation, 5*storage.GiB), nil
	case "instance-create":
		role := storage.StorageRoleProject
		switch cfg.Provider.Type {
		case "docker-container":
			role = storage.StorageRoleDockerEngine
		case "docker-sandboxes":
			role = storage.StorageRoleSandboxRuntime
		case "wsl":
			role = storage.StorageRoleWSLDistribution
		case "tart":
			role = storage.StorageRoleTartStore
		}
		return storage.OperationPlan{ID: operation, Provider: cfg.Provider.Type, MinimumFreeBytes: minimumFree, Phases: []storage.OperationPhase{{ID: operation, Allocations: []storage.Allocation{{ID: "provider-runtime-instance", Role: role, Bytes: 10 * storage.GiB}}}}}, nil
	case "template-build", "image-build", "image-pull", "template-import":
		estimate, err := storageStatusSourceEstimate(ctx, cfg, projectRoot)
		if err != nil {
			return storage.OperationPlan{}, fmt.Errorf("resolve %s storage estimate: %w", operation, err)
		}
		if operation == "template-import" {
			artifactPlan, err := artifactimage.PlanDockerSandboxesImportStorage(estimate, 0)
			if err != nil {
				return storage.OperationPlan{}, err
			}
			artifactPlan.OperationPlan.MinimumFreeBytes = minimumFree
			return artifactPlan.OperationPlan, nil
		}
		dockerDiskBytes := uint64(0)
		if cfg.Provider.Type == "docker-sandboxes" {
			parsed, err := config.ParseByteSize(cfg.DockerSandboxes.DockerDisk)
			if err != nil {
				return storage.OperationPlan{}, err
			}
			dockerDiskBytes = uint64(parsed)
		}
		artifactPlan, err := artifactimage.PlanArtifactStorage(cfg.Provider.Type, estimate, false, dockerDiskBytes)
		if err != nil {
			return storage.OperationPlan{}, err
		}
		artifactPlan.OperationPlan.ID = operation
		artifactPlan.OperationPlan.MinimumFreeBytes = minimumFree
		return artifactPlan.OperationPlan, nil
	default:
		return storage.OperationPlan{}, fmt.Errorf("unsupported storage operation %q", operation)
	}
}

func storageStatusSourceEstimate(ctx context.Context, cfg config.Config, projectRoot string) (artifactimage.SourceSizeEstimate, error) {
	if cfg.Provider.Type == "tart" {
		return artifactimage.SourceSizeEstimate{Confidence: artifactimage.EstimateDerived}, nil
	}
	if cfg.Provider.Type == "wsl" && cfg.Image.SourceType == config.ImageSourceRootFSTar {
		info, err := os.Stat(config.ProjectPath(projectRoot, cfg.Image.SourceImage))
		if err != nil {
			return artifactimage.SourceSizeEstimate{}, err
		}
		if info.Size() < 0 {
			return artifactimage.SourceSizeEstimate{}, fmt.Errorf("source rootfs reports a negative size")
		}
		return artifactimage.EstimateSourceSize(uint64(info.Size()), uint64(info.Size()))
	}
	guestPlatform, err := initDockerGuestPlatform(cfg.Provider.Type, initSandboxPromotionPlatform())
	if err != nil {
		return artifactimage.SourceSizeEstimate{}, err
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	source, err := initResolveDockerSandboxesSource(resolveCtx, cfg.Image.SourceImage, guestPlatform)
	if err != nil {
		return artifactimage.SourceSizeEstimate{}, err
	}
	return artifactimage.EstimateSourceSize(source.CompressedLayerBytes, 0)
}

func runEffectiveGoCacheLimit(args []string) error {
	fs := flag.NewFlagSet("storage effective-go-cache-limit", flag.ContinueOnError)
	cwd, _ := os.Getwd()
	projectRootFlag := fs.String("project-root", cwd, "project root containing EPAR configuration")
	configPathFlag := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("storage effective-go-cache-limit does not accept positional arguments")
	}
	projectRoot, cfg, configPath, _, err := loadStorageConfig(*projectRootFlag, *configPathFlag)
	if err != nil {
		return err
	}
	if configPath != "" {
		if err := importNativeBootstrapAcquisition(projectRoot, configPath, time.Now().UTC()); err != nil {
			return err
		}
	}
	if err := config.ValidateStorage(cfg.Storage); err != nil {
		return err
	}
	limit, err := config.ParseByteSize(cfg.Storage.GoCacheLimit)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, uint64(limit))
	return nil
}

func configuredStorageFiles(cfg config.Config, projectRoot string, configuredAt time.Time) []inventory.ConfiguredFile {
	if cfg.Provider.Type != "wsl" || strings.TrimSpace(cfg.Image.OutputImage) == "" {
		return nil
	}
	output := config.ProjectPath(projectRoot, cfg.Image.OutputImage)
	files := []inventory.ConfiguredFile{
		{Provider: "wsl", Role: "reusable-image", Path: output, Kind: storage.ArtifactProviderImage, Current: true, ConfiguredAt: configuredAt, ProtectionKind: storage.ProtectionConfiguration, ProtectionDetail: "current reusable WSL image"},
		{Provider: "wsl", Role: "image-manifest", Path: artifactimage.WSLImageManifestPath(output), Kind: storage.ArtifactOther, Current: true, ConfiguredAt: configuredAt, ProtectionKind: storage.ProtectionCertification, ProtectionDetail: "current WSL image manifest"},
	}
	if cfg.Image.SourceType == "docker-image" {
		rootfs := artifactimage.WSLSourceRootfsPath(output)
		files = append(files,
			inventory.ConfiguredFile{Provider: "wsl", Role: "source-rootfs-cache", Path: rootfs, Kind: storage.ArtifactProviderCache, Current: true, ConfiguredAt: configuredAt, ProtectionKind: storage.ProtectionLock, ProtectionDetail: "current reusable WSL source rootfs cache"},
			inventory.ConfiguredFile{Provider: "wsl", Role: "source-cache-manifest", Path: artifactimage.SourceCacheManifestPath(rootfs), Kind: storage.ArtifactOther, Current: true, ConfiguredAt: configuredAt, ProtectionKind: storage.ProtectionCertification, ProtectionDetail: "current WSL source cache manifest"},
		)
	}
	return files
}

func loadStorageConfig(projectRoot, explicitConfig string) (string, config.Config, string, time.Time, error) {
	absoluteRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", config.Config{}, "", time.Time{}, err
	}
	cfg := config.Default()
	configPath, err := resolveConfigPath(absoluteRoot, explicitConfig)
	if err != nil {
		return "", config.Config{}, "", time.Time{}, err
	}
	var configTime time.Time
	if configPath != "" {
		cfg, err = config.Load(configPath)
		if err != nil {
			return "", config.Config{}, "", time.Time{}, err
		}
		if err := config.ValidateStorage(cfg.Storage); err != nil {
			return "", config.Config{}, "", time.Time{}, err
		}
		if info, statErr := os.Stat(configPath); statErr == nil {
			configTime = info.ModTime().UTC()
		}
	}
	return absoluteRoot, cfg, configPath, configTime, nil
}

func storagePolicyFromConfig(cfg config.StorageConfig) (storage.Policy, uint64, error) {
	grace, err := time.ParseDuration(cfg.GracePeriod)
	if err != nil {
		return storage.Policy{}, 0, err
	}
	minimumFree, err := config.ParseByteSize(cfg.MinimumFree)
	if err != nil {
		return storage.Policy{}, 0, err
	}
	buildLimit, err := config.ParseByteSize(cfg.BuildCacheLimit)
	if err != nil {
		return storage.Policy{}, 0, err
	}
	goLimit, err := config.ParseByteSize(cfg.GoCacheLimit)
	if err != nil {
		return storage.Policy{}, 0, err
	}
	return storage.Policy{
		GracePeriod:  grace,
		KeepPrevious: cfg.KeepPrevious,
		Budgets: []storage.Budget{
			{Kind: storage.ArtifactBuildKitCache, MaxBytes: uint64(buildLimit)},
			{Kind: storage.ArtifactGoCache, MaxBytes: uint64(goLimit)},
		},
	}, uint64(minimumFree), nil
}

func writeStorageJSON(report storageCommandReport) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func printStorageReport(subcommand string, report storageCommandReport) {
	fmt.Fprintf(os.Stdout, "Storage %s plan: %s\n", subcommand, report.Plan.Hash)
	for _, surface := range report.Plan.Surfaces {
		available := "unknown"
		if surface.Capacity.Known {
			available = formatStorageBytes(surface.Capacity.AvailableBytes)
		}
		fmt.Fprintf(os.Stdout, "Surface %s\tprovider=%s\tkind=%s\tclassification=%s\tsparse=%t\tavailable=%s\tallocated=%s\tvirtualMaximum=%s\tconfidence=%s\tauthoritative=%t\tadvisory=%t\tlocation=%s\n", surface.ID, valueOrDash(surface.Provider), surface.Kind, valueOrDash(surface.Classification), surface.Sparse, available, formatStorageOptionalBytes(surface.AllocatedBytes), formatStorageOptionalBytes(surface.VirtualMaximumBytes), valueOrDash(surface.Confidence), surface.AdmissionAuthoritative, surface.Advisory, surface.Location)
	}
	for _, domain := range report.Plan.CapacityDomains {
		available := "unknown"
		if domain.Capacity.Known {
			available = formatStorageBytes(domain.Capacity.AvailableBytes)
		}
		fmt.Fprintf(os.Stdout, "Domain %s\tkind=%s\tavailable=%s\tconfidence=%s\tprovenance=%s\tlocation=%s\n", domain.ID, domain.Kind, available, valueOrDash(domain.Confidence), valueOrDash(domain.Provenance), domain.Path)
	}
	for _, operation := range report.Plan.OperationPlans {
		fmt.Fprintf(os.Stdout, "Operation %s\tprovider=%s\treserve=%s\n", operation.ID, valueOrDash(operation.Provider), formatStorageBytes(operation.MinimumFreeBytes))
		for _, phase := range operation.Phases {
			fmt.Fprintf(os.Stdout, "Phase %s\toperation=%s\n", phase.ID, operation.ID)
		}
	}
	for _, allocation := range report.Plan.ResolvedAllocations {
		fmt.Fprintf(os.Stdout, "Allocation %s\toperation=%s\tphase=%s\trole=%s\tsurface=%s\tdomain=%s\tbytes=%s\n", allocation.AllocationID, allocation.OperationID, allocation.PhaseID, valueOrDash(string(allocation.Role)), allocation.SurfaceID, allocation.DomainID, formatStorageBytes(allocation.Bytes))
	}
	for _, check := range report.Plan.CapacityChecks {
		available := "unknown"
		if check.Capacity.Known {
			available = formatStorageBytes(check.Capacity.AvailableBytes)
		}
		if check.DomainRequirement != nil {
			fmt.Fprintf(os.Stdout, "Capacity %s\toperation=%s\tdomain=%s\tstatus=%s\tavailable=%s\tphasePeak=%s\treserve=%s\trequired=%s\treason=%s\n", check.Requirement.ID, check.DomainRequirement.OperationID, check.DomainRequirement.DomainID, check.Status, available, formatStorageBytes(check.DomainRequirement.PeakBytes), formatStorageBytes(check.DomainRequirement.MinimumFreeBytes), formatStorageBytes(check.RequiredAvailableBytes), check.Reason)
			continue
		}
		fmt.Fprintf(os.Stdout, "Capacity %s\tstatus=%s\tavailable=%s\testimated=%s\treserve=%s\trequired=%s\treason=%s\n", check.Requirement.ID, check.Status, available, formatStorageBytes(check.Requirement.PeakBytes), formatStorageBytes(check.Requirement.MinimumFreeBytes), formatStorageBytes(check.RequiredAvailableBytes), check.Reason)
	}
	for _, decision := range report.Plan.Decisions {
		fmt.Fprintf(os.Stdout, "Artifact %s\taction=%s\tprovider=%s\tkind=%s\tbytes=%s\tidentity=%s\ttarget=%s\treason=%s\n", decision.Artifact.ID, decision.Action, valueOrDash(decision.Artifact.Provider), decision.Artifact.Kind, formatStorageBytes(decision.Artifact.SizeBytes), valueOrDash(decision.Artifact.Target.Identity), decision.Artifact.Target.Locator, strings.Join(decision.Reasons, ","))
	}
	for _, warning := range append(append([]string(nil), report.Inventory.Warnings...), report.Plan.Warnings...) {
		fmt.Fprintln(os.Stdout, "Warning:", warning)
	}
	fmt.Fprintf(os.Stdout, "Summary: removals=%d reclaimable=%s", report.Plan.RemovalCount, formatStorageBytes(report.Plan.ReclaimableBytes))
	if report.Execution != nil {
		fmt.Fprintf(os.Stdout, " removed=%d reclaimed=%s", report.Execution.RemovedCount, formatStorageBytes(report.Execution.ReclaimedBytes))
	}
	fmt.Fprintln(os.Stdout)
	if subcommand == "prune" && report.Execution == nil {
		if report.Legacy {
			fmt.Fprintf(os.Stdout, "Preview only. Legacy execution requires this exact plan:\n  %s\n", invocation.Command("storage", "prune", "--legacy", "--execute", "--plan", report.Plan.Hash))
		} else {
			fmt.Fprintln(os.Stdout, "Preview only. Re-run with --execute to apply only the exact identities marked remove.")
		}
	}
}

func formatStorageOptionalBytes(value uint64) string {
	if value == 0 {
		return "-"
	}
	return formatStorageBytes(value)
}

func formatStorageBytes(value uint64) string {
	const gib = uint64(1 << 30)
	const mib = uint64(1 << 20)
	switch {
	case value >= gib:
		return fmt.Sprintf("%.2fGiB", float64(value)/float64(gib))
	case value >= mib:
		return fmt.Sprintf("%.2fMiB", float64(value)/float64(mib))
	default:
		return fmt.Sprintf("%dB", value)
	}
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
