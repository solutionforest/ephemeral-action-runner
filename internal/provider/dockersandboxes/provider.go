package dockersandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/staging"
)

const (
	defaultOutputLimit    = 8 << 20
	diagnosticOutputLimit = 256 << 10
	commandWaitDelay      = 5 * time.Second
	keepaliveStartupDelay = 500 * time.Millisecond
)

const directWorkspaceVerificationScript = `set -euo pipefail
test -z "${SSH_AUTH_SOCK:-}"
test -z "${SSH_AGENT_PID:-}"
workspace="$(pwd -P)"
test -n "${workspace}"
source_options="$(findmnt -T "${workspace}" -n -o OPTIONS)"
case ",${source_options}," in
  *,rw,*) ;;
  *) echo "dedicated host staging workspace is not read-write" >&2; exit 1 ;;
esac
test ! -e .git
test -z "$(find . -mindepth 1 -maxdepth 1 -print -quit)"
test ! -e /run/sandbox/source
! pgrep -x git-daemon >/dev/null`

const runtimeVerificationScript = `set -euo pipefail
test -x /opt/epar/verify-template.sh
test -s /opt/epar/helpers.sha256
cd /opt/epar
sha256sum -c helpers.sha256 >/dev/null
/opt/epar/verify-template.sh >/dev/null
docker info --format '{{json .ServerVersion}}'`

type Provider struct {
	Binary string

	runCommand      runCommandFunc
	inspectImage    inspectImageFunc
	inspectTemplate inspectLocalTemplateFunc
}

type instanceReceipt struct {
	SchemaVersion   int    `json:"schemaVersion"`
	StagingPath     string `json:"stagingPath"`
	StagingIdentity string `json:"stagingIdentity"`
	Template        string `json:"template"`
	TemplateDigest  string `json:"templateDigest"`
}

// CachedTemplate is one image retained in the Docker Sandboxes template cache.
// CacheID is the provider's short cache identity, not a content digest.
type CachedTemplate struct {
	Reference string
	CacheID   string
	CreatedAt time.Time
	SizeBytes int64
}

// LocalTemplateImage is the independently read local Docker image identity
// and guest platform for a repository:tag template reference.
type LocalTemplateImage struct {
	Digest   string
	Platform string
}

// HostReadiness is the validated machine-readable summary returned by
// `sbx diagnose --output json`.
type HostReadiness struct {
	ChecksPassed  int
	ChecksWarned  int
	ChecksFailed  int
	ChecksSkipped int
}

type commandRequest struct {
	args            []string
	stdin           io.Reader
	environment     map[string]string
	stdout          io.Writer
	stderr          io.Writer
	sensitiveValues []string
	operation       string
	outputLimit     int
}

type runCommandFunc func(ctx context.Context, request commandRequest) (provider.ExecResult, error)
type inspectImageFunc func(context.Context, string) (string, error)
type inspectLocalTemplateFunc func(context.Context, string) (LocalTemplateImage, error)

func New(binary string) *Provider {
	if binary == "" {
		binary = "sbx"
	}
	return &Provider{Binary: binary}
}

// EnsureArtifacts records that Docker Sandboxes templates are built and
// imported explicitly. Create verifies the exact configured template digest
// against the provider cache before it allocates an instance.
func (*Provider) EnsureArtifacts(_ context.Context, dryRun bool) (bool, error) {
	if dryRun {
		return true, fmt.Errorf("docker-sandboxes does not support dry-run because the exact prewarmed template must be read back from sbx")
	}
	return true, nil
}

