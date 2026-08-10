package storage

import "time"

const (
	GiB uint64 = 1 << 30

	DefaultMinimumFreeBytes = 1 * GiB
	DefaultGracePeriod      = 168 * time.Hour
	DefaultKeepPrevious     = 0
	DefaultBuildKitMaxBytes = 20 * GiB
	DefaultGoCacheMaxBytes  = 10 * GiB
)

// SurfaceKind identifies a logical storage backend. CapacityDomain carries the
// physical identity used for admission and reclaim accounting.
type SurfaceKind string

const (
	SurfaceHostFilesystem SurfaceKind = "host-filesystem"
	SurfaceDockerEngine   SurfaceKind = "docker-engine"
	SurfaceSandboxCache   SurfaceKind = "sandbox-template-cache"
	SurfaceExternal       SurfaceKind = "external"
)

// StorageRole is a provider-neutral purpose for storage consumed by an
// operation. Providers bind roles to concrete surfaces; operation planners do
// not need to know platform paths or capacity-domain identities.
type StorageRole string

const (
	StorageRoleProject              StorageRole = "project"
	StorageRoleDockerEngine         StorageRole = "docker-engine"
	StorageRoleSandboxTemplateCache StorageRole = "sandbox-template-cache"
	StorageRoleSandboxRuntime       StorageRole = "sandbox-runtime"
	StorageRoleContainerdStore      StorageRole = "containerd-store"
	StorageRoleWSLDistribution      StorageRole = "wsl-distribution"
	StorageRoleTartStore            StorageRole = "tart-store"
)

// Capacity is one capacity observation. Unknown capacity is a first-class
// state, not zero available bytes.
type Capacity struct {
	Known          bool      `json:"known"`
	AvailableBytes uint64    `json:"availableBytes,omitempty"`
	TotalBytes     uint64    `json:"totalBytes,omitempty"`
	ObservedAt     time.Time `json:"observedAt,omitempty"`
}

// Surface identifies one logical storage purpose and its diagnostic evidence.
type Surface struct {
	ID                     string      `json:"id"`
	Provider               string      `json:"provider,omitempty"`
	Role                   StorageRole `json:"role,omitempty"`
	Kind                   SurfaceKind `json:"kind"`
	DomainID               string      `json:"domainId,omitempty"`
	Path                   string      `json:"path,omitempty"`
	Location               string      `json:"location,omitempty"`
	Classification         string      `json:"classification,omitempty"`
	Provenance             string      `json:"provenance,omitempty"`
	Sparse                 bool        `json:"sparse,omitempty"`
	VirtualMaximumBytes    uint64      `json:"virtualMaximumBytes,omitempty"`
	AllocatedBytes         uint64      `json:"allocatedBytes,omitempty"`
	Confidence             string      `json:"confidence,omitempty"`
	AdmissionAuthoritative bool        `json:"admissionAuthoritative"`
	Advisory               bool        `json:"advisory,omitempty"`
	Capacity               Capacity    `json:"capacity"`
}

// CapacityDomain identifies one physical pool of available bytes. Multiple
// logical surfaces may resolve to the same domain and must then share one
// capacity check and one free-space reserve.
type CapacityDomain struct {
	ID                        string      `json:"id"`
	Kind                      SurfaceKind `json:"kind"`
	Identity                  string      `json:"identity,omitempty"`
	Path                      string      `json:"path,omitempty"`
	Provenance                string      `json:"provenance,omitempty"`
	Confidence                string      `json:"confidence,omitempty"`
	CapacityUnavailableReason string      `json:"capacityUnavailableReason,omitempty"`
	Capacity                  Capacity    `json:"capacity"`
}

// OperationPlan describes storage growth as overlapping phases. The reserve
// defaults to DefaultMinimumFreeBytes when it is zero.
type OperationPlan struct {
	ID               string           `json:"id"`
	Provider         string           `json:"provider,omitempty"`
	MinimumFreeBytes uint64           `json:"minimumFreeBytes"`
	Phases           []OperationPhase `json:"phases"`
}

// OperationPhase is one interval during which all of its allocations overlap.
// Different phases do not overlap for peak-capacity calculation.
type OperationPhase struct {
	ID          string       `json:"id"`
	Allocations []Allocation `json:"allocations,omitempty"`
}

// Allocation is additional capacity consumed by one role during a phase.
// SurfaceID is an optional compatibility/direct-binding override; new
// provider-neutral plans normally set Role and leave SurfaceID empty.
type Allocation struct {
	ID        string      `json:"id"`
	Role      StorageRole `json:"role,omitempty"`
	SurfaceID string      `json:"surfaceId,omitempty"`
	Bytes     uint64      `json:"bytes"`
}

// ResolvedAllocation records the exact surface and capacity domain selected
// for one logical phase allocation.
type ResolvedAllocation struct {
	OperationID  string      `json:"operationId"`
	PhaseID      string      `json:"phaseId"`
	AllocationID string      `json:"allocationId"`
	Role         StorageRole `json:"role,omitempty"`
	SurfaceID    string      `json:"surfaceId"`
	DomainID     string      `json:"domainId"`
	Bytes        uint64      `json:"bytes"`
}

