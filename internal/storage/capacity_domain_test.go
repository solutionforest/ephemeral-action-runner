package storage

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestResolveOperationPlanAggregatesPhaseOverlapAndReservesOnce(t *testing.T) {
	t.Parallel()
	surfaces := []Surface{
		{ID: "project", Role: StorageRoleProject, Kind: SurfaceHostFilesystem, DomainID: "shared", Path: "/work", Provenance: "test-map", Confidence: "authoritative"},
		{ID: "docker", Role: StorageRoleDockerEngine, Kind: SurfaceDockerEngine, DomainID: "shared", Path: "/docker", Provenance: "test-map", Confidence: "authoritative"},
	}
	domains := []CapacityDomain{{ID: "shared", Kind: SurfaceHostFilesystem, Identity: "filesystem:1", Capacity: Capacity{Known: true, AvailableBytes: 14 * GiB, TotalBytes: 100 * GiB}}}
	operation := OperationPlan{
		ID:               "build",
		MinimumFreeBytes: 2 * GiB,
		Phases: []OperationPhase{
			{ID: "export", Allocations: []Allocation{{ID: "project-export", Role: StorageRoleProject, Bytes: 8 * GiB}, {ID: "docker-export", Role: StorageRoleDockerEngine, Bytes: 1 * GiB}}},
			{ID: "source", Allocations: []Allocation{{ID: "project-source", Role: StorageRoleProject, Bytes: 5 * GiB}, {ID: "docker-source", Role: StorageRoleDockerEngine, Bytes: 7 * GiB}}},
		},
	}
	evaluation, err := EvaluateOperationPlan(operation, surfaces, domains)
	if err != nil {
		t.Fatalf("EvaluateOperationPlan() error = %v", err)
	}
	if len(evaluation.Requirements) != 1 {
		t.Fatalf("requirements = %+v, want one shared-domain requirement", evaluation.Requirements)
	}
	requirement := evaluation.Requirements[0]
	if requirement.PeakBytes != 12*GiB || requirement.MinimumFreeBytes != 2*GiB || requirement.RequiredAvailableBytes != 14*GiB {
		t.Fatalf("shared domain requirement = %+v, want peak=12 GiB reserve=2 GiB required=14 GiB", requirement)
	}
	if len(evaluation.CapacityChecks) != 1 || evaluation.CapacityChecks[0].Status != CapacityReady {
		t.Fatalf("capacity checks = %+v, want one ready check", evaluation.CapacityChecks)
	}
	if got := evaluation.Allocations[0]; got.Role == "" || got.SurfaceID == "" || got.DomainID != "shared" {
		t.Fatalf("resolved allocation omitted diagnostics: %+v", got)
	}
}

func TestResolveOperationPlanKeepsDifferentDomainsIndependent(t *testing.T) {
	t.Parallel()
	surfaces := []Surface{
		{ID: "project", Role: StorageRoleProject, Kind: SurfaceHostFilesystem, DomainID: "host"},
		{ID: "docker", Role: StorageRoleDockerEngine, Kind: SurfaceDockerEngine, DomainID: "engine"},
	}
	domains := []CapacityDomain{
		{ID: "host", Kind: SurfaceHostFilesystem, Capacity: Capacity{Known: true, AvailableBytes: 10 * GiB, TotalBytes: 100 * GiB}},
		{ID: "engine", Kind: SurfaceDockerEngine, Capacity: Capacity{Known: true, AvailableBytes: 10 * GiB, TotalBytes: 100 * GiB}},
	}
	resolved, err := ResolveOperationPlan(OperationPlan{
		ID:               "import",
		MinimumFreeBytes: GiB,
		Phases: []OperationPhase{{ID: "import", Allocations: []Allocation{
			{ID: "archive", Role: StorageRoleProject, Bytes: 3 * GiB},
			{ID: "engine", Role: StorageRoleDockerEngine, Bytes: 4 * GiB},
		}}},
	}, surfaces, domains)
	if err != nil {
		t.Fatalf("ResolveOperationPlan() error = %v", err)
	}
	if len(resolved.Requirements) != 2 {
		t.Fatalf("requirements = %+v, want two physical domains", resolved.Requirements)
	}
	want := map[string]uint64{"engine": 5 * GiB, "host": 4 * GiB}
	for _, requirement := range resolved.Requirements {
		if requirement.RequiredAvailableBytes != want[requirement.DomainID] {
			t.Errorf("domain %q required bytes = %d, want %d", requirement.DomainID, requirement.RequiredAvailableBytes, want[requirement.DomainID])
		}
	}
}