// VerifyHostReadiness requires machine-readable Docker Sandboxes diagnostics
// to contain at least one passing check and no failed checks. Warnings and
// skipped checks do not make an otherwise healthy installation unavailable.
func (p *Provider) VerifyHostReadiness(ctx context.Context) (HostReadiness, error) {
	readiness, err := p.readHostReadiness(ctx)
	if err != nil {
		return HostReadiness{}, fmt.Errorf("%w; run 'sbx diagnose --output json' and review its output", err)
	}
	if readiness.ChecksFailed != 0 {
		return HostReadiness{}, fmt.Errorf("docker sandboxes diagnostics reported %d failed check(s); run 'sbx diagnose --output json' and review the hints for each failed check", readiness.ChecksFailed)
	}
	if readiness.ChecksPassed == 0 {
		return HostReadiness{}, fmt.Errorf("docker sandboxes diagnostics reported no passing checks; run 'sbx diagnose --output json' and review its check details")
	}
	return readiness, nil
}

func (p *Provider) Create(ctx context.Context, request provider.CreateRequest) (provider.Instance, error) {
	if err := validateCreateRequest(request); err != nil {
		return provider.Instance{}, err
	}
	if err := p.VerifyAdmission(ctx); err != nil {
		return provider.Instance{}, err
	}
	if err := p.verifyCachedTemplate(ctx, request.Template, request.TemplateDigest); err != nil {
		return provider.Instance{}, err
	}
	items, err := p.inventoryVerified(ctx)
	if err != nil {
		return provider.Instance{}, err
	}
	for _, item := range items {
		if item.Instance.Name == request.Name {
			return provider.Instance{}, fmt.Errorf("docker sandbox name is already allocated")
		}
	}
	var ownedStaging staging.OwnedDirectory
	if p.runCommand == nil {
		stagingRoot, openErr := staging.Open(filepath.Dir(request.StagingPath))
		if openErr != nil {
			return provider.Instance{}, openErr
		}
		if filepath.Clean(request.StagingPath) != filepath.Join(stagingRoot.Root(), request.Name) {
			return provider.Instance{}, fmt.Errorf("Docker Sandboxes staging path must be the exact provider-owned path for %q", request.Name)
		}
		ownedStaging, err = stagingRoot.CreateOwned(request.Name)
		if err != nil {
			return provider.Instance{}, err
		}
	} else {
		ownedStaging = staging.OwnedDirectory{Path: request.StagingPath, Identity: "test-staging-identity"}
	}

	args := []string{
		"create",
		"--name", request.Name,
		"--cpus", strconv.Itoa(request.CPUs),
		"--memory", request.Memory,
		"--template", request.Template,
	}
	args = append(args, "shell", request.StagingPath)
	environment := make(map[string]string, 2)
	if request.RootDisk != "" {
		environment["DOCKER_SANDBOXES_ROOT_SIZE"] = request.RootDisk
	}
	if request.DockerDisk != "" {
		environment["DOCKER_SANDBOXES_DOCKER_SIZE"] = request.DockerDisk
	}
	if _, err := p.run(ctx, commandRequest{args: args, environment: environment, operation: "create docker sandbox"}); err != nil {
		return provider.Instance{}, err
	}
	items, err = p.inventoryVerified(ctx)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("docker sandbox was created but identity readback failed: %w", err)
	}
	for _, item := range items {
		if item.Instance.Name == request.Name {
			if item.Instance.ProviderID == "" {
				return provider.Instance{}, fmt.Errorf("docker sandbox inventory omitted the stable provider id")
			}
			if item.Source != "shell" || !containsExactWorkspace(item.Workspaces, request.StagingPath) {
				return provider.Instance{}, fmt.Errorf("docker sandbox inventory did not bind the exact shell workspace")
			}
			if err := p.verifyNoPublishedPorts(ctx, item.Instance); err != nil {
				return provider.Instance{}, err
			}
			if err := p.verifyInspection(ctx, item.Instance, &request); err != nil {
				return provider.Instance{}, err
			}
			if err := p.verifyDirectWorkspace(ctx, item.Instance); err != nil {
				return provider.Instance{}, err
			}
			receipt, encodeErr := json.Marshal(instanceReceipt{
				SchemaVersion:   1,
				StagingPath:     ownedStaging.Path,
				StagingIdentity: ownedStaging.Identity,
				Template:        request.Template,
				TemplateDigest:  request.TemplateDigest,
			})
			if encodeErr != nil {
				return provider.Instance{}, encodeErr
			}
			instance := item.Instance
			instance.ReceiptVersion = "v1"
			instance.Receipt = receipt
			return instance, nil
		}
	}
	return provider.Instance{}, fmt.Errorf("docker sandbox was not present in inventory after create")
}

