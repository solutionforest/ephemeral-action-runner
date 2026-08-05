package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const (
	maximumTemplateMetadataBytes = 4 << 20
	templateMetadataSchema       = 4
)

var (
	templateDigestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	templateCacheIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
	templateTagPattern     = regexp.MustCompile(`^(?:docker\.io/library/)?epar-docker-sandboxes-[a-z0-9._-]+:[a-z0-9._-]+$`)
	templateProfilePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	templateArchivePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.tar$`)
)

type templateOptions struct {
	Root        string
	Selections  []TemplateSelection
	Protections []TemplateProtection
}

type templateMetadata struct {
	SchemaVersion int    `json:"schemaVersion"`
	Profile       string `json:"profile"`
	Platform      string `json:"platform"`
	Template      struct {
		Tag           string `json:"tag"`
		Digest        string `json:"digest"`
		CacheID       string `json:"cacheID"`
		RootDisk      string `json:"rootDisk"`
		Archive       string `json:"archive"`
		ArchiveSHA256 string `json:"archiveSha256"`
		ArchiveBytes  uint64 `json:"archiveBytes"`
	} `json:"template"`
	Compatibility struct {
		TemplateSchemaVersion     int    `json:"templateSchemaVersion"`
		RunnerExecution           string `json:"runnerExecution"`
		DockerDaemonOwner         string `json:"dockerDaemonOwner"`
		ExpectedDockerDaemonCount int    `json:"expectedDockerDaemonCount"`
	} `json:"compatibility"`
}

type templateRecord struct {
	artifact       storage.Artifact
	metadata       templateMetadata
	metadataSHA256 string
}

func collectTemplates(options templateOptions) ([]storage.Artifact, []string, error) {
	if err := validateTemplateInputs(options); err != nil {
		return nil, nil, err
	}
	rootTarget, exists, err := inspectOptionalRoot(options.Root)
	if !exists {
		if err == nil {
			return nil, nil, nil
		}
		artifact := unknownRootArtifact("template-archive", options.Root, ProviderDockerSandboxes, err)
		return []storage.Artifact{artifact}, []string{fmt.Sprintf("Docker Sandboxes template root was not inventoried safely: %v", err)}, nil
	}
	entries, err := os.ReadDir(rootTarget.Locator)
	if err != nil {
		artifact := unknownRootArtifact("template-archive", rootTarget.Locator, ProviderDockerSandboxes, err)
		return []storage.Artifact{artifact}, []string{fmt.Sprintf("Docker Sandboxes template root is unreadable: %v", err)}, nil
	}
	protections := make(map[string][]storage.Protection)
	for _, protection := range options.Protections {
		protections[protection.ArchiveSHA256] = append(protections[protection.ArchiveSHA256], storage.Protection{Kind: protection.Kind, Detail: protection.Detail})
	}
	var records []templateRecord
	var artifacts []storage.Artifact
	var warnings []string
	for _, entry := range entries {
		path := filepath.Join(rootTarget.Locator, entry.Name())
		info, infoErr := entry.Info()
		if infoErr != nil || isRedirect(info) || !entry.IsDir() {
			artifact := unknownEntryArtifact("template-archive", path, ProviderDockerSandboxes, entry.IsDir(), storage.ArtifactOther, infoErr)
			artifacts = append(artifacts, artifact)
			warnings = append(warnings, fmt.Sprintf("Docker Sandboxes template entry %q is not a safe artifact directory", entry.Name()))
			continue
		}
		children, childrenErr := os.ReadDir(path)
		if childrenErr == nil && len(children) == 0 {
			continue
		}
		record, unknown, inspectErr := inspectTemplateDirectory(path)
		if inspectErr != nil {
			artifacts = append(artifacts, unknown)
			warnings = append(warnings, fmt.Sprintf("Docker Sandboxes template directory %q remains ownership-unknown: %v", entry.Name(), inspectErr))
			continue
		}
		record.artifact.Protections = append(record.artifact.Protections, protections[record.metadata.Template.ArchiveSHA256]...)
		records = append(records, record)
	}
	for index := range records {
		sortTemplateProtections(records[index].artifact.Protections)
		record := records[index]
		artifacts = append(artifacts, record.artifact)
	}
	return artifacts, warnings, nil
}

func inspectTemplateDirectory(path string) (templateRecord, storage.Artifact, error) {
	metadataPath := filepath.Join(path, "template-metadata.json")
	metadataTarget, err := storage.SnapshotFilesystemTarget(metadataPath)
	if err != nil || metadataTarget.Kind != storage.TargetFile {
		cause := err
		if cause == nil {
			cause = fmt.Errorf("template metadata is not a regular file")
		}
		unknown := unknownEntryArtifact("template-directory", path, ProviderDockerSandboxes, true, storage.ArtifactOther, cause)
		return templateRecord{}, unknown, fmt.Errorf("template metadata is missing or unsafe: %w", cause)
	}
	data, err := readBoundedFile(metadataTarget.Locator, maximumTemplateMetadataBytes)
	if err != nil {
		unknown := unknownEntryArtifact("template-directory", path, ProviderDockerSandboxes, true, storage.ArtifactOther, err)
		return templateRecord{}, unknown, err
	}
	if err := validateUniqueJSON(data); err != nil {
		unknown := unknownEntryArtifact("template-directory", path, ProviderDockerSandboxes, true, storage.ArtifactOther, err)
		return templateRecord{}, unknown, fmt.Errorf("invalid template metadata JSON: %w", err)
	}
	var metadata templateMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		unknown := unknownEntryArtifact("template-directory", path, ProviderDockerSandboxes, true, storage.ArtifactOther, err)
		return templateRecord{}, unknown, fmt.Errorf("decode template metadata: %w", err)
	}
	if err := validateTemplateMetadata(metadata); err != nil {
		unknown := unknownEntryArtifact("template-directory", path, ProviderDockerSandboxes, true, storage.ArtifactOther, err)
		return templateRecord{}, unknown, err
	}
	metadataSHA256, _, err := hashFile(metadataTarget.Locator)
	if err != nil {
		unknown := unknownEntryArtifact("template-directory", path, ProviderDockerSandboxes, true, storage.ArtifactOther, err)
		return templateRecord{}, unknown, err
	}
	if metadataSHA256 != hashBytes(data) {
		err := fmt.Errorf("template metadata drifted while being inspected")
		unknown := unknownEntryArtifact("template-directory", path, ProviderDockerSandboxes, true, storage.ArtifactOther, err)
		return templateRecord{}, unknown, err
	}
	archivePath := filepath.Join(path, metadata.Template.Archive)
	archiveTarget, targetErr := storage.SnapshotFilesystemTarget(archivePath)
	if targetErr != nil || archiveTarget.Kind != storage.TargetFile {
		cause := targetErr
		if cause == nil {
			cause = fmt.Errorf("template archive is not a regular file")
		}
		unknown := unknownEntryArtifact("template-archive", archivePath, ProviderDockerSandboxes, false, storage.ArtifactTemplateArchive, cause)
		return templateRecord{}, unknown, fmt.Errorf("template archive is missing or unsafe: %w", cause)
	}
	actualSHA256, actualBytes, hashErr := hashFile(archiveTarget.Locator)
	info, statErr := os.Lstat(archiveTarget.Locator)
	if hashErr != nil || statErr != nil || actualSHA256 != metadata.Template.ArchiveSHA256 || actualBytes != metadata.Template.ArchiveBytes {
		cause := fmt.Errorf("archive integrity mismatch: expected sha256=%s bytes=%d, got sha256=%s bytes=%d", metadata.Template.ArchiveSHA256, metadata.Template.ArchiveBytes, actualSHA256, actualBytes)
		if hashErr != nil {
			cause = hashErr
		} else if statErr != nil {
			cause = statErr
		}
		unknown := unknownEntryArtifact("template-archive", archiveTarget.Locator, ProviderDockerSandboxes, false, storage.ArtifactTemplateArchive, cause)
		unknown.SizeBytes = actualBytes
		return templateRecord{}, unknown, cause
	}
	artifact := storage.Artifact{
		ID:             stableID("template-archive", archiveTarget.Locator+"@"+actualSHA256),
		Provider:       ProviderDockerSandboxes,
		SurfaceID:      ProjectSurfaceID,
		Kind:           storage.ArtifactTemplateArchive,
		RetentionGroup: metadata.Profile + "/" + metadata.Platform,
		Target:         archiveTarget,
		Ownership: storage.Ownership{
			Kind:     storage.OwnershipExact,
			OwnerID:  "template-archive:" + actualSHA256,
			Evidence: "template-metadata.json@" + metadataSHA256,
		},
		SizeBytes: actualBytes,
		CreatedAt: info.ModTime().UTC(),
	}
	return templateRecord{artifact: artifact, metadata: metadata, metadataSHA256: metadataSHA256}, storage.Artifact{}, nil
}

func validateTemplateMetadata(metadata templateMetadata) error {
	if metadata.SchemaVersion != templateMetadataSchema {
		return fmt.Errorf("template metadata schemaVersion must be %d", templateMetadataSchema)
	}
	if !templateProfilePattern.MatchString(metadata.Profile) {
		return fmt.Errorf("template metadata profile is invalid")
	}
	if metadata.Platform != "linux/amd64" && metadata.Platform != "linux/arm64" {
		return fmt.Errorf("template metadata platform is invalid")
	}
	if !templateTagPattern.MatchString(metadata.Template.Tag) || !templateDigestPattern.MatchString(metadata.Template.Digest) || !templateCacheIDPattern.MatchString(metadata.Template.CacheID) {
		return fmt.Errorf("template metadata contains an invalid tag, digest, or cache ID")
	}
	if metadata.Template.CacheID != strings.TrimPrefix(metadata.Template.Digest, "sha256:")[:12] {
		return fmt.Errorf("template metadata cache ID does not match image digest")
	}
	rootDisk, err := config.ParseByteSize(metadata.Template.RootDisk)
	if err != nil || rootDisk < int64(20*storage.GiB) {
		return fmt.Errorf("template metadata root disk is invalid")
	}
	if !templateArchivePattern.MatchString(metadata.Template.Archive) || filepath.Base(metadata.Template.Archive) != metadata.Template.Archive {
		return fmt.Errorf("template metadata archive is not an exact basename")
	}
	if !templateDigestPattern.MatchString(metadata.Template.ArchiveSHA256) || metadata.Template.ArchiveBytes == 0 {
		return fmt.Errorf("template metadata archive digest or size is invalid")
	}
	if metadata.Compatibility.TemplateSchemaVersion != 1 || metadata.Compatibility.RunnerExecution != "direct-actions-listener" || metadata.Compatibility.DockerDaemonOwner != "docker-sandboxes-runtime" || metadata.Compatibility.ExpectedDockerDaemonCount != 1 {
		return fmt.Errorf("template metadata compatibility does not preserve the Docker Sandboxes runner runtime contract")
	}
	return nil
}

func validateTemplateInputs(options templateOptions) error {
	seenGroups := make(map[string]struct{})
	for _, selection := range options.Selections {
		if (selection.Profile != "" && !templateProfilePattern.MatchString(selection.Profile)) || (selection.Platform != "" && selection.Platform != "linux/amd64" && selection.Platform != "linux/arm64") || !templateTagPattern.MatchString(selection.Tag) || !templateDigestPattern.MatchString(selection.TemplateDigest) {
			return fmt.Errorf("configured template selection has an invalid exact identity")
		}
		if selection.MetadataSHA256 != "" && !templateDigestPattern.MatchString(selection.MetadataSHA256) {
			return fmt.Errorf("configured template selection metadata digest is invalid")
		}
		group := normalizedTemplateTag(selection.Tag) + "@" + selection.TemplateDigest
		if _, exists := seenGroups[group]; exists {
			return fmt.Errorf("duplicate configured template selection exists for %q", group)
		}
		seenGroups[group] = struct{}{}
	}
	for _, protection := range options.Protections {
		if !templateDigestPattern.MatchString(protection.ArchiveSHA256) || protection.Kind == "" {
			return fmt.Errorf("template protection has an invalid archive digest or kind")
		}
	}
	return nil
}

func normalizedTemplateTag(value string) string {
	return strings.TrimPrefix(value, "docker.io/library/")
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return data, nil
}

func validateUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func() error
	readValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("JSON object key is not a string")
				}
				if _, exists := keys[key]; exists {
					return fmt.Errorf("JSON object contains duplicate key %q", key)
				}
				keys[key] = struct{}{}
				if err := readValue(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return fmt.Errorf("JSON object is not terminated")
			}
		case '[':
			for decoder.More() {
				if err := readValue(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("JSON array is not terminated")
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
		return nil
	}
	if err := readValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("JSON contains trailing content")
		}
		return err
	}
	return nil
}

func sortTemplateProtections(protections []storage.Protection) {
	sort.Slice(protections, func(i, j int) bool {
		if protections[i].Kind == protections[j].Kind {
			return protections[i].Detail < protections[j].Detail
		}
		return protections[i].Kind < protections[j].Kind
	})
}
