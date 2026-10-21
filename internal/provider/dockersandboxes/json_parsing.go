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
	sandboxcli "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/cli"
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
	// Diagnostics contain only fixed categories and a size, never provider data
	// or decoder errors (which can include credentials from malformed output).
	invalid := func(category string) error {
		return fmt.Errorf("docker sandbox template inventory parse failure: category=%s stdoutBytes=%d", category, len(data))
	}
	if !json.Valid(data) {
		return nil, invalid("invalid_json")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, invalid("duplicate_json_key")
	}
	// v0.48+ returns an array; earlier releases wrap it in {"images":...}.
	rawImages := bytes.TrimSpace(data)
	if rawImages[0] != '[' {
		var wrapper map[string]json.RawMessage
		if err := decodeStrictJSON(data, &wrapper); err != nil || wrapper == nil {
			return nil, invalid("invalid_root_type")
		}
		var ok bool
		rawImages, ok = wrapper["images"]
		if !ok {
			return nil, invalid("missing_images")
		}
	}
	if bytes.Equal(bytes.TrimSpace(rawImages), []byte("null")) || bytes.TrimSpace(rawImages)[0] != '[' {
		return nil, invalid("invalid_images_type")
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(rawImages, &records); err != nil {
		return nil, invalid("invalid_image_schema")
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
			return nil, invalid("invalid_image_schema")
		}
		if !templatePattern.MatchString(repository) || !profilePattern.MatchString(tag) || flavor != "" && !profilePattern.MatchString(flavor) {
			return nil, invalid("invalid_image_identity")
		}
		reference := repository + ":" + tag
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, invalid("duplicate_image_id")
		}
		if _, duplicate := seenReferences[reference]; duplicate {
			return nil, invalid("duplicate_image_reference")
		}
		seenIDs[id] = struct{}{}
		seenReferences[reference] = struct{}{}
		images = append(images, cachedTemplate{ID: id, Repository: repository, Tag: tag, Flavor: flavor, CreatedAt: createdAt, Size: sizeValue})
	}
	return images, nil
}

func parseInventory(data []byte) ([]provider.InventoryItem, error) {
	// sbx does not expose a host-wide generation or count alongside this API.
	// Recovery therefore treats one validated `sbx ls --json` response as the
	// complete authoritative snapshot and rejects duplicate keys at every depth
	// before map decoding could collapse contradictory evidence.
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("docker sandboxes inventory returned an unsupported json schema")
	}
	// Both documented envelopes carry the same identity/workspace contract.
	rawSandboxes := bytes.TrimSpace(data)
	if rawSandboxes[0] != '[' {
		var wrapper map[string]json.RawMessage
		if err := decodeStrictJSON(data, &wrapper); err != nil {
			return nil, fmt.Errorf("docker sandboxes inventory returned an unsupported json schema")
		}
		rawSandboxes = wrapper["sandboxes"]
	}
	var records []map[string]json.RawMessage
	if bytes.Equal(bytes.TrimSpace(rawSandboxes), []byte("null")) || json.Unmarshal(rawSandboxes, &records) != nil {
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
		workspaces, err := inventoryWorkspaces(record)
		if err != nil {
			return nil, fmt.Errorf("docker sandboxes inventory contained invalid workspaces")
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

func rejectDuplicateJSONKeys(data []byte) error {
	return sandboxcli.RejectDuplicateJSONKeys(data)
}

func inventoryWorkspaces(record map[string]json.RawMessage) ([]string, error) {
	raw, ok := record["workspaces"]
	if !ok {
		// sbx omits this field for a sandbox created without workspace mounts.
		// Normalize that documented state to an explicit, known-empty list;
		// EPAR-owned instances still require their exact staging workspace.
		return []string{}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
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
	report, err := sandboxcli.ParseDiagnostics(data)
	return report.Passed, report.Warned, report.Failed, report.Skipped, err
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

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("unexpected trailing json")
	} else if err != io.EOF {
		return err
	}
	return nil
}