// VerifyAdmission fail-closes on provider-wide channels Docker Sandboxes can
// inject into every sandbox. EPAR deliberately does not consume global secrets;
// repository and workflow input can never opt out of this check.
func (p *Provider) VerifyAdmission(ctx context.Context) error {
	if _, err := p.VerifyHostReadiness(ctx); err != nil {
		return err
	}
	return p.verifyNoGlobalSecrets(ctx)
}

func (p *Provider) VerifyInstanceAdmission(ctx context.Context, instance provider.Instance) error {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("docker sandbox is missing")
	}
	if err := p.verifyNoPublishedPorts(ctx, instance); err != nil {
		return err
	}
	return p.verifyInspection(ctx, instance, nil)
}

func (p *Provider) verifyInspection(ctx context.Context, instance provider.Instance, expected *provider.CreateRequest) error {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"inspect", "--json", instance.Name},
		operation:   "verify docker sandbox attached capabilities",
		outputLimit: diagnosticOutputLimit,
	})
	if err != nil {
		return err
	}
	var inspection map[string]json.RawMessage
	if err := decodeStrictJSON([]byte(result.Stdout), &inspection); err != nil {
		return fmt.Errorf("docker sandbox inspection returned an unsupported JSON schema")
	}
	if stringValue(inspection["name"]) != instance.Name || stringValue(inspection["agent"]) != "shell" || strings.TrimSpace(stringValue(inspection["daemon_version"])) == "" {
		return fmt.Errorf("docker sandbox inspection did not match the exact shell runtime")
	}
	var mcpGateway bool
	if raw, ok := inspection["mcp_gateway"]; !ok || json.Unmarshal(raw, &mcpGateway) != nil || mcpGateway {
		return fmt.Errorf("docker sandbox inspection reported an enabled MCP gateway")
	}
	if expected != nil && (stringValue(inspection["image"]) != expected.Template || stringValue(inspection["image_digest"]) != expected.TemplateDigest || stringValue(inspection["workspace"]) != expected.StagingPath) {
		return fmt.Errorf("docker sandbox inspection did not bind the exact template identity and staging path")
	}
	for _, field := range []string{"kits", "secrets", "published_ports", "ports", "auth", "auth_mode", "docker_auth"} {
		value, ok := inspection[field]
		if field == "kits" && !ok {
			return fmt.Errorf("docker sandbox inspection omitted required attached-capability field %q", field)
		}
		if ok && !emptyJSONValue(value) {
			return fmt.Errorf("docker sandbox inspection reported forbidden attached capability %q", field)
		}
	}
	return nil
}

func stringValue(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func emptyJSONValue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		return typed == ""
	case float64:
		return typed == 0
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func (p *Provider) verifyNoPublishedPorts(ctx context.Context, instance provider.Instance) error {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"ports", instance.Name, "--json"},
		operation:   "verify docker sandbox has no published ports",
		outputLimit: diagnosticOutputLimit,
	})
	if err != nil {
		return err
	}
	var ports []map[string]json.RawMessage
	if err := decodeStrictJSON([]byte(result.Stdout), &ports); err != nil || ports == nil {
		return fmt.Errorf("docker sandbox published-port inventory returned an unsupported JSON schema")
	}
	if len(ports) != 0 {
		return fmt.Errorf("docker sandbox reported a forbidden published port")
	}
	return nil
}

func (p *Provider) verifyNoGlobalSecrets(ctx context.Context) error {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"secret", "ls", "-g"},
		operation:   "verify docker sandboxes global secret isolation",
		outputLimit: 64 << 10,
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(strings.ReplaceAll(result.Stdout, "\r\n", "\n")) != `No secrets found for scope "(global)".` {
		return fmt.Errorf("docker sandboxes global secrets are configured; EPAR refuses to expose shared registry or service credentials to workflow sandboxes")
	}
	return nil
}