// DomainRequirement is the peak growth plus one reserve required from one
// physical capacity domain for one operation.
type DomainRequirement struct {
	OperationID            string `json:"operationId"`
	DomainID               string `json:"domainId"`
	PeakBytes              uint64 `json:"peakBytes"`
	MinimumFreeBytes       uint64 `json:"minimumFreeBytes"`
	RequiredAvailableBytes uint64 `json:"requiredAvailableBytes"`
}

// ResolvedOperationPlan is a normalized plan together with its exact physical
// allocation bindings and per-domain peak requirements.
type ResolvedOperationPlan struct {
	Plan         OperationPlan        `json:"plan"`
	Allocations  []ResolvedAllocation `json:"allocations,omitempty"`
	Requirements []DomainRequirement  `json:"requirements,omitempty"`
}

// OperationEvaluation adds capacity checks to a resolved plan.
type OperationEvaluation struct {
	ResolvedOperationPlan
	CapacityChecks []CapacityCheck `json:"capacityChecks,omitempty"`
}

// Requirement is the peak additional capacity needed by an operation on one
// surface. MinimumFreeBytes defaults to DefaultMinimumFreeBytes when zero.
type Requirement struct {
	ID               string `json:"id"`
	Provider         string `json:"provider,omitempty"`
	SurfaceID        string `json:"surfaceId"`
	PeakBytes        uint64 `json:"peakBytes"`
	MinimumFreeBytes uint64 `json:"minimumFreeBytes"`
}

// CapacityStatus is the result of a capacity preflight.
type CapacityStatus string

const (
	CapacityReady        CapacityStatus = "ready"
	CapacityInsufficient CapacityStatus = "insufficient"
	CapacityUnknown      CapacityStatus = "unknown"
)

// CapacityCheck binds a requirement to the exact capacity observation used to
// evaluate it.
type CapacityCheck struct {
	Requirement            Requirement        `json:"requirement"`
	DomainRequirement      *DomainRequirement `json:"domainRequirement,omitempty"`
	Capacity               Capacity           `json:"capacity"`
	Status                 CapacityStatus     `json:"status"`
	RequiredAvailableBytes uint64             `json:"requiredAvailableBytes"`
	DeficitBytes           uint64             `json:"deficitBytes,omitempty"`
	Reason                 string             `json:"reason"`
}

// ArtifactKind selects the retention policy applicable to an artifact.
type ArtifactKind string

const (
	ArtifactNativeControllerRevision ArtifactKind = "native-controller-revision"
	ArtifactTemplateArchive          ArtifactKind = "template-archive"
	ArtifactDockerImage              ArtifactKind = "docker-image"
	ArtifactSandboxTemplate          ArtifactKind = "sandbox-template"
	ArtifactDockerVolume             ArtifactKind = "docker-volume"
	ArtifactBuildKitCache            ArtifactKind = "buildkit-cache"
	ArtifactGoCache                  ArtifactKind = "go-cache"
	ArtifactProviderImage            ArtifactKind = "provider-image"
	ArtifactProviderCache            ArtifactKind = "provider-cache"
	ArtifactOther                    ArtifactKind = "other"
)

// TargetKind describes the one exact object an executor may receive.
type TargetKind string

const (
	TargetFile            TargetKind = "file"
	TargetDirectory       TargetKind = "directory"
	TargetDockerImageTag  TargetKind = "docker-image-tag"
	TargetDockerVolume    TargetKind = "docker-volume"
	TargetBuildKitRecord  TargetKind = "buildkit-record"
	TargetSandboxTemplate TargetKind = "sandbox-template"
	TargetExternal        TargetKind = "external"
)

// MatchKind records whether a locator identifies one object or a broad set.
type MatchKind string

const (
	MatchExact   MatchKind = "exact"
	MatchPrefix  MatchKind = "prefix"
	MatchUnknown MatchKind = "unknown"
)

