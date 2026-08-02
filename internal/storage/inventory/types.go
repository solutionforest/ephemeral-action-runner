package inventory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const (
	ProviderDockerSandboxes = "docker-sandboxes"
	ProjectSurfaceID        = "project-filesystem"
)

// TemplateSelection is an explicit configured template identity. Profile and
// Platform may be omitted when the caller has only the configured tag and full
// template digest. ActivatedAt is optional; without it, non-current archives
// remain retention-uncertain.
type TemplateSelection struct {
	Profile        string    `json:"profile"`
	Platform       string    `json:"platform"`
	Tag            string    `json:"tag"`
	TemplateDigest string    `json:"templateDigest"`
	MetadataSHA256 string    `json:"metadataSha256,omitempty"`
	ActivatedAt    time.Time `json:"activatedAt,omitempty"`
}

// TemplateProtection preserves an archive by its full content digest.
type TemplateProtection struct {
	ArchiveSHA256 string                 `json:"archiveSha256"`
	Kind          storage.ProtectionKind `json:"kind"`
	Detail        string                 `json:"detail,omitempty"`
}

// ConfiguredFile identifies one provider artifact by its explicit persisted
// configuration path. Configured files are inventoried with exact filesystem
// identity, but remain protected from automatic cleanup.
type ConfiguredFile struct {
	Provider         string                 `json:"provider"`
	Role             string                 `json:"role"`
	Path             string                 `json:"path"`
	Kind             storage.ArtifactKind   `json:"kind"`
	Current          bool                   `json:"current,omitempty"`
	ConfiguredAt     time.Time              `json:"configuredAt,omitempty"`
	ProtectionKind   storage.ProtectionKind `json:"protectionKind,omitempty"`
	ProtectionDetail string                 `json:"protectionDetail,omitempty"`
}

// Options supplies all paths and active identities explicitly. Empty storage
// roots use their project-local defaults.
type Options struct {
	ProjectRoot         string               `json:"projectRoot"`
	Provider            string               `json:"provider,omitempty"`
	Now                 time.Time            `json:"now"`
	LogsRoot            string               `json:"logsRoot,omitempty"`
	NativeRoot          string               `json:"nativeRoot,omitempty"`
	TemplateRoot        string               `json:"templateRoot,omitempty"`
	CurrentExecutable   string               `json:"currentExecutable,omitempty"`
	CurrentRevision     string               `json:"currentRevision,omitempty"`
	ConfiguredTemplates []TemplateSelection  `json:"configuredTemplates,omitempty"`
	TemplateProtections []TemplateProtection `json:"templateProtections,omitempty"`
	ConfiguredFiles     []ConfiguredFile     `json:"configuredFiles,omitempty"`
}

// Snapshot is a deterministic storage-core input plus fail-closed collection
// warnings.
type Snapshot struct {
	CollectedAt    time.Time          `json:"collectedAt"`
	ProjectRoot    string             `json:"projectRoot"`
	ProviderFilter string             `json:"providerFilter,omitempty"`
	Surfaces       []storage.Surface  `json:"surfaces"`
	Artifacts      []storage.Artifact `json:"artifacts,omitempty"`
	Warnings       []string           `json:"warnings,omitempty"`
}

// PreviewRequest converts an inventory into a storage plan request without
// changing any inventory classification.
func (snapshot Snapshot) PreviewRequest(policy storage.Policy, requirements []storage.Requirement) storage.PreviewRequest {
	return storage.PreviewRequest{
		Now:          snapshot.CollectedAt,
		Policy:       policy,
		Surfaces:     append([]storage.Surface(nil), snapshot.Surfaces...),
		Requirements: append([]storage.Requirement(nil), requirements...),
		Artifacts:    append([]storage.Artifact(nil), snapshot.Artifacts...),
	}
}

func (snapshot *Snapshot) normalize() {
	sort.Slice(snapshot.Surfaces, func(i, j int) bool { return snapshot.Surfaces[i].ID < snapshot.Surfaces[j].ID })
	sort.Slice(snapshot.Artifacts, func(i, j int) bool { return snapshot.Artifacts[i].ID < snapshot.Artifacts[j].ID })
	sort.Strings(snapshot.Warnings)
}

func (options Options) validate() error {
	if strings.TrimSpace(options.ProjectRoot) == "" {
		return fmt.Errorf("storage inventory project root is required")
	}
	if options.Now.IsZero() {
		return fmt.Errorf("storage inventory time is required")
	}
	if strings.TrimSpace(options.Provider) != options.Provider {
		return fmt.Errorf("storage inventory provider filter has surrounding whitespace")
	}
	return nil
}

func includeProvider(filter, provider string) bool {
	return provider == "" || filter == "" || filter == provider
}