func (p *Provider) verifyDirectWorkspace(ctx context.Context, instance provider.Instance) error {
	result, err := p.run(ctx, commandRequest{
		args:      []string{"exec", "-i", instance.Name, "--", "bash", "-lc", directWorkspaceVerificationScript},
		stdin:     strings.NewReader(""),
		operation: "verify dedicated docker sandbox staging workspace",
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("dedicated docker sandbox staging verification returned unexpected output")
	}
	return nil
}

func containsExactWorkspace(workspaces []string, expected string) bool {
	for _, workspace := range workspaces {
		if workspace == expected {
			return true
		}
	}
	return false
}

func (p *Provider) Start(ctx context.Context, instance provider.Instance, opts provider.StartOptions) (*provider.RunningProcess, error) {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil || !present {
		if err == nil {
			return nil, fmt.Errorf("docker sandbox is missing")
		}
		return nil, err
	}
	request := commandRequest{
		args:      []string{"exec", "-i", instance.Name, "--", "/bin/sleep", "infinity"},
		stdin:     strings.NewReader(""),
		stdout:    opts.Stdout,
		stderr:    opts.Stderr,
		operation: "start docker sandbox with a managed keepalive",
	}
	if p.runCommand != nil {
		if _, err := p.run(ctx, request); err != nil {
			return nil, err
		}
		return &provider.RunningProcess{Name: instance.Name}, nil
	}
	return p.startKeepalive(ctx, instance.Name, request)
}

func (p *Provider) startKeepalive(ctx context.Context, name string, request commandRequest) (*provider.RunningProcess, error) {
	if err := validateCommandRequest(request); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, p.Binary, request.args...)
	isolateKeepaliveProcess(command)
	command.WaitDelay = commandWaitDelay
	command.Stdin = request.stdin
	command.Env = childEnvironment(request.environment)
	stdout := &boundedBuffer{limit: defaultOutputLimit}
	stderr := &boundedBuffer{limit: defaultOutputLimit}
	command.Stdout = captureWriter(stdout, request.stdout)
	command.Stderr = captureWriter(stderr, request.stderr)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("%s failed: %w", request.operation, err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- command.Wait()
	}()
	timer := time.NewTimer(keepaliveStartupDelay)
	defer timer.Stop()
	select {
	case err := <-finished:
		detail := strings.TrimSpace(stderr.String())
		if err == nil {
			err = fmt.Errorf("keepalive command exited before startup completed")
		}
		if detail != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", request.operation, err, detail)
		}
		return nil, fmt.Errorf("%s failed: %w", request.operation, err)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return &provider.RunningProcess{Name: name, PID: command.Process.Pid}, nil
	}
}

func (p *Provider) VerifyRuntime(ctx context.Context, instance provider.Instance) (provider.RuntimeInfo, error) {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return provider.RuntimeInfo{}, err
	}
	if !present {
		return provider.RuntimeInfo{}, fmt.Errorf("docker sandbox is missing")
	}
	result, err := p.run(ctx, commandRequest{
		args:      []string{"exec", "-i", instance.Name, "--", "bash", "-lc", runtimeVerificationScript},
		stdin:     strings.NewReader(""),
		operation: "verify docker sandbox runtime",
	})
	if err != nil {
		return provider.RuntimeInfo{}, err
	}
	var version string
	if err := decodeStrictJSON([]byte(strings.TrimSpace(result.Stdout)), &version); err != nil || strings.TrimSpace(version) == "" {
		return provider.RuntimeInfo{}, fmt.Errorf("docker sandbox runtime returned an unsupported verification schema")
	}
	return provider.RuntimeInfo{Ready: true, Runtime: "docker", Version: version}, nil
}

func (*Provider) Address(context.Context, provider.Instance, int) (string, bool, error) {
	return "", false, nil
}

