package inventory

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

var (
	cacheKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	leasePattern    = regexp.MustCompile(`^lease(?:[.-]).+$`)
)

type nativeOptions struct {
	Root              string
	CurrentExecutable string
	CurrentRevision   string
}

type nativeRevision struct {
	artifact  storage.Artifact
	key       string
	completed time.Time
	binary    storage.Target
}

func collectNative(options nativeOptions) ([]storage.Artifact, []string) {
	rootTarget, exists, err := inspectOptionalRoot(options.Root)
	if !exists {
		if err == nil {
			return nil, nil
		}
		artifact := unknownRootArtifact("native-controller", options.Root, "", err)
		return []storage.Artifact{artifact}, []string{fmt.Sprintf("native-controller root was not inventoried safely: %v", err)}
	}
	entries, err := os.ReadDir(rootTarget.Locator)
	if err != nil {
		artifact := unknownRootArtifact("native-controller", rootTarget.Locator, "", err)
		return []storage.Artifact{artifact}, []string{fmt.Sprintf("native-controller root is unreadable: %v", err)}
	}

	currentKey, keyWarning := normalizeCurrentRevision(options.CurrentRevision)
	var currentExecutable storage.Target
	var warnings []string
	if keyWarning != "" {
		warnings = append(warnings, keyWarning)
	}
	if options.CurrentExecutable != "" {
		currentExecutable, err = storage.SnapshotFilesystemTarget(options.CurrentExecutable)
		if err != nil || currentExecutable.Kind != storage.TargetFile {
			warnings = append(warnings, fmt.Sprintf("current native-controller executable is not an exact regular file: %v", err))
			currentExecutable = storage.Target{}
		}
	}

	var revisions []nativeRevision
	var artifacts []storage.Artifact
	stable, stableNames, stableCurrentMatch, stableFound, stableErr := inspectStableNativeController(rootTarget.Locator, currentExecutable)
	if stableFound && stableErr == nil {
		artifacts = append(artifacts, stable)
	} else if stableFound {
		warnings = append(warnings, fmt.Sprintf("stable native-controller files remain ownership-unknown: %v", stableErr))
	}
	for _, entry := range entries {
		if stableErr == nil && stableNames[entry.Name()] {
			continue
		}
		path := filepath.Join(rootTarget.Locator, entry.Name())
		info, infoErr := entry.Info()
		if infoErr != nil || isRedirect(info) {
			artifact := unknownEntryArtifact("native-controller", path, "", entry.IsDir(), storage.ArtifactOther, infoErr)
			artifacts = append(artifacts, artifact)
			warnings = append(warnings, fmt.Sprintf("native-controller entry %q is unsafe or unreadable", entry.Name()))
			continue
		}
		if !entry.IsDir() || !cacheKeyPattern.MatchString(entry.Name()) {
			artifact := unknownEntryArtifact("native-controller", path, "", entry.IsDir(), storage.ArtifactOther, nil)
			artifacts = append(artifacts, artifact)
			continue
		}
		revision, revisionErr := inspectNativeRevision(path, entry.Name())
		if revisionErr != nil {
			artifact := unknownEntryArtifact("native-controller-revision", path, "", true, storage.ArtifactNativeControllerRevision, revisionErr)
			if currentKey == entry.Name() {
				artifact.Current = true
				artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionCurrent, Detail: "configured current revision has uncertain ownership"})
			}
			artifacts = append(artifacts, artifact)
			warnings = append(warnings, fmt.Sprintf("native-controller revision %q remains ownership-unknown: %v", entry.Name(), revisionErr))
			continue
		}
		revisions = append(revisions, revision)
	}

	var currentIndexes []int
	for index, revision := range revisions {
		matchesKey := currentKey != "" && revision.key == currentKey
		matchesExecutable := currentExecutable.Identity != "" && revision.binary.Identity == currentExecutable.Identity && revision.binary.Fingerprint == currentExecutable.Fingerprint
		if matchesKey || matchesExecutable {
			currentIndexes = append(currentIndexes, index)
		}
	}
	if len(currentIndexes) > 1 {
		warnings = append(warnings, "native-controller current identity is ambiguous; all recognized revisions are protected")
		for index := range revisions {
			revisions[index].artifact.Protections = append(revisions[index].artifact.Protections, storage.Protection{Kind: storage.ProtectionUncertain, Detail: "ambiguous current revision"})
		}
	} else if len(currentIndexes) == 1 {
		currentIndex := currentIndexes[0]
		current := revisions[currentIndex]
		revisions[currentIndex].artifact.Current = true
		revisions[currentIndex].artifact.Protections = append(revisions[currentIndex].artifact.Protections, storage.Protection{Kind: storage.ProtectionCurrent, Detail: "explicit current native-controller identity"})
		for index := range revisions {
			if index == currentIndex || !revisions[index].completed.Before(current.completed) {
				continue
			}
			supersededAt := current.completed
			revisions[index].artifact.SupersededAt = &supersededAt
		}
	} else if (currentKey != "" || currentExecutable.Identity != "") && !stableCurrentMatch {
		warnings = append(warnings, "explicit current native-controller identity did not match a recognized revision")
	}
	for _, revision := range revisions {
		artifacts = append(artifacts, revision.artifact)
	}
	return artifacts, warnings
}

