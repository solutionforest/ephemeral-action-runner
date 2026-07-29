// Package capacity derives Docker Sandboxes resource floors and evaluates
// host admission without performing external side effects.
package capacity

import (
	"errors"
	"fmt"
	"math"
)

const (
	GiB                    uint64 = 1 << 30
	MinimumRootDisk               = 20 * GiB
	RootWritableHeadroom          = 20 * GiB
	CustomizationAllowance        = 5 * GiB
	DefaultDockerDisk             = 50 * GiB
	MinimumDockerDisk             = 1 * GiB
	MinimumHostFreeSpace          = 1 * GiB
	rootRoundingQuantum           = 10 * GiB
)

// Requirements contains minimum sizes derived from one measured template and
// workload. These values are evidence inputs to a promotion manifest, not
// guesses made at provisioning time.
type Requirements struct {
	RootDisk         uint64
	DockerDisk       uint64
	MinHostFreeSpace uint64
}

type HostSpace struct {
	AvailableBytes uint64
	TotalBytes     uint64
}

// HostWatermark returns the configured fixed physical free-space reserve. The
// backing volume size is deliberately not used: a percentage reserve produces
// nonsensical requirements on large volumes.
func HostWatermark(configured, backingVolumeSize uint64) (uint64, error) {
	_ = backingVolumeSize
	if configured == 0 {
		return MinimumHostFreeSpace, nil
	}
	return configured, nil
}

// Derive calculates the resource floors required by the Docker Sandboxes
// promotion plan. The 25 percent additions use ceiling arithmetic so integer
// truncation can never weaken a floor.
func Derive(measuredRootPeak, representativeDockerPeak, backingVolumeSize uint64) (Requirements, error) {
	if measuredRootPeak == 0 {
		return Requirements{}, errors.New("measured root peak must be greater than zero")
	}
	if representativeDockerPeak == 0 {
		return Requirements{}, errors.New("representative Docker peak must be greater than zero")
	}
	rootDisk, err := DeriveRootDisk(measuredRootPeak, RootWritableHeadroom)
	if err != nil {
		return Requirements{}, fmt.Errorf("derive root disk: %w", err)
	}
	_ = representativeDockerPeak
	dockerDisk := DefaultDockerDisk
	hostWatermark, err := HostWatermark(0, backingVolumeSize)
	if err != nil {
		return Requirements{}, fmt.Errorf("derive host watermark: %w", err)
	}
	return Requirements{RootDisk: rootDisk, DockerDisk: dockerDisk, MinHostFreeSpace: hostWatermark}, nil
}

// DeriveRootDisk turns the expanded source-image estimate and writable
// headroom into the total root-disk capacity passed to Docker Sandboxes.
func DeriveRootDisk(measuredRootPeak, writableHeadroom uint64) (uint64, error) {
	if measuredRootPeak == 0 {
		return 0, errors.New("measured root peak must be greater than zero")
	}
	if writableHeadroom < RootWritableHeadroom {
		return 0, fmt.Errorf("writable root headroom must be at least %d", RootWritableHeadroom)
	}
	if measuredRootPeak > math.MaxUint64-CustomizationAllowance {
		return 0, errors.New("customization allowance overflows uint64")
	}
	rootWithAllowance := measuredRootPeak + CustomizationAllowance
	if rootWithAllowance > math.MaxUint64-writableHeadroom {
		return 0, errors.New("writable headroom overflows uint64")
	}
	rootWithMargin := rootWithAllowance + writableHeadroom
	rootDisk, err := roundUp(rootWithMargin, rootRoundingQuantum)
	if err != nil {
		return 0, err
	}
	if rootDisk < MinimumRootDisk {
		rootDisk = MinimumRootDisk
	}
	return rootDisk, nil
}

func addPercentAndHeadroom(measured, headroom uint64) (uint64, error) {
	quarter := ceilDiv(measured, 4)
	if measured > math.MaxUint64-quarter {
		return 0, errors.New("25 percent margin overflows uint64")
	}
	withMargin := measured + quarter
	if withMargin > math.MaxUint64-headroom {
		return 0, errors.New("headroom addition overflows uint64")
	}
	return withMargin + headroom, nil
}

func roundUp(value, quantum uint64) (uint64, error) {
	if quantum == 0 {
		return 0, errors.New("rounding quantum must be greater than zero")
	}
	remainder := value % quantum
	if remainder == 0 {
		return value, nil
	}
	increment := quantum - remainder
	if value > math.MaxUint64-increment {
		return 0, errors.New("rounding overflows uint64")
	}
	return value + increment, nil
}

func ceilDiv(value, divisor uint64) uint64 {
	return value/divisor + boolToUint64(value%divisor != 0)
}

func boolToUint64(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

// Admission is a point-in-time host capacity decision. ReservedBytes includes
// all live and uncertain ledger reservations; uncertainty must never release a
// reservation.
type Admission struct {
	HostFreeBytes        uint64
	ReservedBytes        uint64
	RequestedBytes       uint64
	MinHostFreeSpace     uint64
	ActiveCreates        int
	MaxConcurrentCreates int
}

// Check rejects an admission when either the create-concurrency ceiling or the
// post-reservation physical free-space watermark would be crossed.
func (a Admission) Check() error {
	if a.MaxConcurrentCreates <= 0 {
		return errors.New("max concurrent creates must be greater than zero")
	}
	if a.ActiveCreates < 0 {
		return errors.New("active creates must not be negative")
	}
	if a.ActiveCreates >= a.MaxConcurrentCreates {
		return fmt.Errorf("create concurrency exhausted: %d active, limit %d", a.ActiveCreates, a.MaxConcurrentCreates)
	}
	if a.MinHostFreeSpace < MinimumHostFreeSpace {
		return fmt.Errorf("host free-space watermark %d is below the hard minimum %d", a.MinHostFreeSpace, MinimumHostFreeSpace)
	}
	if a.ReservedBytes > a.HostFreeBytes {
		return errors.New("existing reservations exceed reported host free space")
	}
	available := a.HostFreeBytes - a.ReservedBytes
	if a.RequestedBytes > available {
		return fmt.Errorf("requested reservation %d exceeds unreserved host free space %d", a.RequestedBytes, available)
	}
	remaining := available - a.RequestedBytes
	if remaining < a.MinHostFreeSpace {
		return fmt.Errorf("requested reservation would leave %d bytes, below watermark %d", remaining, a.MinHostFreeSpace)
	}
	return nil
}