func (p *Provider) Exec(ctx context.Context, instance provider.Instance, command []string, opts provider.ExecOptions) (provider.ExecResult, error) {
	if err := validateGuestCommand(command, opts); err != nil {
		return provider.ExecResult{}, err
	}
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return provider.ExecResult{}, err
	}
	if !present {
		return provider.ExecResult{}, fmt.Errorf("docker sandbox is missing")
	}
	args := make([]string, 0, len(command)+5)
	args = append(args, "exec", "-i", instance.Name, "--")
	args = append(args, command...)
	return p.run(ctx, commandRequest{
		args:            args,
		stdin:           strings.NewReader(opts.Stdin),
		stdout:          opts.Stdout,
		stderr:          opts.Stderr,
		sensitiveValues: opts.SensitiveValues,
		operation:       "execute in docker sandbox",
	})
}

func (p *Provider) Diagnostics(ctx context.Context, instance provider.Instance) (provider.Diagnostics, error) {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return provider.Diagnostics{}, err
	}
	if !present {
		return provider.Diagnostics{}, fmt.Errorf("docker sandbox is missing")
	}
	statusResult, err := p.run(ctx, commandRequest{
		args:        []string{"daemon", "status", "--json"},
		operation:   "read docker sandbox daemon status",
		outputLimit: diagnosticOutputLimit,
	})
	if err != nil {
		return provider.Diagnostics{}, err
	}
	daemonState, daemonHealthy, err := parseDaemonStatus([]byte(statusResult.Stdout))
	if err != nil {
		return provider.Diagnostics{}, err
	}
	readiness, err := p.readHostReadiness(ctx)
	if err != nil {
		return provider.Diagnostics{}, err
	}
	return provider.Diagnostics{
		Healthy:       daemonHealthy && readiness.ChecksPassed > 0 && readiness.ChecksFailed == 0,
		DaemonState:   daemonState,
		ChecksPassed:  readiness.ChecksPassed,
		ChecksWarned:  readiness.ChecksWarned,
		ChecksFailed:  readiness.ChecksFailed,
		ChecksSkipped: readiness.ChecksSkipped,
	}, nil
}

func (p *Provider) readHostReadiness(ctx context.Context) (HostReadiness, error) {
	diagnoseResult, err := p.run(ctx, commandRequest{
		args:        []string{"diagnose", "--output", "json"},
		operation:   "diagnose docker sandboxes",
		outputLimit: diagnosticOutputLimit,
	})
	if err != nil {
		return HostReadiness{}, err
	}
	passed, warned, failed, skipped, err := parseDiagnose([]byte(diagnoseResult.Stdout))
	if err != nil {
		return HostReadiness{}, err
	}
	return HostReadiness{
		ChecksPassed:  passed,
		ChecksWarned:  warned,
		ChecksFailed:  failed,
		ChecksSkipped: skipped,
	}, nil
}

func (p *Provider) Stop(ctx context.Context, instance provider.Instance) error {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil || !present {
		return err
	}
	result, err := p.run(ctx, commandRequest{args: []string{"stop", instance.Name}, operation: "stop docker sandbox"})
	if err != nil && isMissingSandbox(result.Stdout+"\n"+result.Stderr+"\n"+err.Error()) {
		return nil
	}
	return err
}

func (p *Provider) Delete(ctx context.Context, instance provider.Instance) error {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil || !present {
		return err
	}
	var receipt instanceReceipt
	if p.runCommand == nil {
		if instance.ReceiptVersion != "v1" || json.Unmarshal(instance.Receipt, &receipt) != nil || receipt.SchemaVersion != 1 || receipt.StagingPath == "" || receipt.StagingIdentity == "" {
			return fmt.Errorf("refusing Docker Sandbox deletion without an exact staging ownership receipt")
		}
	}
	result, err := p.run(ctx, commandRequest{args: []string{"rm", "--force", instance.Name}, operation: "delete docker sandbox"})
	if err != nil && isMissingSandbox(result.Stdout+"\n"+result.Stderr+"\n"+err.Error()) {
		err = nil
	}
	if err != nil {
		return err
	}
	if p.runCommand == nil {
		stagingRoot, openErr := staging.Open(filepath.Dir(receipt.StagingPath))
		if openErr != nil {
			return openErr
		}
		if filepath.Clean(receipt.StagingPath) != filepath.Join(stagingRoot.Root(), instance.Name) {
			return fmt.Errorf("refusing Docker Sandbox staging cleanup outside the exact owned path")
		}
		if purgeErr := stagingRoot.PurgeOwned(instance.Name, receipt.StagingIdentity); purgeErr != nil {
			return purgeErr
		}
	}
	return nil
}

