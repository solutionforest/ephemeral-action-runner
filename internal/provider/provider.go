package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

var ErrTemplateNotFound = errors.New("imported provider template not found")

type Instance struct {
	Name           string
	ProviderID     string
	Source         string
	State          string
	ReceiptVersion string
	Receipt        json.RawMessage
}

// CreateRequest is the provider-neutral input for allocating one isolated
// runtime. Providers must reject fields they cannot honor instead of silently
// weakening the requested isolation or resource constraints.
type CreateRequest struct {
	Name     string
	Source   string
	Template string
	// TemplateDigest is the full sha256 local image/template identity recorded by
	// the trusted build and load manifest. Providers must fail closed when they
	// cannot bind the configured template reference to this identity.
	TemplateDigest string
	StagingPath    string
	CPUs           int
	Memory         string
	RootDisk       string
	DockerDisk     string
}

type RuntimeInfo struct {
	Ready   bool
	Runtime string
	Version string
}

type Diagnostics struct {
	Healthy       bool
	DaemonState   string
	ChecksPassed  int
	ChecksWarned  int
	ChecksFailed  int
	ChecksSkipped int
	OutputLimited bool
}

type InventoryItem struct {
	Instance Instance
	State    string
	Source   string
	// Workspaces are the exact host paths reported by the provider for this
	// instance. They are ownership evidence for crash reconciliation; callers
	// must compare canonical paths using host-platform path semantics.
	Workspaces []string
}

type StartOptions struct {
	Network    string
	RosettaTag string
	LogPath    string
	Stdout     io.Writer
	Stderr     io.Writer
}

type RunningProcess struct {
	Name    string
	PID     int
	LogPath string
}

type ExecOptions struct {
	Stdin              string
	StdinReader        io.Reader
	Env                map[string]string
	SensitiveValues    []string
	LogPath            string
	Stdout             io.Writer
	Stderr             io.Writer
	SuppressTranscript bool
}

type ExecResult struct {
	Stdout string
	Stderr string
}

// Lifecycle is the provider contract used by new orchestration code. Address
// discovery is explicitly optional because delegated runtimes such as Docker
// Sandboxes intentionally expose command execution without a host-routable
// guest address.
type Lifecycle interface {
	Create(ctx context.Context, request CreateRequest) (Instance, error)
	Start(ctx context.Context, instance Instance, opts StartOptions) (*RunningProcess, error)
	VerifyRuntime(ctx context.Context, instance Instance) (RuntimeInfo, error)
	Address(ctx context.Context, instance Instance, waitSeconds int) (address string, available bool, err error)
	Exec(ctx context.Context, instance Instance, command []string, opts ExecOptions) (ExecResult, error)
	Diagnostics(ctx context.Context, instance Instance) (Diagnostics, error)
	Stop(ctx context.Context, instance Instance) error
	Delete(ctx context.Context, instance Instance) error
	Inventory(ctx context.Context) ([]InventoryItem, error)
}

// ArtifactManager is an optional provider capability for runtimes whose
// reusable artifact is not prepared by the shared OCI image pipeline.
type ArtifactManager interface {
	EnsureArtifacts(ctx context.Context, dryRun bool) (handled bool, err error)
}

// TemplateArtifact is the exact immutable reusable artifact selected by the
// shared image coordinator for a template-backed provider.
type TemplateArtifact struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
	CacheID   string `json:"cacheId"`
	Platform  string `json:"platform"`
	RootDisk  string `json:"rootDisk,omitempty"`
}

// TemplateArtifactRuntime exposes only provider-specific template-cache
// integration. Source resolution, builds, manifests, receipts, and retention
// remain owned by the shared image and storage packages.
type TemplateArtifactRuntime interface {
	ImportTemplate(ctx context.Context, archivePath string) error
	VerifyImportedTemplate(ctx context.Context, artifact TemplateArtifact) error
	ActivateTemplate(artifact TemplateArtifact) error
}

// TemplateArtifactCleaner is an optional exact cleanup capability for
// template-backed providers. The shared image/storage lifecycle calls it only
// for an immutable cache identity backed by EPAR ownership evidence.
type TemplateArtifactCleaner interface {
	RemoveTemplate(ctx context.Context, artifact TemplateArtifact) error
}

// TemplateArtifactObserver performs an exact, read-only cache lookup for
// catalog reconciliation without activating or deleting the template.
type TemplateArtifactObserver interface {
	ObserveTemplate(ctx context.Context, artifact TemplateArtifact) (bool, error)
}

// StorageContribution is required for every registered provider. It describes
// the provider's measurable capacity surface and operation expansion before
// the shared pool performs provider side effects.
type StorageContribution interface {
	StorageSnapshot(ctx context.Context, request StorageRequest) (StorageSnapshot, error)
}

type StorageRequest struct {
	Operation        string
	Now              time.Time
	PeakBytes        uint64
	MinimumFreeBytes uint64
}

type StorageSnapshot struct {
	Surfaces     []storage.Surface
	Requirements []storage.Requirement
	Artifacts    []storage.Artifact
}

