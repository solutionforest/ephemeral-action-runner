package capacity

import (
	"math"
	"testing"
)

func TestDerivePromotionRequirementsFromMeasuredGuestUsage(t *testing.T) {
	requirements, err := Derive(72*GiB, 80*GiB, 2_000*GiB)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := requirements.RootDisk, 100*GiB; got != want {
		t.Fatalf("root disk = %d, want %d", got, want)
	}
	if got, want := requirements.DockerDisk, DefaultDockerDisk; got != want {
		t.Fatalf("Docker disk = %d, want %d", got, want)
	}
	if got, want := requirements.MinHostFreeSpace, MinimumHostFreeSpace; got != want {
		t.Fatalf("host watermark = %d, want %d", got, want)
	}
}

func TestDeriveRootDiskSeparatesMeasuredPeakFromHeadroom(t *testing.T) {
	got, err := DeriveRootDisk(324_780_032, RootWritableHeadroom)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(30 * GiB); got != want {
		t.Fatalf("root disk = %d, want %d", got, want)
	}
	for _, test := range []struct {
		name     string
		peak     uint64
		headroom uint64
	}{
		{name: "missing peak", headroom: RootWritableHeadroom},
		{name: "headroom below preview floor", peak: GiB, headroom: GiB},
		{name: "overflow", peak: math.MaxUint64, headroom: RootWritableHeadroom},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DeriveRootDisk(test.peak, test.headroom); err == nil {
				t.Fatal("DeriveRootDisk accepted invalid input")
			}
		})
	}
}

func TestDeriveEnforcesAbsoluteFloors(t *testing.T) {
	requirements, err := Derive(1*GiB, 1*GiB, 100*GiB)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := requirements.RootDisk, 30*GiB; got != want {
		t.Fatalf("root disk = %d, want %d", got, want)
	}
	if got, want := requirements.DockerDisk, DefaultDockerDisk; got != want {
		t.Fatalf("Docker disk = %d, want %d", got, want)
	}
	if got, want := requirements.MinHostFreeSpace, MinimumHostFreeSpace; got != want {
		t.Fatalf("host watermark = %d, want %d", got, want)
	}
}

func TestDeriveRejectsMissingAndOverflowingEvidence(t *testing.T) {
	for _, test := range []struct {
		name                 string
		template, peak, disk uint64
	}{
		{name: "template", template: 0, peak: GiB, disk: GiB},
		{name: "peak", template: GiB, peak: 0, disk: GiB},
		{name: "overflow", template: math.MaxUint64, peak: GiB, disk: GiB},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Derive(test.template, test.peak, test.disk); err == nil {
				t.Fatal("Derive accepted invalid evidence")
			}
		})
	}
}

func TestHostWatermarkUsesFixedConfiguredReserve(t *testing.T) {
	tests := []struct {
		name       string
		configured uint64
		volume     uint64
		want       uint64
	}{
		{name: "two terabyte volume uses fixed default", volume: 2_000 * GiB, want: 1 * GiB},
		{name: "twenty terabyte volume uses fixed default", volume: 20_000 * GiB, want: 1 * GiB},
		{name: "configured strengthening", configured: 250 * GiB, volume: 2_000 * GiB, want: 250 * GiB},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := HostWatermark(test.configured, test.volume)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("watermark = %d, want %d", got, test.want)
			}
		})
	}
	if got, err := HostWatermark(0, 0); err != nil || got != MinimumHostFreeSpace {
		t.Fatalf("HostWatermark(0, 0) = %d, %v; want fixed default", got, err)
	}
}

func TestAdmissionAccountsForReservationsAndWatermark(t *testing.T) {
	valid := Admission{
		HostFreeBytes:        500 * GiB,
		ReservedBytes:        100 * GiB,
		RequestedBytes:       120 * GiB,
		MinHostFreeSpace:     200 * GiB,
		ActiveCreates:        1,
		MaxConcurrentCreates: 2,
	}
	if err := valid.Check(); err != nil {
		t.Fatalf("valid admission rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Admission)
	}{
		{name: "concurrency", mutate: func(a *Admission) { a.ActiveCreates = 2 }},
		{name: "weak watermark", mutate: func(a *Admission) { a.MinHostFreeSpace = 0 }},
		{name: "uncertain reservations", mutate: func(a *Admission) { a.ReservedBytes = 450 * GiB }},
		{name: "post-reservation watermark", mutate: func(a *Admission) { a.RequestedBytes = 250 * GiB }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := candidate.Check(); err == nil {
				t.Fatal("admission unexpectedly passed")
			}
		})
	}
}
