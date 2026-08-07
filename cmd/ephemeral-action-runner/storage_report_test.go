package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func TestStorageReportTextShowsUnknownCapacityAndPartialPlan(t *testing.T) {
	report := unknownCapacityStorageReport()
	output := captureStorageStdout(t, func() error {
		printStorageReport("status", report)
		return nil
	})
	for _, want := range []string{
		"Domain unknown-domain",
		"available=unknown",
		"Operation template-build",
		"status=unknown",
		"reason=stat /var/lib/docker: no such file or directory",
		"Warning: Docker Engine capacity is unavailable",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("storage report = %q, want %q", output, want)
		}
	}
	if !strings.Contains(output, "Domain known-domain") || !strings.Contains(output, "available=80.00GiB") {
		t.Errorf("storage report did not retain known domain: %q", output)
	}
}

func TestStorageReportJSONShowsUnknownCapacityAndPartialPlan(t *testing.T) {
	report := unknownCapacityStorageReport()
	output := captureStorageStdout(t, func() error { return writeStorageJSON(report) })
	var decoded storageCommandReport
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Plan.OperationPlans) != 1 || decoded.Plan.OperationPlans[0].ID != "template-build" {
		t.Fatalf("operation plans = %#v", decoded.Plan.OperationPlans)
	}
	if len(decoded.Plan.CapacityDomains) != 2 || decoded.Plan.CapacityDomains[1].Capacity.Known {
		t.Fatalf("capacity domains = %#v", decoded.Plan.CapacityDomains)
	}
	if decoded.Plan.CapacityDomains[1].CapacityUnavailableReason != "stat /var/lib/docker: no such file or directory" {
		t.Fatalf("unknown domain reason = %q", decoded.Plan.CapacityDomains[1].CapacityUnavailableReason)
	}
	if len(decoded.Plan.CapacityChecks) != 1 || decoded.Plan.CapacityChecks[0].Status != storage.CapacityUnknown {
		t.Fatalf("capacity checks = %#v", decoded.Plan.CapacityChecks)
	}
	if decoded.Plan.CapacityChecks[0].Reason != "stat /var/lib/docker: no such file or directory" {
		t.Fatalf("unknown check reason = %q", decoded.Plan.CapacityChecks[0].Reason)
	}
	if len(decoded.Inventory.Warnings) != 1 {
		t.Fatalf("warnings = %#v", decoded.Inventory.Warnings)
	}
}

func unknownCapacityStorageReport() storageCommandReport {
	domainRequirement := &storage.DomainRequirement{OperationID: "template-build", DomainID: "unknown-domain", PeakBytes: 10 * storage.GiB, MinimumFreeBytes: storage.GiB, RequiredAvailableBytes: 11 * storage.GiB}
	return storageCommandReport{
		Inventory: storageInventorySummary{Warnings: []string{"Docker Engine capacity is unavailable: stat /var/lib/docker: no such file or directory"}},
		Plan: storage.Plan{
			SchemaVersion: 2,
			CapacityDomains: []storage.CapacityDomain{
				{ID: "known-domain", Kind: storage.SurfaceHostFilesystem, Path: "/project", Capacity: storage.Capacity{Known: true, TotalBytes: 100 * storage.GiB, AvailableBytes: 80 * storage.GiB}},
				{ID: "unknown-domain", Kind: storage.SurfaceDockerEngine, Path: "/var/lib/docker", CapacityUnavailableReason: "stat /var/lib/docker: no such file or directory", Capacity: storage.Capacity{}},
			},
			OperationPlans: []storage.OperationPlan{{ID: "template-build", Provider: "docker-sandboxes", MinimumFreeBytes: storage.GiB, Phases: []storage.OperationPhase{{ID: "build"}}}},
			CapacityChecks: []storage.CapacityCheck{{
				Requirement:            storage.Requirement{ID: "template-build-unknown-domain", SurfaceID: "unknown-domain", PeakBytes: 10 * storage.GiB, MinimumFreeBytes: storage.GiB},
				DomainRequirement:      domainRequirement,
				Capacity:               storage.Capacity{},
				Status:                 storage.CapacityUnknown,
				RequiredAvailableBytes: 11 * storage.GiB,
				Reason:                 "stat /var/lib/docker: no such file or directory",
			}},
		},
	}
}

func captureStorageStdout(t *testing.T, run func() error) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = write
	runErr := run()
	os.Stdout = previous
	if closeErr := write.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	output, readErr := io.ReadAll(read)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr := read.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	return string(output)
}