// AdmissionVerifier rechecks provider-wide state that can change independently
// of one sandbox. Callers use it before registration and while issuing bounded
// job-admission leases so shared host configuration cannot silently weaken an
// already-created runtime.
type AdmissionVerifier interface {
	VerifyAdmission(ctx context.Context) error
}

// InstanceAdmissionVerifier rechecks mutable provider state attached to one
// exact runtime, including kits, injected authentication, secrets, published
// ports, and management gateways. It is deliberately separate from general
// runtime health because any violation must stop job admission immediately.
type InstanceAdmissionVerifier interface {
	VerifyInstanceAdmission(ctx context.Context, instance Instance) error
}

type NetworkPolicyDecision string

const (
	NetworkPolicyAllow NetworkPolicyDecision = "allow"
	NetworkPolicyDeny  NetworkPolicyDecision = "deny"
)

// NetworkPolicyRule is the attributed effective-policy record returned by a
// policy-capable provider. Read results include every relevant resource type;
// the current mutation methods remain deliberately limited to network rules.
type NetworkPolicyRule struct {
	ID           string
	Name         string
	PolicyID     string
	Scope        string
	AppliesTo    string
	ResourceType string
	Resources    []string
	Decision     NetworkPolicyDecision
	Origin       string
	Status       string
	Editable     bool
	Active       bool
}

// PolicyManager is implemented only by providers that can apply and verify
// exact, instance-scoped network rules. Global policy mutation is deliberately
// absent from this contract.
type PolicyManager interface {
	ApplyNetworkPolicy(ctx context.Context, instance Instance, rules []NetworkPolicyRule) error
	ReadNetworkPolicy(ctx context.Context, instance Instance) ([]NetworkPolicyRule, error)
	RemoveNetworkPolicy(ctx context.Context, instance Instance, rules []NetworkPolicyRule) error
}

// Provider is the legacy EPAR provider contract. New orchestration code should
// consume Lifecycle and wrap existing providers with AdaptLegacy.
type Provider interface {
	Clone(ctx context.Context, source, name string) error
	Start(ctx context.Context, name string, opts StartOptions) (*RunningProcess, error)
	Exec(ctx context.Context, name string, command []string, opts ExecOptions) (ExecResult, error)
	IP(ctx context.Context, name string, waitSeconds int) (string, error)
	Stop(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]Instance, error)
}

func CopyText(ctx context.Context, p Provider, vmName, path, mode, content string) error {
	tmp := "/tmp/epar-copy"
	cmd := []string{"bash", "-lc", fmt.Sprintf("cat > %s && if command -v sudo >/dev/null 2>&1; then sudo install -m %s %s %s; else install -m %s %s %s; fi && rm -f %s", shellQuote(tmp), shellQuote(mode), shellQuote(tmp), shellQuote(path), shellQuote(mode), shellQuote(tmp), shellQuote(path), shellQuote(tmp))}
	_, err := p.Exec(ctx, vmName, cmd, ExecOptions{Stdin: content})
	return err
}

// CopyTextAtomic installs content through a sibling temporary file and then
// renames it over path. Callers must ensure the destination directory exists.
func CopyTextAtomic(ctx context.Context, p Provider, vmName, path, mode, content string) error {
	tmp := path + ".tmp"
	staging := "/tmp/epar-copy"
	cmd := []string{"bash", "-lc", fmt.Sprintf("cat > %s && if command -v sudo >/dev/null 2>&1; then sudo install -m %s %s %s && sudo mv -f %s %s; else install -m %s %s %s && mv -f %s %s; fi && rm -f %s", shellQuote(staging), shellQuote(mode), shellQuote(staging), shellQuote(tmp), shellQuote(tmp), shellQuote(path), shellQuote(mode), shellQuote(staging), shellQuote(tmp), shellQuote(tmp), shellQuote(path), shellQuote(staging))}
	_, err := p.Exec(ctx, vmName, cmd, ExecOptions{Stdin: content})
	return err
}

// CopyFile streams one regular host file into a guest without loading the
// complete payload into memory. The provider command installs through a
// temporary file and removes it on both success and failure.
func CopyFile(ctx context.Context, p Provider, vmName, source, destination, mode string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("copy source %s must be a regular file", source)
	}
	tmp := "/tmp/epar-copy"
	cmd := []string{"bash", "-lc", fmt.Sprintf("trap 'rm -f %s' EXIT; cat > %s && if command -v sudo >/dev/null 2>&1; then sudo install -m %s %s %s; else install -m %s %s %s; fi", shellQuote(tmp), shellQuote(tmp), shellQuote(mode), shellQuote(tmp), shellQuote(destination), shellQuote(mode), shellQuote(tmp), shellQuote(destination))}
	_, err = p.Exec(ctx, vmName, cmd, ExecOptions{StdinReader: file})
	return err
}

func ShellCommand(script string) []string {
	return []string{"bash", "-lc", script}
}

func EnvCommand(env map[string]string, command []string) []string {
	if len(env) == 0 {
		return command
	}
	out := []string{"env"}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return append(out, command...)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