func inspectStableNativeController(root string, currentExecutable storage.Target) (storage.Artifact, map[string]bool, bool, bool, error) {
	manifestPath := filepath.Join(root, "ephemeral-action-runner.manifest")
	manifestInfo, err := os.Lstat(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return storage.Artifact{}, nil, false, false, nil
	}
	if err != nil {
		return storage.Artifact{}, nil, false, true, err
	}
	if !manifestInfo.Mode().IsRegular() || isRedirect(manifestInfo) {
		return storage.Artifact{}, nil, false, true, fmt.Errorf("manifest is not an exact regular file")
	}
	fields, err := parseStableManifest(manifestPath)
	if err != nil {
		return storage.Artifact{}, nil, false, true, err
	}
	executableName := fields["executable"]
	if executableName != "ephemeral-action-runner" && executableName != "ephemeral-action-runner.exe" {
		return storage.Artifact{}, nil, false, true, fmt.Errorf("manifest executable is invalid")
	}
	binaryPath := filepath.Join(root, executableName)
	binaryInfo, err := os.Lstat(binaryPath)
	if err != nil {
		return storage.Artifact{}, nil, false, true, err
	}
	if !binaryInfo.Mode().IsRegular() || isRedirect(binaryInfo) {
		return storage.Artifact{}, nil, false, true, fmt.Errorf("controller executable is not an exact regular file")
	}
	binary, err := storage.SnapshotFilesystemTarget(binaryPath)
	if err != nil {
		return storage.Artifact{}, nil, false, true, err
	}
	manifestSHA, _, err := hashFile(manifestPath)
	if err != nil {
		return storage.Artifact{}, nil, false, true, err
	}
	completed, err := time.Parse(time.RFC3339Nano, fields["completedAtUtc"])
	if err != nil {
		return storage.Artifact{}, nil, false, true, fmt.Errorf("manifest completedAtUtc is invalid")
	}
	size := uint64(binaryInfo.Size() + manifestInfo.Size())
	artifact := storage.Artifact{
		ID:             "native-controller-stable:" + fields["fingerprint"],
		SurfaceID:      ProjectSurfaceID,
		Kind:           storage.ArtifactNativeControllerRevision,
		RetentionGroup: "native-controller",
		Target:         binary,
		Ownership: storage.Ownership{
			Kind:     storage.OwnershipExact,
			OwnerID:  "native-controller:" + fields["fingerprint"],
			Evidence: "ephemeral-action-runner.manifest@" + manifestSHA,
		},
		SizeBytes: size,
		CreatedAt: completed.UTC(),
		Current:   true,
		Protections: []storage.Protection{{
			Kind: storage.ProtectionCurrent, Detail: "stable native-controller manifest",
		}},
	}
	currentMatch := currentExecutable.Identity != "" && binary.Identity == currentExecutable.Identity && binary.Fingerprint == currentExecutable.Fingerprint
	stableNames := map[string]bool{executableName: true, filepath.Base(manifestPath): true}
	lockPath := filepath.Join(root, ".native-controller.lock")
	if lockInfo, lockErr := os.Lstat(lockPath); lockErr == nil && lockInfo.Mode().IsRegular() && !isRedirect(lockInfo) && lockInfo.Size() == 0 {
		stableNames[filepath.Base(lockPath)] = true
	}
	return artifact, stableNames, currentMatch, true, nil
}

