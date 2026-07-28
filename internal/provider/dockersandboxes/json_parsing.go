package dockersandboxes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

var cachedTemplateIDPattern = regexp.MustCompile(`^[a-f0-9]{12}$`)

type cachedTemplate struct {
	ID         string
	Repository string
	Tag        string
	Flavor     string
	CreatedAt  time.Time
	Size       int64
}

func parseTemplateInventory(data []byte) ([]cachedTemplate, error) {
	var wrapper map[string]json.RawMessage
	if err := decodeStrictJSON(data, &wrapper); err != nil {
		return nil, fmt.Errorf("docker sandbox template inventory returned an unsupported json schema")
	}
	rawImages, ok := wrapper["images"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawImages), []byte("null")) {
		return nil, fmt.Errorf("docker sandbox template inventory returned an unsupported json schema")
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(rawImages, &records); err != nil {
		return nil, fmt.Errorf("docker sandbox template inventory returned an unsupported json schema")
	}
	images := make([]cachedTemplate, 0, len(records))
	seenIDs := make(map[string]struct{}, len(records))
	seenReferences := make(map[string]struct{}, len(records))
	for _, record := range records {
		id, idErr := requiredJSONString(record, "id")
		repository, repositoryErr := requiredJSONString(record, "repository")
		tag, tagErr := requiredJSONString(record, "tag")
		flavor, flavorErr := optionalJSONString(record, "flavor")
		createdAtText, createdAtErr := requiredJSONString(record, "created_at")
		createdAt, timestampErr := time.Parse(time.RFC3339, createdAtText)
		var size json.Number
		rawSize, sizePresent := record["size"]
		sizeErr := json.Unmarshal(rawSize, &size)
		sizeValue, integerErr := strconv.ParseInt(size.String(), 10, 64)
		if idErr != nil || !cachedTemplateIDPattern.MatchString(id) || repositoryErr != nil || tagErr != nil || flavorErr != nil || createdAtErr != nil || timestampErr != nil || !sizePresent || len(rawSize) == 0 || rawSize[0] < '0' || rawSize[0] > '9' || sizeErr != nil || integerErr != nil || sizeValue <= 0 {
			return nil, fmt.Errorf("docker sandbox template inventory returned an unsupported image schema")
		}
		if !templatePattern.MatchString(repository) || !profilePattern.MatchString(tag) || flavor != "" && !profilePattern.MatchString(flavor) {
			return nil, fmt.Errorf("docker sandbox template inventory returned an invalid image identity")
		}
		reference := repository + ":" + tag
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, fmt.Errorf("docker sandbox template inventory returned a duplicate image id")
		}
		if _, duplicate := seenReferences[reference]; duplicate {
			return nil, fmt.Errorf("docker sandbox template inventory returned a duplicate image reference")
		}
		seenIDs[id] = struct{}{}
		seenReferences[reference] = struct{}{}
		images = append(images, cachedTemplate{ID: id, Repository: repository, Tag: tag, Flavor: flavor, CreatedAt: createdAt, Size: sizeValue})
	}
	return images, nil
}

func parseLocalTemplateImage(data []byte) (LocalTemplateImage, error) {
	var record map[string]json.RawMessage
	if err := decodeStrictJSON(data, &record); err != nil {
		return LocalTemplateImage{}, fmt.Errorf("local docker image inspection returned an unsupported json schema")
	}
	digest, digestErr := requiredJSONString(record, "Id")
	osName, osErr := requiredJSONString(record, "Os")
	architecture, architectureErr := requiredJSONString(record, "Architecture")
	if digestErr != nil || osErr != nil || architectureErr != nil || !validFullTemplateDigest(digest) {
		return LocalTemplateImage{}, fmt.Errorf("local docker image inspection returned an unsupported image schema")
	}
	platform := osName + "/" + architecture
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return LocalTemplateImage{}, fmt.Errorf("local docker image inspection returned an unsupported linux template platform")
	}
	return LocalTemplateImage{Digest: digest, Platform: platform}, nil
}