func TestResolveOperationPlanDistinguishesReserveOnlyAllocationFromNoOpPhase(t *testing.T) {
	t.Parallel()
	surfaces := []Surface{{ID: "project", Role: StorageRoleProject, Kind: SurfaceHostFilesystem, DomainID: "host"}}
	domains := []CapacityDomain{{ID: "host", Kind: SurfaceHostFilesystem}}
	reserveOnly, err := ResolveOperationPlan(OperationPlan{
		ID:               "reserve-only",
		MinimumFreeBytes: 3 * GiB,
		Phases:           []OperationPhase{{ID: "preflight", Allocations: []Allocation{{ID: "project", Role: StorageRoleProject}}}},
	}, surfaces, domains)
	if err != nil {
		t.Fatalf("ResolveOperationPlan(reserve-only) error = %v", err)
	}
	if len(reserveOnly.Requirements) != 1 || reserveOnly.Requirements[0].PeakBytes != 0 || reserveOnly.Requirements[0].RequiredAvailableBytes != 3*GiB {
		t.Fatalf("reserve-only requirements = %+v, want zero growth plus 3 GiB reserve", reserveOnly.Requirements)
	}
	noOp, err := ResolveOperationPlan(OperationPlan{ID: "no-op", MinimumFreeBytes: 3 * GiB, Phases: []OperationPhase{{ID: "cached"}}}, surfaces, domains)
	if err != nil {
		t.Fatalf("ResolveOperationPlan(no-op) error = %v", err)
	}
	if len(noOp.Requirements) != 0 {
		t.Fatalf("no-op requirements = %+v, want none", noOp.Requirements)
	}
}