func (p *Provider) Inventory(ctx context.Context) ([]provider.InventoryItem, error) {
	return p.inventoryVerified(ctx)
}

func (p *Provider) inventoryVerified(ctx context.Context) ([]provider.InventoryItem, error) {
	result, err := p.run(ctx, commandRequest{args: []string{"ls", "--json"}, operation: "inventory docker sandboxes"})
	if err != nil {
		return nil, err
	}
	return parseInventory([]byte(result.Stdout))
}

// CachedTemplates returns the strictly parsed, host-level Docker Sandboxes
// template cache inventory. It does not create, load, or otherwise mutate a
// template.
func (p *Provider) CachedTemplates(ctx context.Context) ([]CachedTemplate, error) {
	result, err := p.run(ctx, commandRequest{
		args:        []string{"template", "ls", "--json"},
		operation:   "read docker sandbox template cache",
		outputLimit: diagnosticOutputLimit,
	})
	if err != nil {
		return nil, err
	}
	images, err := parseTemplateInventory([]byte(result.Stdout))
	if err != nil {
		return nil, err
	}
	templates := make([]CachedTemplate, 0, len(images))
	for _, image := range images {
		templates = append(templates, CachedTemplate{
			Reference: image.Repository + ":" + image.Tag,
			CacheID:   image.ID,
			CreatedAt: image.CreatedAt,
			SizeBytes: image.Size,
		})
	}
	return templates, nil
}

// InspectLocalTemplate independently reads a local Docker image's full
// identity and guest platform. The supplied reference must be a repository:tag
// reference; digests, untagged repositories, and shell-style input are refused.
func (p *Provider) InspectLocalTemplate(ctx context.Context, reference string) (LocalTemplateImage, error) {
	if err := validateLocalTemplateReference(reference); err != nil {
		return LocalTemplateImage{}, err
	}
	inspect := p.inspectTemplate
	if inspect == nil {
		inspect = inspectLocalDockerImage
	}
	image, err := inspect(ctx, reference)
	if err != nil {
		return LocalTemplateImage{}, err
	}
	if !validFullTemplateDigest(image.Digest) {
		return LocalTemplateImage{}, fmt.Errorf("local docker image inspection did not return a full lowercase sha256 identity")
	}
	if image.Platform != "linux/amd64" && image.Platform != "linux/arm64" {
		return LocalTemplateImage{}, fmt.Errorf("local docker image inspection did not return a supported linux template platform")
	}
	return image, nil
}

func (p *Provider) verifyCachedTemplate(ctx context.Context, reference, digest string) error {
	if err := p.verifyLocalTemplateImage(ctx, reference, digest); err != nil {
		return err
	}
	result, err := p.run(ctx, commandRequest{args: []string{"template", "ls", "--json"}, operation: "verify cached docker sandbox template"})
	if err != nil {
		return err
	}
	images, err := parseTemplateInventory([]byte(result.Stdout))
	if err != nil {
		return err
	}
	repository, tag, err := splitTemplateReference(reference)
	if err != nil {
		return err
	}
	wantCacheID := strings.TrimPrefix(digest, "sha256:")[:12]
	for _, image := range images {
		if image.Repository == repository && image.Tag == tag {
			if image.ID != wantCacheID {
				return fmt.Errorf("cached docker sandbox template cache ID did not match the first 12 hexadecimal characters of the independently verified full local image identity")
			}
			return nil
		}
	}
	return fmt.Errorf("configured docker sandbox template was not present in the local cache")
}