func parseInventory(data []byte) ([]provider.InventoryItem, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var wrapper map[string]json.RawMessage
	if err := decoder.Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("docker sandboxes inventory returned an unsupported json schema")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("docker sandboxes inventory returned an unsupported json schema")
	}
	var records []map[string]json.RawMessage
	rawSandboxes, ok := wrapper["sandboxes"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawSandboxes), []byte("null")) || json.Unmarshal(rawSandboxes, &records) != nil {
		return nil, fmt.Errorf("docker sandboxes inventory returned an unsupported json schema")
	}
	items := make([]provider.InventoryItem, 0, len(records))
	seenNames := make(map[string]struct{}, len(records))
	seenIDs := make(map[string]struct{}, len(records))
	for _, record := range records {
		id, err := requiredJSONString(record, "id")
		if err != nil || !providerIDPattern.MatchString(id) {
			return nil, fmt.Errorf("docker sandboxes inventory omitted a valid stable id")
		}
		name, err := requiredJSONString(record, "name")
		if err != nil || !sandboxNamePattern.MatchString(name) {
			return nil, fmt.Errorf("docker sandboxes inventory contained an invalid name")
		}
		status, err := requiredJSONString(record, "status")
		if err != nil || strings.TrimSpace(status) == "" {
			return nil, fmt.Errorf("docker sandboxes inventory omitted status")
		}
		agent, err := optionalJSONString(record, "agent")
		if err != nil {
			return nil, fmt.Errorf("docker sandboxes inventory returned an unsupported agent field")
		}
		workspaces, err := requiredStringArray(record, "workspaces")
		if err != nil || len(workspaces) == 0 {
			return nil, fmt.Errorf("docker sandboxes inventory omitted workspaces")
		}
		if _, duplicate := seenNames[name]; duplicate {
			return nil, fmt.Errorf("docker sandboxes inventory returned a duplicate name")
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, fmt.Errorf("docker sandboxes inventory returned a duplicate stable id")
		}
		seenNames[name] = struct{}{}
		seenIDs[id] = struct{}{}
		instance := provider.Instance{Name: name, ProviderID: id, Source: agent, State: status}
		items = append(items, provider.InventoryItem{Instance: instance, State: status, Source: agent, Workspaces: workspaces})
	}
	return items, nil
}

func requiredStringArray(record map[string]json.RawMessage, key string) ([]string, error) {
	raw, ok := record[key]
	if !ok {
		return nil, fmt.Errorf("missing field")
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || len(values) == 0 {
		return nil, fmt.Errorf("invalid field")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid field")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("duplicate field value")
		}
		seen[value] = struct{}{}
	}
	return values, nil
}

func parseDaemonStatus(data []byte) (state string, healthy bool, err error) {
	var record map[string]json.RawMessage
	if err := decodeStrictJSON(data, &record); err != nil {
		return "", false, fmt.Errorf("docker sandboxes daemon status returned an unsupported json schema")
	}
	raw, ok := record["status"]
	if !ok || json.Unmarshal(raw, &state) != nil || strings.TrimSpace(state) == "" {
		return "", false, fmt.Errorf("docker sandboxes daemon status returned an unsupported json schema")
	}
	normalized := strings.ToLower(state)
	return state, normalized == "running", nil
}

func parseDiagnose(data []byte) (passed, warned, failed, skipped int, err error) {
	var record map[string]json.RawMessage
	if err := decodeStrictJSON(data, &record); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unsupported json schema")
	}
	version, versionErr := requiredJSONString(record, "version")
	if versionErr != nil || version != "1.0" {
		return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unsupported schema version")
	}
	rawChecks, ok := record["checks"]
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unsupported json schema")
	}
	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(rawChecks, &checks); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unsupported json schema")
	}
	for _, check := range checks {
		if _, fieldErr := requiredJSONString(check, "name"); fieldErr != nil {
			return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unsupported check schema")
		}
		for _, field := range []string{"message", "detail", "hint"} {
			if _, fieldErr := requiredStringField(check, field); fieldErr != nil {
				return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unsupported check schema")
			}
		}
		status, statusErr := requiredJSONString(check, "status")
		if statusErr != nil {
			return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unsupported check schema")
		}
		switch strings.ToLower(status) {
		case "pass":
			passed++
		case "warn":
			warned++
		case "fail":
			failed++
		case "skip":
			skipped++
		default:
			return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics returned an unknown check status")
		}
	}
	var summary map[string]json.RawMessage
	rawSummary, ok := record["summary"]
	if !ok || json.Unmarshal(rawSummary, &summary) != nil {
		return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics summary did not match its checks")
	}
	summaryCounts := make([]int, 4)
	for index, key := range []string{"pass", "warn", "fail", "skip"} {
		rawCount, present := summary[key]
		if !present || json.Unmarshal(rawCount, &summaryCounts[index]) != nil || summaryCounts[index] < 0 {
			return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics summary did not match its checks")
		}
	}
	if summaryCounts[0] != passed || summaryCounts[1] != warned || summaryCounts[2] != failed || summaryCounts[3] != skipped {
		return 0, 0, 0, 0, fmt.Errorf("docker sandboxes diagnostics summary did not match its checks")
	}
	return passed, warned, failed, skipped, nil
}

func requiredJSONString(record map[string]json.RawMessage, key string) (string, error) {
	raw, ok := record[key]
	if !ok {
		return "", fmt.Errorf("missing field")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("invalid field")
	}
	return value, nil
}

func optionalJSONString(record map[string]json.RawMessage, key string) (string, error) {
	raw, ok := record[key]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func requiredStringField(record map[string]json.RawMessage, key string) (string, error) {
	raw, ok := record[key]
	if !ok {
		return "", fmt.Errorf("missing field")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("invalid field")
	}
	return value, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("unexpected trailing json")
	} else if err != io.EOF {
		return err
	}
	return nil
}
