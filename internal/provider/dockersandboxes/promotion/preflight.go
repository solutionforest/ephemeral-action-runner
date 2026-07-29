package promotion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	sandboxcapacity "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/capacity"
	sandboxpolicy "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/policy"
)

const (
	DisableEnvironment   = "EPAR_DISABLE_DOCKER_SANDBOXES"
	preflightOutputLimit = 256 << 10
)

var (
	sandboxScopePattern = regexp.MustCompile(`^sandbox:[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Failure struct {
	Gate       string
	Detail     string
	Resolution string
}

type PreflightResult struct {
	Failures []Failure
}

type HostSpace = sandboxcapacity.HostSpace

func (result PreflightResult) Passed() bool {
	return len(result.Failures) == 0
}

type PreflightOptions struct {
	ProjectRoot         string
	StorageRoot         string
	NativeController    bool
	ControllerRevision  string
	RunSBX              func(context.Context, []string) ([]byte, error)
	InspectTemplate     func(context.Context, string) (string, error)
	HostSpace           func(string) (HostSpace, error)
	CheckVirtualization func() error
}

func LocalPreflight(ctx context.Context, record Record, projectRoot string, nativeController bool, controllerRevision string) PreflightResult {
	if record.Platform != CurrentPlatform() {
		return PreflightResult{Failures: []Failure{{
			Gate:       "promoted platform",
			Detail:     fmt.Sprintf("promotion record targets %s, but the native controller is running on %s", record.Platform, CurrentPlatform()),
			Resolution: "Explicitly choose another provider; never reuse a Docker Sandboxes promotion record across platforms.",
		}}}
	}
	if os.Getenv(DisableEnvironment) == "1" {
		return PreflightResult{Failures: []Failure{{
			Gate:       "operator kill switch",
			Detail:     DisableEnvironment + "=1 disables Docker Sandboxes admission and automatic selection",
			Resolution: "Unset the kill switch only after the Docker Sandboxes issue is resolved, or explicitly choose another provider.",
		}}}
	}
	storageRoot, err := sandboxcapacity.DockerSandboxesStorageRoot()
	if err != nil {
		return PreflightResult{Failures: []Failure{{
			Gate:       "resource availability",
			Detail:     fmt.Sprintf("cannot locate Docker Sandboxes provider storage: %v", err),
			Resolution: "Use another explicitly selected provider until Docker Sandboxes storage can be located for capacity admission.",
		}}}
	}
	return RunPreflight(ctx, record, PreflightOptions{
		ProjectRoot:         projectRoot,
		StorageRoot:         storageRoot,
		NativeController:    nativeController,
		ControllerRevision:  controllerRevision,
		RunSBX:              runSBXCommand,
		InspectTemplate:     inspectLocalTemplateImage,
		HostSpace:           sandboxHostSpace,
		CheckVirtualization: sandboxVirtualizationAvailable,
	})
}

func RunPreflight(ctx context.Context, record Record, opts PreflightOptions) PreflightResult {
	var result PreflightResult
	add := func(gate, detail, resolution string) {
		result.Failures = append(result.Failures, Failure{Gate: gate, Detail: detail, Resolution: resolution})
	}
	if err := Validate(record); err != nil {
		add("promotion record", err.Error(), "Use Docker Container or another explicitly selected provider until a valid platform promotion record is embedded.")
		return result
	}
	if !validSHA256(opts.ControllerRevision) {
		add("controller revision", "the running native controller does not report an exact clean source/build sha256 identity", "Rebuild the native controller from the exact clean promoted source tree with scripts/build-native-controller, then rerun setup.")
	} else if opts.ControllerRevision != record.EPARRevision {
		add("controller revision", fmt.Sprintf("the running native controller identity is %s, but the promotion record requires %s", opts.ControllerRevision, record.EPARRevision), "Rebuild and run the exact promoted native controller revision, or use another explicitly selected provider.")
	}
	if !opts.NativeController {
		add("native controller", "EPAR is running inside the legacy controller container, where Docker Sandboxes host state is unavailable.", "Run EPAR through ./start or scripts/build-native-controller so the controller executes on the host.")
	}
	if opts.CheckVirtualization == nil {
		add("virtualization", "the native virtualization check is unavailable", "Use Docker Container or another provider until EPAR can verify host virtualization.")
	} else if err := opts.CheckVirtualization(); err != nil {
		add("virtualization", err.Error(), "Enable the platform virtualization facility and ensure the current host user can access it, then rerun setup.")
	}
	if opts.StorageRoot == "" {
		add("resource availability", "the Docker Sandboxes provider-storage path is unavailable", "Use another provider until EPAR can locate the actual Docker Sandboxes storage volume.")
	} else if opts.HostSpace == nil {
		add("resource availability", "the host free-space check is unavailable", "Use Docker Container or another provider until EPAR can verify host capacity.")
	} else {
		space, err := opts.HostSpace(opts.StorageRoot)
		required, overflow := requiredHostFreeBytes(record, space.TotalBytes)
		switch {
		case err != nil:
			add("resource availability", fmt.Sprintf("cannot read free space for Docker Sandboxes provider storage %s: %v", opts.StorageRoot, err), "Make the Docker Sandboxes storage filesystem available to the native controller, then rerun setup.")
		case overflow:
			add("resource availability", "the promoted disk reservation overflows the supported byte range", "Use Docker Container and report the invalid promotion record.")
		case space.AvailableBytes < required:
			add("resource availability", fmt.Sprintf("Docker Sandboxes storage free space is %d bytes; the configured fixed reserve requires at least %d bytes on the %d-byte provider-storage volume", space.AvailableBytes, required, space.TotalBytes), "Free space on the Docker Sandboxes provider-storage volume, then rerun setup.")
		}
	}
	if opts.RunSBX == nil {
		add("sbx command", "the Docker Sandboxes command runner is unavailable", "Install sbx on the native host, then run sbx diagnose --output json and review the hints for any failed checks.")
		return result
	}

	daemonOutput, daemonErr := opts.RunSBX(ctx, []string{"daemon", "status", "--json"})
	if daemonErr != nil {
		add("daemon health", daemonErr.Error(), "Start or repair the Docker Sandboxes daemon, confirm sbx daemon status --json reports running, then rerun setup.")
	} else if err := verifyDaemonRunning(daemonOutput); err != nil {
		add("daemon health", err.Error(), "Start or repair the Docker Sandboxes daemon, confirm sbx daemon status --json reports running, then rerun setup.")
	}

	diagnoseOutput, diagnoseErr := opts.RunSBX(ctx, []string{"diagnose", "--output", "json"})
	if diagnoseErr != nil {
		add("daemon diagnostics", diagnoseErr.Error(), "Run sbx diagnose --output json and review the hints for any failed checks, then rerun setup.")
	} else {
		checks, err := parseDiagnostics(diagnoseOutput)
		if err != nil {
			add("daemon diagnostics", err.Error(), "Run sbx diagnose --output json and review the hints for any failed checks, then rerun setup.")
		} else {
			passed, failed := diagnosticPassAndFailureCounts(checks)
			if failed != 0 {
				add("daemon diagnostics", fmt.Sprintf("diagnostics reported %d failed check(s)", failed), "Run sbx diagnose --output json and review the hints for each failed check, then rerun setup.")
			} else if passed == 0 {
				add("daemon diagnostics", "diagnostics reported no passing checks", "Run sbx diagnose --output json and review its check details, then rerun setup.")
			}
		}
	}

	templateOutput, templateErr := opts.RunSBX(ctx, []string{"template", "ls", "--json"})
	if templateErr != nil {
		add("promoted template", templateErr.Error(), fmt.Sprintf("Build and load the exact promoted template %s, then rerun setup.", record.Template))
	} else if err := verifyPromotedTemplate(templateOutput, record.Template, record.TemplateCacheID); err != nil {
		add("promoted template", err.Error(), fmt.Sprintf("Build and load the exact promoted template %s with cache ID %s, then rerun setup.", record.Template, record.TemplateCacheID))
	}
	if opts.InspectTemplate == nil {
		add("promoted template evidence", "the independent local Docker image identity reader is unavailable", "Keep the full promoted template image in the local Docker image store, then rerun setup.")
	} else {
		fullIdentity, inspectErr := opts.InspectTemplate(ctx, record.Template)
		fullIdentity = strings.TrimSpace(fullIdentity)
		switch {
		case inspectErr != nil:
			add("promoted template evidence", inspectErr.Error(), "Restore the exact locally built and hash-anchored promoted template image, then rerun setup.")
		case !validSHA256(fullIdentity):
			add("promoted template evidence", "local Docker image inspection did not return a full lowercase sha256 identity", "Restore the exact locally built and hash-anchored promoted template image, then rerun setup.")
		case fullIdentity != record.TemplateDigest:
			add("promoted template evidence", fmt.Sprintf("full local Docker image identity is %s, want %s", fullIdentity, record.TemplateDigest), "Restore the exact locally built and hash-anchored promoted template image, then rerun setup.")
		}
	}

	policyOutput, policyErr := opts.RunSBX(ctx, []string{"policy", "ls", "--include-inactive", "--json"})
	if policyErr != nil {
		add("promoted policy", policyErr.Error(), "Restore the exact promoted Docker Sandboxes global policy and rerun setup.")
	} else if err := verifyPromotedPolicy(policyOutput, record.PolicyFingerprint); err != nil {
		add("promoted policy", err.Error(), "Restore the exact promoted Docker Sandboxes global policy and rerun setup.")
	}
	return result
}

func requiredHostFreeBytes(record Record, backingVolumeSize uint64) (uint64, bool) {
	watermark, err := sandboxcapacity.HostWatermark(record.MinHostFreeSpaceBytes, backingVolumeSize)
	if err != nil {
		return 0, true
	}
	return watermark, false
}

func runSBXCommand(ctx context.Context, args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("refusing to invoke sbx without a subcommand")
	}
	if args[0] == "tui" || args[0] == "reset" {
		return nil, fmt.Errorf("refusing to invoke forbidden sbx subcommand %q", args[0])
	}
	command := exec.CommandContext(ctx, "sbx", args...)
	command.Env = sandboxCommandEnvironment()
	stdout := &preflightBuffer{limit: preflightOutputLimit}
	stderr := &preflightBuffer{limit: preflightOutputLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.overflow || stderr.overflow {
		err = errors.Join(err, errors.New("sbx preflight output limit exceeded"))
	}
	if err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return nil, fmt.Errorf("sbx %s failed: %w: %s", args[0], err, text)
		}
		return nil, fmt.Errorf("sbx %s failed: %w", args[0], err)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func inspectLocalTemplateImage(ctx context.Context, reference string) (string, error) {
	command := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", reference)
	command.Env = sandboxCommandEnvironment()
	stdout := &preflightBuffer{limit: 1024}
	stderr := &preflightBuffer{limit: 4096}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.overflow || stderr.overflow {
		err = errors.Join(err, errors.New("Docker image inspection output limit exceeded"))
	}
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("docker image inspect failed: %w: %s", err, detail)
		}
		return "", fmt.Errorf("docker image inspect failed: %w", err)
	}
	return stdout.String(), nil
}

func sandboxCommandEnvironment() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(strings.ToUpper(key), "DOCKER_SANDBOXES_") {
			continue
		}
		environment = append(environment, item)
	}
	return environment
}

type preflightBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *preflightBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = buffer.Buffer.Write(data[:remaining])
		buffer.overflow = true
		return len(data), nil
	}
	return buffer.Buffer.Write(data)
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("unexpected trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func verifyDaemonRunning(data []byte) error {
	var record map[string]json.RawMessage
	if err := decodeStrictJSON(data, &record); err != nil {
		return errors.New("Docker Sandboxes daemon status returned unsupported JSON")
	}
	var status string
	if raw, ok := record["status"]; !ok || json.Unmarshal(raw, &status) != nil || !strings.EqualFold(strings.TrimSpace(status), "running") {
		return fmt.Errorf("Docker Sandboxes daemon status is not running")
	}
	return nil
}

type diagnosticCheck struct {
	Name   string
	Status string
}

func parseDiagnostics(data []byte) ([]diagnosticCheck, error) {
	var record map[string]json.RawMessage
	if err := decodeStrictJSON(data, &record); err != nil {
		return nil, errors.New("Docker Sandboxes diagnostics returned unsupported JSON")
	}
	var version string
	if raw, ok := record["version"]; !ok || json.Unmarshal(raw, &version) != nil || version != "1.0" {
		return nil, errors.New("Docker Sandboxes diagnostics returned an unsupported schema version")
	}
	var rawChecks []map[string]json.RawMessage
	if raw, ok := record["checks"]; !ok || json.Unmarshal(raw, &rawChecks) != nil || len(rawChecks) == 0 {
		return nil, errors.New("Docker Sandboxes diagnostics omitted its checks")
	}
	checks := make([]diagnosticCheck, 0, len(rawChecks))
	counts := map[string]int{"pass": 0, "warn": 0, "fail": 0, "skip": 0}
	for _, rawCheck := range rawChecks {
		name, err := requiredString(rawCheck, "name", false)
		if err != nil {
			return nil, errors.New("Docker Sandboxes diagnostics returned an unsupported check schema")
		}
		status, err := requiredString(rawCheck, "status", false)
		if err != nil {
			return nil, errors.New("Docker Sandboxes diagnostics returned an unsupported check schema")
		}
		for _, field := range []string{"message", "detail", "hint"} {
			if _, err := requiredString(rawCheck, field, true); err != nil {
				return nil, errors.New("Docker Sandboxes diagnostics returned an unsupported check schema")
			}
		}
		status = strings.ToLower(status)
		if _, ok := counts[status]; !ok {
			return nil, errors.New("Docker Sandboxes diagnostics returned an unknown check status")
		}
		counts[status]++
		checks = append(checks, diagnosticCheck{Name: name, Status: status})
	}
	var summary map[string]json.RawMessage
	if raw, ok := record["summary"]; !ok || json.Unmarshal(raw, &summary) != nil {
		return nil, errors.New("Docker Sandboxes diagnostic summary is missing")
	}
	for _, key := range []string{"pass", "warn", "fail", "skip"} {
		var count int
		if raw, ok := summary[key]; !ok || json.Unmarshal(raw, &count) != nil || count != counts[key] {
			return nil, errors.New("Docker Sandboxes diagnostic summary did not match its checks")
		}
	}
	return checks, nil
}

func diagnosticPassAndFailureCounts(checks []diagnosticCheck) (passed, failed int) {
	for _, check := range checks {
		switch check.Status {
		case "pass":
			passed++
		case "fail":
			failed++
		}
	}
	return passed, failed
}

func verifyPromotedTemplate(data []byte, reference, cacheID string) error {
	repository, tag, err := splitTemplateReference(reference)
	if err != nil {
		return err
	}
	if !validTemplateCacheID(cacheID) {
		return errors.New("promoted Docker Sandboxes template cache ID is invalid")
	}
	var wrapper map[string]json.RawMessage
	if err := decodeStrictJSON(data, &wrapper); err != nil {
		return errors.New("Docker Sandboxes template inventory returned unsupported JSON")
	}
	var images []map[string]json.RawMessage
	if raw, ok := wrapper["images"]; !ok || json.Unmarshal(raw, &images) != nil {
		return errors.New("Docker Sandboxes template inventory omitted images")
	}
	for _, image := range images {
		actualRepository, repositoryErr := requiredString(image, "repository", false)
		actualTag, tagErr := requiredString(image, "tag", false)
		actualID, idErr := requiredString(image, "id", false)
		createdAt, createdAtErr := requiredString(image, "created_at", false)
		var size json.Number
		sizeErr := json.Unmarshal(image["size"], &size)
		sizeValue, integerErr := strconv.ParseInt(size.String(), 10, 64)
		if repositoryErr != nil || tagErr != nil || idErr != nil || !validTemplateCacheID(actualID) || createdAtErr != nil || sizeErr != nil || integerErr != nil || sizeValue <= 0 {
			return errors.New("Docker Sandboxes template inventory returned an unsupported image schema")
		}
		if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
			return errors.New("Docker Sandboxes template inventory returned an invalid creation time")
		}
		if actualRepository == repository && actualTag == tag {
			if actualID != cacheID {
				return fmt.Errorf("cached template cache ID %s does not match promoted cache ID %s", actualID, cacheID)
			}
			return nil
		}
	}
	return fmt.Errorf("promoted template %s is not present in the Docker Sandboxes cache", reference)
}

func splitTemplateReference(reference string) (string, string, error) {
	separator := strings.LastIndex(reference, ":")
	if separator <= strings.LastIndex(reference, "/") || separator == len(reference)-1 || strings.Contains(reference, "@") {
		return "", "", errors.New("promoted Docker Sandboxes template must be an exact repository:tag reference")
	}
	repository, tag := reference[:separator], reference[separator+1:]
	if repository == "" || tag == "" {
		return "", "", errors.New("promoted Docker Sandboxes template must be an exact repository:tag reference")
	}
	if !strings.Contains(repository, "/") {
		repository = "docker.io/library/" + repository
	} else {
		first := strings.SplitN(repository, "/", 2)[0]
		if first != "localhost" && !strings.ContainsAny(first, ".:") {
			repository = "docker.io/" + repository
		}
	}
	return repository, tag, nil
}

func verifyPromotedPolicy(data []byte, expected string) error {
	rules, err := parseGlobalPolicy(data)
	if err != nil {
		return err
	}
	actual, err := sandboxpolicy.Fingerprint(rules)
	if err != nil {
		return fmt.Errorf("fingerprint Docker Sandboxes global policy: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("Docker Sandboxes global policy fingerprint is %s, want %s", actual, expected)
	}
	return nil
}

func parseGlobalPolicy(data []byte) ([]provider.NetworkPolicyRule, error) {
	var wrapper map[string]json.RawMessage
	if err := decodeStrictJSON(data, &wrapper); err != nil {
		return nil, errors.New("Docker Sandboxes policy inventory returned unsupported JSON")
	}
	var records []map[string]json.RawMessage
	if raw, ok := wrapper["rules"]; !ok || json.Unmarshal(raw, &records) != nil {
		return nil, errors.New("Docker Sandboxes policy inventory omitted rules")
	}
	var global []provider.NetworkPolicyRule
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		id, idErr := requiredString(record, "id", false)
		name, nameErr := requiredString(record, "name", false)
		policyID, policyErr := requiredString(record, "policy_id", false)
		scope, scopeErr := requiredString(record, "scope", false)
		appliesTo, targetErr := requiredString(record, "applies_to", false)
		resourceType, typeErr := requiredString(record, "resource_type", false)
		decisionText, decisionErr := requiredString(record, "decision", false)
		origin, originErr := requiredString(record, "origin", false)
		status, statusErr := requiredString(record, "status", false)
		var resources []string
		resourcesErr := json.Unmarshal(record["resources"], &resources)
		var editable bool
		editableErr := json.Unmarshal(record["editable"], &editable)
		if idErr != nil || nameErr != nil || policyErr != nil || scopeErr != nil || targetErr != nil || typeErr != nil || decisionErr != nil || originErr != nil || statusErr != nil || resourcesErr != nil || len(resources) == 0 || editableErr != nil {
			return nil, errors.New("Docker Sandboxes policy inventory returned an unsupported rule schema")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("Docker Sandboxes policy inventory returned duplicate rule id %q", id)
		}
		seen[id] = struct{}{}
		decision := provider.NetworkPolicyDecision(strings.ToLower(decisionText))
		if decision != provider.NetworkPolicyAllow && decision != provider.NetworkPolicyDeny {
			return nil, fmt.Errorf("Docker Sandboxes policy rule %q has unsupported decision %q", id, decisionText)
		}
		if scope != "global" {
			if !sandboxScopePattern.MatchString(scope) || appliesTo != scope {
				return nil, fmt.Errorf("Docker Sandboxes policy rule %q has unsupported scope", id)
			}
			if !strings.EqualFold(status, "active") {
				return nil, fmt.Errorf("Docker Sandboxes sandbox-scoped policy rule %q is not active", id)
			}
			continue
		}
		if appliesTo != "all" {
			return nil, fmt.Errorf("Docker Sandboxes global policy rule %q has unsupported target %q", id, appliesTo)
		}
		if !strings.EqualFold(status, "active") {
			return nil, fmt.Errorf("Docker Sandboxes global policy rule %q is not active", id)
		}
		global = append(global, provider.NetworkPolicyRule{
			ID: id, Name: name, PolicyID: policyID, Scope: scope, AppliesTo: appliesTo, ResourceType: resourceType, Resources: resources,
			Decision: decision, Origin: origin, Status: status, Editable: editable, Active: true,
		})
	}
	if len(global) == 0 {
		return nil, errors.New("Docker Sandboxes policy inventory omitted its global baseline")
	}
	return global, nil
}

func requiredString(record map[string]json.RawMessage, key string, allowEmpty bool) (string, error) {
	raw, ok := record[key]
	if !ok {
		return "", errors.New("missing field")
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !allowEmpty && strings.TrimSpace(value) == "" {
		return "", errors.New("invalid field")
	}
	return value, nil
}
