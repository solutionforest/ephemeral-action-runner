package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

type nativeBootstrapAcquisition struct {
	SchemaVersion      int    `json:"schemaVersion"`
	ProjectRoot        string `json:"projectRoot"`
	Phase              string `json:"phase"`
	GoImage            string `json:"goImage"`
	DevImage           string `json:"devImage"`
	PreviousGoImageID  string `json:"previousGoImageID"`
	ResolvedGoImageID  string `json:"resolvedGoImageID"`
	PreviousDevImageID string `json:"previousDevImageID,omitempty"`
	ResolvedDevImageID string `json:"resolvedDevImageID"`
	UpdatedAtUTC       string `json:"updatedAtUtc,omitempty"`
	UpdatedAtUnix      int64  `json:"updatedAtUnix,omitempty"`
}

func importNativeBootstrapAcquisition(projectRoot, configPath string, now time.Time) error {
	path := filepath.Join(projectRoot, ".local", "storage", "bootstrap", "native-controller-acquisition.json")
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read native-controller acquisition journal: %w", err)
	}
	var record nativeBootstrapAcquisition
	if err := json.Unmarshal(content, &record); err != nil {
		return fmt.Errorf("decode native-controller acquisition journal: %w", err)
	}
	if record.SchemaVersion != 1 || record.Phase != "toolchain-built" || record.ResolvedDevImageID == "" {
		return fmt.Errorf("native-controller acquisition journal is incomplete at phase %q", record.Phase)
	}
	backendID, err := currentDockerBackendID()
	if err != nil {
		return fmt.Errorf("identify Docker backend for native-controller acquisition journal: %w", err)
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		return err
	}
	lockContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backendLock, err := store.AcquireBackendLock(lockContext, backendID)
	if err != nil {
		return err
	}
	defer backendLock.Close()
	_, err = store.WithLock(now, func(value *storagecatalog.Catalog) error {
		configRecord, err := storagecatalog.RegisterConfig(value, projectRoot, configPath, now)
		if err != nil {
			return err
		}
		devTag := normalizeDockerTag(record.DevImage)
		dev := storagecatalog.Resource{
			BackendID: backendID, InstallationIDs: []string{configRecord.InstallationID}, Kind: "docker-image", Role: "native-toolchain", Locator: devTag,
			Identity: record.ResolvedDevImageID, Custody: storagecatalog.CustodyGenerated, State: storagecatalog.StateCurrent,
			IntroducedTags: []string{devTag}, CreatedAt: now, LastSeenAt: now,
		}
		dev.Key = storagecatalog.ResourceKey(dev.BackendID, dev.Kind, dev.Identity)
		var existingReferences []storagecatalog.Reference
		for _, resource := range value.Resources {
			if resource.Key == dev.Key {
				existingReferences = append(existingReferences, resource.References...)
				dev.CreatedAt = resource.CreatedAt
				dev.InstallationIDs = mergeUniqueStrings(dev.InstallationIDs, resource.InstallationIDs)
			}
		}
		dev.References = existingReferences
		if err := storagecatalog.UpsertResource(value, dev); err != nil {
			return err
		}
		storagecatalog.ReplaceConfigRoleReferences(value, configRecord.ID, "native-toolchain", map[string]storagecatalog.Reference{
			dev.Key: {},
		}, now)
		if record.PreviousDevImageID != "" && record.PreviousDevImageID != record.ResolvedDevImageID {
			old := storagecatalog.Resource{
				BackendID: backendID, InstallationIDs: []string{configRecord.InstallationID}, Kind: "docker-image", Role: "native-toolchain", Locator: record.PreviousDevImageID,
				Identity: record.PreviousDevImageID, Custody: storagecatalog.CustodyGenerated, State: storagecatalog.StateSuperseded,
				CreatedAt: now, LastSeenAt: now,
			}
			when := now.UTC()
			old.SupersededAt = &when
			if err := storagecatalog.UpsertResource(value, old); err != nil {
				return err
			}
		}
		if record.ResolvedGoImageID != "" && record.ResolvedGoImageID != record.PreviousGoImageID {
			source := storagecatalog.Resource{
				BackendID: backendID, InstallationIDs: []string{configRecord.InstallationID}, Kind: "docker-image", Role: "native-toolchain-source",
				Locator: normalizeDockerTag(record.GoImage), Identity: record.ResolvedGoImageID,
				Custody: storagecatalog.CustodyAcquired, State: storagecatalog.StateSuperseded,
				CreatedAt: now, LastSeenAt: now,
			}
			if record.PreviousGoImageID == "" {
				source.IntroducedTags = []string{normalizeDockerTag(record.GoImage)}
			}
			when := now.UTC()
			source.SupersededAt = &when
			if err := storagecatalog.UpsertResource(value, source); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("retire imported native-controller acquisition journal: %w", err)
	}
	return nil
}

func mergeUniqueStrings(values ...[]string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, group := range values {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func normalizeDockerTag(reference string) string {
	reference = strings.TrimSpace(reference)
	lastSlash := strings.LastIndex(reference, "/")
	if strings.LastIndex(reference, ":") <= lastSlash {
		return reference + ":latest"
	}
	return reference
}

func currentDockerBackendID() (string, error) {
	output, err := exec.Command("docker", "info", "--format", "{{.ID}}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker info failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	id := strings.TrimSpace(string(output))
	if id == "" {
		return "", errors.New("Docker Engine returned an empty daemon identity")
	}
	return "docker:" + id, nil
}
