package storage

import (
	"math"
	"testing"
	"time"
)

func TestEvaluateCapacity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		capacity     Capacity
		requirement  Requirement
		wantStatus   CapacityStatus
		wantRequired uint64
		wantDeficit  uint64
	}{
		{
			name:         "unknown fails closed",
			capacity:     Capacity{},
			requirement:  Requirement{ID: "full-build", SurfaceID: "host", PeakBytes: 30 * GiB},
			wantStatus:   CapacityUnknown,
			wantRequired: 50 * GiB,
		},
		{
			name:         "insufficient includes deficit",
			capacity:     Capacity{Known: true, AvailableBytes: 49 * GiB, TotalBytes: 100 * GiB, ObservedAt: now},
			requirement:  Requirement{ID: "full-build", SurfaceID: "host", PeakBytes: 30 * GiB},
			wantStatus:   CapacityInsufficient,
			wantRequired: 50 * GiB,
			wantDeficit:  GiB,
		},
		{
			name:         "ready",
			capacity:     Capacity{Known: true, AvailableBytes: 50 * GiB, TotalBytes: 100 * GiB, ObservedAt: now},
			requirement:  Requirement{ID: "full-build", SurfaceID: "host", PeakBytes: 30 * GiB},
			wantStatus:   CapacityReady,
			wantRequired: 50 * GiB,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			check, err := EvaluateCapacity(Surface{ID: "host", Kind: SurfaceHostFilesystem, Capacity: test.capacity}, test.requirement)
			if err != nil {
				t.Fatalf("EvaluateCapacity() error = %v", err)
			}
			if check.Status != test.wantStatus || check.RequiredAvailableBytes != test.wantRequired || check.DeficitBytes != test.wantDeficit {
				t.Fatalf("EvaluateCapacity() = %+v, want status=%s required=%d deficit=%d", check, test.wantStatus, test.wantRequired, test.wantDeficit)
			}
		})
	}
}

func TestEvaluateCapacityRejectsOverflowAndInvalidObservation(t *testing.T) {
	t.Parallel()
	surface := Surface{ID: "host", Capacity: Capacity{Known: true, AvailableBytes: 10, TotalBytes: 5}}
	if _, err := EvaluateCapacity(surface, Requirement{ID: "build", SurfaceID: "host", MinimumFreeBytes: 1}); err == nil {
		t.Fatal("EvaluateCapacity() accepted available bytes greater than total bytes")
	}
	surface.Capacity = Capacity{Known: true, AvailableBytes: math.MaxUint64, TotalBytes: math.MaxUint64}
	if _, err := EvaluateCapacity(surface, Requirement{ID: "build", SurfaceID: "host", PeakBytes: math.MaxUint64, MinimumFreeBytes: 1}); err == nil {
		t.Fatal("EvaluateCapacity() accepted overflowing requirement")
	}
}