func parseStableManifest(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fields := make(map[string]string, 6)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 16*1024)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("manifest contains an invalid field")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("manifest contains duplicate field %q", key)
		}
		fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(fields) != 6 {
		return nil, fmt.Errorf("manifest must contain exactly six fields")
	}
	for _, key := range []string{"schemaVersion", "fingerprint", "executable", "toolchainImageID", "sourceRevision", "completedAtUtc"} {
		if fields[key] == "" {
			return nil, fmt.Errorf("manifest is missing %q", key)
		}
	}
	if fields["schemaVersion"] != "2" || !cacheKeyPattern.MatchString(fields["fingerprint"]) {
		return nil, fmt.Errorf("manifest schema or fingerprint is invalid")
	}
	sourceFingerprint := strings.TrimPrefix(strings.TrimPrefix(fields["sourceRevision"], "dirty:"), "sha256:")
	sourceMatches := fields["sourceRevision"] == "unknown" || sourceFingerprint == fields["fingerprint"]
	if !sourceMatches || !strings.HasPrefix(fields["toolchainImageID"], "sha256:") || !cacheKeyPattern.MatchString(strings.TrimPrefix(fields["toolchainImageID"], "sha256:")) {
		return nil, fmt.Errorf("manifest source or toolchain identity is invalid")
	}
	return fields, nil
}

func inspectNativeRevision(path, cacheKey string) (nativeRevision, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nativeRevision{}, err
	}
	var manifestPath, executableName string
	var leaseNames []string
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nativeRevision{}, err
		}
		if entry.IsDir() || !info.Mode().IsRegular() || isRedirect(info) {
			return nativeRevision{}, fmt.Errorf("contains a directory, special file, or redirect at %q", entry.Name())
		}
		switch {
		case entry.Name() == "controller-cache.manifest":
			manifestPath = filepath.Join(path, entry.Name())
		case entry.Name() == "ephemeral-action-runner" || entry.Name() == "ephemeral-action-runner.exe":
			if executableName != "" {
				return nativeRevision{}, fmt.Errorf("contains multiple controller executables")
			}
			executableName = entry.Name()
		case leasePattern.MatchString(entry.Name()):
			leaseNames = append(leaseNames, entry.Name())
		default:
			return nativeRevision{}, fmt.Errorf("contains unexpected file %q", entry.Name())
		}
	}
	if manifestPath == "" || executableName == "" {
		return nativeRevision{}, fmt.Errorf("requires one manifest and one controller executable")
	}
	fields, err := parseManifest(manifestPath)
	if err != nil {
		return nativeRevision{}, err
	}
	if fields["schemaVersion"] != "1" || fields["cacheKey"] != cacheKey || fields["executable"] != executableName {
		return nativeRevision{}, fmt.Errorf("manifest identity does not match the revision directory")
	}
	var completed time.Time
	if value, exists := fields["completedAtUnix"]; exists {
		completedUnix, err := strconv.ParseInt(value, 10, 64)
		if err != nil || completedUnix <= 0 {
			return nativeRevision{}, fmt.Errorf("manifest completedAtUnix is invalid")
		}
		completed = time.Unix(completedUnix, 0).UTC()
	} else {
		completed, err = time.Parse(time.RFC3339Nano, fields["completedAtUtc"])
		if err != nil {
			return nativeRevision{}, fmt.Errorf("manifest completedAtUtc is invalid")
		}
		completed = completed.UTC()
	}
	target, err := storage.SnapshotFilesystemTarget(path)
	if err != nil {
		return nativeRevision{}, err
	}
	binary, err := storage.SnapshotFilesystemTarget(filepath.Join(path, executableName))
	if err != nil {
		return nativeRevision{}, err
	}
	manifestSHA, _, err := hashFile(manifestPath)
	if err != nil {
		return nativeRevision{}, err
	}
	size, err := directoryBytes(path)
	if err != nil {
		return nativeRevision{}, err
	}
	artifact := storage.Artifact{
		ID:             "native-controller:" + cacheKey,
		SurfaceID:      ProjectSurfaceID,
		Kind:           storage.ArtifactNativeControllerRevision,
		RetentionGroup: "native-controller",
		Target:         target,
		Ownership: storage.Ownership{
			Kind:     storage.OwnershipExact,
			OwnerID:  "native-controller:" + cacheKey,
			Evidence: "controller-cache.manifest@" + manifestSHA,
		},
		SizeBytes: size,
		CreatedAt: completed,
	}
	if len(leaseNames) > 0 {
		artifact.Protections = append(artifact.Protections, storage.Protection{Kind: storage.ProtectionLease, Detail: "revision contains one or more unexpired-status-unknown leases"})
	}
	return nativeRevision{artifact: artifact, key: cacheKey, completed: completed, binary: binary}, nil
}

