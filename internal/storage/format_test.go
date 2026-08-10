package storage

import (
	"strings"
	"testing"
)

func TestFormatBytes(t *testing.T) {
	tests := map[uint64]string{
		0:                   "0 bytes",
		512:                 "512 bytes",
		1536:                "1.50 KiB",
		140 * GiB:           "140.00 GiB",
		9223372036854775807: "8.00 EiB",
	}
	for value, want := range tests {
		if got := FormatBytes(value); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestCapacityAdmissionError(t *testing.T) {
	surface := Surface{
		Location: "sandbox-data",
		Capacity: Capacity{Known: true, AvailableBytes: 140 * GiB},
	}
	requirement := Requirement{PeakBytes: 130 * GiB, MinimumFreeBytes: 50 * GiB}
	check, err := EvaluateCapacity(Surface{
		ID:       "sandbox",
		Location: surface.Location,
		Capacity: surface.Capacity,
	}, Requirement{
		ID:               "create",
		SurfaceID:        "sandbox",
		PeakBytes:        requirement.PeakBytes,
		MinimumFreeBytes: requirement.MinimumFreeBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := CapacityAdmissionError("initialize the runner", surface, requirement, check, "./start storage prune --provider docker-sandboxes").Error()
	for _, want := range []string{
		"not enough disk space to initialize the runner",
		"Available: 140.00 GiB",
		"Estimated operation growth: 130.00 GiB",
		"Free-space reserve: 50.00 GiB",
		"Required before starting: 180.00 GiB",
		"Additional space needed: 40.00 GiB",
		"./start storage prune --provider docker-sandboxes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "150323855360") {
		t.Fatalf("error exposes raw byte count: %q", got)
	}
}