func TestResolveOperationPlanRejectsOverflowUnknownAndDuplicates(t *testing.T) {
	t.Parallel()
	baseSurface := Surface{ID: "project", Role: StorageRoleProject, Kind: SurfaceHostFilesystem, DomainID: "host"}
	baseDomain := CapacityDomain{ID: "host", Kind: SurfaceHostFilesystem, Identity: "filesystem:1"}
	tests := []struct {
		name     string
		plan     OperationPlan
		surfaces []Surface
		domains  []CapacityDomain
		contains string
	}{
		{
			name: "phase sum overflow",
			plan: OperationPlan{ID: "overflow", MinimumFreeBytes: 1, Phases: []OperationPhase{{ID: "phase", Allocations: []Allocation{
				{ID: "a", Role: StorageRoleProject, Bytes: math.MaxUint64},
				{ID: "b", SurfaceID: "project", Bytes: 1},
			}}}},
			surfaces: []Surface{baseSurface}, domains: []CapacityDomain{baseDomain}, contains: "overflows",
		},
		{
			name: "required bytes overflow",
			plan: OperationPlan{ID: "overflow", MinimumFreeBytes: 2, Phases: []OperationPhase{{ID: "phase", Allocations: []Allocation{
				{ID: "a", Role: StorageRoleProject, Bytes: math.MaxUint64 - 1},
			}}}},
			surfaces: []Surface{baseSurface}, domains: []CapacityDomain{baseDomain}, contains: "overflows",
		},
		{
			name:     "unknown domain",
			plan:     OperationPlan{ID: "unknown", Phases: []OperationPhase{{ID: "phase"}}},
			surfaces: []Surface{baseSurface}, contains: "unknown capacity domain",
		},
		{
			name:     "duplicate phase",
			plan:     OperationPlan{ID: "duplicate", Phases: []OperationPhase{{ID: "phase"}, {ID: "phase"}}},
			surfaces: []Surface{baseSurface}, domains: []CapacityDomain{baseDomain}, contains: "duplicate phase ID",
		},
		{
			name: "duplicate allocation",
			plan: OperationPlan{ID: "duplicate", Phases: []OperationPhase{
				{ID: "a", Allocations: []Allocation{{ID: "same", Role: StorageRoleProject}}},
				{ID: "b", Allocations: []Allocation{{ID: "same", Role: StorageRoleProject}}},
			}},
			surfaces: []Surface{baseSurface}, domains: []CapacityDomain{baseDomain}, contains: "duplicate allocation ID",
		},
		{
			name:     "duplicate domain identity",
			plan:     OperationPlan{ID: "duplicate", Phases: []OperationPhase{{ID: "phase"}}},
			surfaces: []Surface{baseSurface}, domains: []CapacityDomain{baseDomain, {ID: "other", Kind: SurfaceHostFilesystem, Identity: baseDomain.Identity}}, contains: "identity",
		},
		{
			name:     "duplicate domain ID",
			plan:     OperationPlan{ID: "duplicate", Phases: []OperationPhase{{ID: "phase"}}},
			surfaces: []Surface{baseSurface}, domains: []CapacityDomain{baseDomain, baseDomain}, contains: "duplicate storage capacity domain ID",
		},
		{
			name:     "duplicate surface ID",
			plan:     OperationPlan{ID: "duplicate", Phases: []OperationPhase{{ID: "phase"}}},
			surfaces: []Surface{baseSurface, baseSurface}, domains: []CapacityDomain{baseDomain}, contains: "duplicate storage surface ID",
		},
		{
			name: "unknown role",
			plan: OperationPlan{ID: "unknown", Phases: []OperationPhase{{ID: "phase", Allocations: []Allocation{
				{ID: "missing", Role: StorageRoleDockerEngine, Bytes: 1},
			}}}},
			surfaces: []Surface{baseSurface}, domains: []CapacityDomain{baseDomain}, contains: "unknown storage role",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveOperationPlan(test.plan, test.surfaces, test.domains)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("ResolveOperationPlan() error = %v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestPreviewSchemaV2HashDeterministicAcrossDomainPlanOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	surfaces := []Surface{
		{ID: "project", Role: StorageRoleProject, Kind: SurfaceHostFilesystem, DomainID: "host", Path: "/project", Provenance: "configured-project-root", Confidence: "authoritative"},
		{ID: "docker", Role: StorageRoleDockerEngine, Kind: SurfaceDockerEngine, DomainID: "engine", Path: "/docker", Provenance: "provider-probe", Confidence: "authoritative"},
	}
	domains := []CapacityDomain{
		{ID: "host", Kind: SurfaceHostFilesystem, Identity: "filesystem:1", Capacity: Capacity{Known: true, AvailableBytes: 20 * GiB, TotalBytes: 100 * GiB, ObservedAt: now}},
		{ID: "engine", Kind: SurfaceDockerEngine, Identity: "engine:1", Capacity: Capacity{Known: true, AvailableBytes: 20 * GiB, TotalBytes: 100 * GiB, ObservedAt: now}},
	}
	operation := OperationPlan{ID: "build", MinimumFreeBytes: GiB, Phases: []OperationPhase{
		{ID: "source", Allocations: []Allocation{{ID: "project", Role: StorageRoleProject, Bytes: 2 * GiB}}},
		{ID: "export", Allocations: []Allocation{{ID: "docker", Role: StorageRoleDockerEngine, Bytes: 3 * GiB}}},
	}}
	first, err := Preview(PreviewRequest{Now: now, Policy: DefaultPolicy(), Surfaces: surfaces, CapacityDomains: domains, OperationPlans: []OperationPlan{operation}})
	if err != nil {
		t.Fatalf("first Preview() error = %v", err)
	}
	operation.Phases[0], operation.Phases[1] = operation.Phases[1], operation.Phases[0]
	second, err := Preview(PreviewRequest{Now: now, Policy: DefaultPolicy(), Surfaces: []Surface{surfaces[1], surfaces[0]}, CapacityDomains: []CapacityDomain{domains[1], domains[0]}, OperationPlans: []OperationPlan{operation}})
	if err != nil {
		t.Fatalf("second Preview() error = %v", err)
	}
	if first.SchemaVersion != 2 || second.SchemaVersion != 2 {
		t.Fatalf("schema versions = %d and %d, want v2", first.SchemaVersion, second.SchemaVersion)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) || first.Hash != second.Hash {
		t.Fatalf("v2 plan/hash is nondeterministic:\n%s\n%s", firstJSON, secondJSON)
	}
	if first.Surfaces[1].Role == "" || first.Surfaces[1].Path == "" || first.Surfaces[1].Provenance == "" || first.Surfaces[1].Confidence == "" {
		t.Fatalf("surface diagnostics were not retained: %+v", first.Surfaces[1])
	}
}