func parseManifest(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fields := make(map[string]string, 4)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 16*1024)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("manifest contains an invalid field")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("manifest contains duplicate field %q", key)
		}
		fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(fields) != 4 {
		return nil, fmt.Errorf("manifest must contain exactly four fields")
	}
	for _, key := range []string{"schemaVersion", "cacheKey", "executable"} {
		if _, exists := fields[key]; !exists {
			return nil, fmt.Errorf("manifest is missing %q", key)
		}
	}
	_, hasUnix := fields["completedAtUnix"]
	_, hasUTC := fields["completedAtUtc"]
	if hasUnix == hasUTC {
		return nil, fmt.Errorf("manifest must contain exactly one completion timestamp")
	}
	return fields, nil
}

func normalizeCurrentRevision(value string) (string, string) {
	if value == "" {
		return "", ""
	}
	value = strings.TrimPrefix(value, "dirty:")
	value = strings.TrimPrefix(value, "sha256:")
	if !cacheKeyPattern.MatchString(value) {
		return "", fmt.Sprintf("current native-controller revision %q is not an exact 64-character SHA-256 identity", value)
	}
	return value, ""
}

func unknownEntryArtifact(prefix, path, provider string, directory bool, kind storage.ArtifactKind, cause error) storage.Artifact {
	target, err := storage.SnapshotFilesystemTarget(path)
	if err != nil {
		target = unknownTarget(path, directory)
	}
	var size uint64
	if target.Match == storage.MatchExact {
		if directory {
			size, _ = directoryBytes(path)
		} else if info, statErr := os.Lstat(path); statErr == nil && info.Size() >= 0 {
			size = uint64(info.Size())
		}
	}
	detail := "entry does not satisfy an exact EPAR ownership schema"
	if cause != nil {
		detail = cause.Error()
	}
	return storage.Artifact{
		ID:        stableID(prefix+"-unknown", path),
		Provider:  provider,
		SurfaceID: ProjectSurfaceID,
		Kind:      kind,
		Target:    target,
		Ownership: storage.Ownership{Kind: storage.OwnershipUnknown},
		SizeBytes: size,
		Protections: []storage.Protection{{
			Kind:   storage.ProtectionUncertain,
			Detail: detail,
		}},
	}
}