func (p *Provider) verifyLocalTemplateImage(ctx context.Context, reference, expectedDigest string) error {
	inspect := p.inspectImage
	if inspect == nil {
		inspect = inspectLocalDockerImageDigest
	}
	actualDigest, err := inspect(ctx, reference)
	if err != nil {
		return fmt.Errorf("verify full local docker sandbox template image identity: %w", err)
	}
	actualDigest = strings.TrimSpace(actualDigest)
	if !validFullTemplateDigest(actualDigest) {
		return fmt.Errorf("local docker image inspection did not return a full lowercase sha256 identity")
	}
	if actualDigest != expectedDigest {
		return fmt.Errorf("full local docker sandbox template image identity %s did not match configured identity %s", actualDigest, expectedDigest)
	}
	return nil
}

func inspectLocalDockerImageDigest(ctx context.Context, reference string) (string, error) {
	image, err := inspectLocalDockerImage(ctx, reference)
	if err != nil {
		return "", err
	}
	return image.Digest, nil
}

func inspectLocalDockerImage(ctx context.Context, reference string) (LocalTemplateImage, error) {
	command := exec.CommandContext(ctx, "docker", localTemplateInspectArgs(reference)...)
	command.Env = childEnvironment(nil)
	stdout := &boundedBuffer{limit: diagnosticOutputLimit}
	stderr := &boundedBuffer{limit: 4096}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		err = errors.Join(err, fmt.Errorf("docker image inspection output limit exceeded"))
	}
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return LocalTemplateImage{}, fmt.Errorf("docker image inspect failed: %w: %s", err, detail)
		}
		return LocalTemplateImage{}, fmt.Errorf("docker image inspect failed: %w", err)
	}
	return parseLocalTemplateImage([]byte(stdout.String()))
}

func localTemplateInspectArgs(reference string) []string {
	return []string{"image", "inspect", "--format", "{{json .}}", reference}
}

func validFullTemplateDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func (p *Provider) assertIdentity(ctx context.Context, instance provider.Instance) (bool, error) {
	if err := validateInstance(instance, true); err != nil {
		return false, err
	}
	items, err := p.inventoryVerified(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Instance.Name != instance.Name {
			continue
		}
		if item.Instance.ProviderID != instance.ProviderID {
			return false, fmt.Errorf("docker sandbox identity changed")
		}
		return true, nil
	}
	return false, nil
}

func (p *Provider) run(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
	if err := validateCommandRequest(request); err != nil {
		return provider.ExecResult{}, err
	}
	if request.outputLimit == 0 {
		request.outputLimit = defaultOutputLimit
	}
	if err := ctx.Err(); err != nil {
		return provider.ExecResult{}, err
	}
	bufferedStdout, bufferedStderr, flush := provider.BufferSensitiveSinks(request.sensitiveValues, request.stdout, request.stderr)
	request.stdout = bufferedStdout
	request.stderr = bufferedStderr

	var result provider.ExecResult
	var runErr error
	if p.runCommand != nil {
		result, runErr = p.runCommand(ctx, request)
	} else {
		result, runErr = p.runRaw(ctx, request)
	}
	if len(result.Stdout) > request.outputLimit || len(result.Stderr) > request.outputLimit {
		runErr = errors.Join(runErr, fmt.Errorf("%s exceeded the output limit", request.operation))
		result.Stdout = truncate(result.Stdout, request.outputLimit)
		result.Stderr = truncate(result.Stderr, request.outputLimit)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		runErr = errors.Join(ctxErr, runErr)
	}
	result, finishErr := provider.FinishSensitiveExecution(result, runErr, flush(), request.sensitiveValues)
	if finishErr != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail != "" {
			finishErr = fmt.Errorf("%s failed: %w: %s", request.operation, finishErr, detail)
		} else {
			finishErr = fmt.Errorf("%s failed: %w", request.operation, finishErr)
		}
		finishErr = provider.RedactError(finishErr, request.sensitiveValues...)
	}
	return result, finishErr
}