// Target binds a locator to an immutable object identity. Fingerprint may bind
// additional mutable metadata, such as a filesystem object's size and mtime.
type Target struct {
	Kind        TargetKind `json:"kind"`
	Locator     string     `json:"locator"`
	Identity    string     `json:"identity,omitempty"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	Match       MatchKind  `json:"match"`
}

// OwnershipKind expresses how confidently an artifact belongs exclusively to
// this EPAR installation.
type OwnershipKind string

const (
	OwnershipExact   OwnershipKind = "exact"
	OwnershipShared  OwnershipKind = "shared"
	OwnershipUnknown OwnershipKind = "unknown"
)

// Ownership carries the exact owner identity and its evidence. Names or
// prefixes alone are not exact ownership evidence.
type Ownership struct {
	Kind     OwnershipKind `json:"kind"`
	OwnerID  string        `json:"ownerId,omitempty"`
	Evidence string        `json:"evidence,omitempty"`
}

// ProtectionKind identifies a reason an artifact must not be selected.
type ProtectionKind string

const (
	ProtectionActive        ProtectionKind = "active"
	ProtectionCurrent       ProtectionKind = "current"
	ProtectionLease         ProtectionKind = "lease"
	ProtectionConfiguration ProtectionKind = "configuration"
	ProtectionLock          ProtectionKind = "source-lock"
	ProtectionPromotion     ProtectionKind = "promotion"
	ProtectionCertification ProtectionKind = "certification"
	ProtectionCustomRoot    ProtectionKind = "custom-root"
	ProtectionOperator      ProtectionKind = "operator"
	ProtectionUncertain     ProtectionKind = "uncertain"
)

// Protection is adapter-supplied evidence that an artifact must be retained.
type Protection struct {
	Kind   ProtectionKind `json:"kind"`
	Detail string         `json:"detail,omitempty"`
}

// Lease protects an artifact through ExpiresAt. A missing ID or expiry is
// treated as uncertain and remains protected.
type Lease struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Artifact is an evidence-bearing snapshot of one storage object.
type Artifact struct {
	ID             string       `json:"id"`
	Provider       string       `json:"provider,omitempty"`
	SurfaceID      string       `json:"surfaceId"`
	Kind           ArtifactKind `json:"kind"`
	RetentionGroup string       `json:"retentionGroup,omitempty"`
	Target         Target       `json:"target"`
	Ownership      Ownership    `json:"ownership"`
	SizeBytes      uint64       `json:"sizeBytes"`
	CreatedAt      time.Time    `json:"createdAt,omitempty"`
	LastUsedAt     time.Time    `json:"lastUsedAt,omitempty"`
	SupersededAt   *time.Time   `json:"supersededAt,omitempty"`
	Current        bool         `json:"current,omitempty"`
	Active         bool         `json:"active,omitempty"`
	Lease          *Lease       `json:"lease,omitempty"`
	Protections    []Protection `json:"protections,omitempty"`
	BackendID      string       `json:"backendId,omitempty"`
	Custody        string       `json:"custody,omitempty"`
	LifecycleState string       `json:"lifecycleState,omitempty"`
	ConfigRefs     []string     `json:"configReferences,omitempty"`
	CleanupError   string       `json:"cleanupError,omitempty"`
}

// Budget bounds one automatically managed artifact kind.
type Budget struct {
	Kind     ArtifactKind `json:"kind"`
	MaxBytes uint64       `json:"maxBytes"`
}

// Policy is the complete deterministic retention policy embedded into a plan.
type Policy struct {
	GracePeriod  time.Duration `json:"gracePeriod"`
	KeepPrevious int           `json:"keepPrevious"`
	Budgets      []Budget      `json:"budgets"`
}

// DefaultPolicy returns the approved conservative automatic defaults.
func DefaultPolicy() Policy {
	return Policy{
		GracePeriod:  DefaultGracePeriod,
		KeepPrevious: DefaultKeepPrevious,
		Budgets: []Budget{
			{Kind: ArtifactBuildKitCache, MaxBytes: DefaultBuildKitMaxBytes},
			{Kind: ArtifactGoCache, MaxBytes: DefaultGoCacheMaxBytes},
		},
	}
}

// Action is the deterministic classification for one artifact.
type Action string

const (
	ActionKeep       Action = "keep"
	ActionProtected  Action = "protected"
	ActionReportOnly Action = "report-only"
	ActionRemove     Action = "remove"
)

// Decision is one artifact's retention outcome and evidence.
type Decision struct {
	Artifact Artifact `json:"artifact"`
	Action   Action   `json:"action"`
	Reasons  []string `json:"reasons"`
}

// PreviewRequest supplies a complete storage snapshot and an explicit clock.
type PreviewRequest struct {
	Now             time.Time        `json:"now"`
	Policy          Policy           `json:"policy"`
	Surfaces        []Surface        `json:"surfaces"`
	CapacityDomains []CapacityDomain `json:"capacityDomains,omitempty"`
	OperationPlans  []OperationPlan  `json:"operationPlans,omitempty"`
	Requirements    []Requirement    `json:"requirements,omitempty"`
	Artifacts       []Artifact       `json:"artifacts,omitempty"`
}

// Plan is an immutable, deterministic preview. Hash is the SHA-256 of the plan
// with the Hash field empty.
type Plan struct {
	SchemaVersion       int                  `json:"schemaVersion"`
	CreatedAt           time.Time            `json:"createdAt"`
	Policy              Policy               `json:"policy"`
	Surfaces            []Surface            `json:"surfaces"`
	CapacityDomains     []CapacityDomain     `json:"capacityDomains,omitempty"`
	OperationPlans      []OperationPlan      `json:"operationPlans,omitempty"`
	ResolvedAllocations []ResolvedAllocation `json:"resolvedAllocations,omitempty"`
	DomainRequirements  []DomainRequirement  `json:"domainRequirements,omitempty"`
	CapacityChecks      []CapacityCheck      `json:"capacityChecks,omitempty"`
	Decisions           []Decision           `json:"decisions,omitempty"`
	RemovalCount        int                  `json:"removalCount"`
	ReclaimableBytes    uint64               `json:"reclaimableBytes"`
	Warnings            []string             `json:"warnings,omitempty"`
	Hash                string               `json:"hash"`
}