func (p *Provider) runRaw(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
	cmd := exec.CommandContext(ctx, p.Binary, request.args...)
	cmd.WaitDelay = commandWaitDelay
	var cancellationKilledProcess atomic.Bool
	defaultCancel := cmd.Cancel
	cmd.Cancel = func() error {
		err := defaultCancel()
		if err == nil {
			cancellationKilledProcess.Store(true)
		}
		return err
	}
	cmd.Stdin = request.stdin
	cmd.Env = childEnvironment(request.environment)
	stdout := &boundedBuffer{limit: request.outputLimit}
	stderr := &boundedBuffer{limit: request.outputLimit}
	cmd.Stdout = captureWriter(stdout, request.stdout)
	cmd.Stderr = captureWriter(stderr, request.stderr)
	err := cmd.Run()
	if cancellationKilledProcess.Load() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
	}
	result := provider.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if stdout.exceeded || stderr.exceeded {
		err = errors.Join(err, fmt.Errorf("output limit exceeded"))
	}
	return result, err
}

func validateCommandRequest(request commandRequest) error {
	if len(request.args) == 0 {
		return fmt.Errorf("refusing to invoke docker sandboxes without a subcommand")
	}
	if request.operation == "" {
		return fmt.Errorf("docker sandboxes operation label is required")
	}
	switch request.args[0] {
	case "version", "create", "exec", "daemon", "diagnose", "stop", "rm", "ls", "template", "policy", "inspect", "ports":
	case "secret":
		if len(request.args) != 3 || request.args[1] != "ls" || request.args[2] != "-g" {
			return fmt.Errorf("only exact global-secret absence verification is permitted")
		}
	default:
		return fmt.Errorf("docker sandboxes command %q is not permitted", request.args[0])
	}
	if request.args[0] == "inspect" && (len(request.args) != 3 || request.args[1] != "--json" || !sandboxNamePattern.MatchString(request.args[2])) {
		return fmt.Errorf("only exact machine-readable sandbox inspection is permitted")
	}
	if request.args[0] == "ports" && (len(request.args) != 3 || !sandboxNamePattern.MatchString(request.args[1]) || request.args[2] != "--json") {
		return fmt.Errorf("only exact machine-readable published-port absence verification is permitted")
	}
	for _, arg := range request.args {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("docker sandboxes argument contains a null byte")
		}
	}
	for key := range request.environment {
		if key != "DOCKER_SANDBOXES_ROOT_SIZE" && key != "DOCKER_SANDBOXES_DOCKER_SIZE" {
			return fmt.Errorf("docker sandboxes child environment contains a forbidden override")
		}
	}
	return nil
}

func childEnvironment(additions map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(additions))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		upperKey := strings.ToUpper(key)
		if strings.HasPrefix(upperKey, "DOCKER_SANDBOXES_") || upperKey == "SSH_AUTH_SOCK" || upperKey == "SSH_AGENT_PID" {
			continue
		}
		environment = append(environment, item)
	}
	for _, key := range []string{"DOCKER_SANDBOXES_ROOT_SIZE", "DOCKER_SANDBOXES_DOCKER_SIZE"} {
		if value := additions[key]; value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func captureWriter(capture io.Writer, sink io.Writer) io.Writer {
	if sink == nil {
		return capture
	}
	return io.MultiWriter(capture, sink)
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		_, _ = buffer.buffer.Write(data[:remaining])
	}
	if len(data) > remaining {
		buffer.exceeded = true
	}
	return written, nil
}

func (buffer *boundedBuffer) String() string { return buffer.buffer.String() }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func isMissingSandbox(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "sandbox not found") || strings.Contains(text, "no such sandbox") || strings.Contains(text, "status 404")
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("unexpected trailing json value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing json value")
		}
		return err
	}
	return nil
}

var _ provider.Lifecycle = (*Provider)(nil)
var _ provider.AdmissionVerifier = (*Provider)(nil)
var _ provider.InstanceAdmissionVerifier = (*Provider)(nil)
var _ provider.PolicyManager = (*Provider)(nil)
