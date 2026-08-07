package storage

import "fmt"

const (
	KiB uint64 = 1 << 10
	MiB uint64 = 1 << 20
	TiB uint64 = 1 << 40
	PiB uint64 = 1 << 50
	EiB uint64 = 1 << 60
)

var byteUnits = []struct {
	name  string
	bytes uint64
}{
	{name: "EiB", bytes: EiB},
	{name: "PiB", bytes: PiB},
	{name: "TiB", bytes: TiB},
	{name: "GiB", bytes: GiB},
	{name: "MiB", bytes: MiB},
	{name: "KiB", bytes: KiB},
}

// FormatBytes renders a byte count in a compact binary unit suitable for
// operator-facing capacity messages.
func FormatBytes(value uint64) string {
	for _, unit := range byteUnits {
		if value >= unit.bytes {
			return fmt.Sprintf("%.2f %s", float64(value)/float64(unit.bytes), unit.name)
		}
	}
	return fmt.Sprintf("%d bytes", value)
}

// CapacityAdmissionError renders a confirmed insufficient-capacity decision
// in terms an operator can act on without translating byte counts or policy arithmetic.
func CapacityAdmissionError(action string, surface Surface, requirement Requirement, check CapacityCheck, cleanupCommand string) error {
	available := "unknown"
	if surface.Capacity.Known {
		available = FormatBytes(surface.Capacity.AvailableBytes)
	}
	message := fmt.Sprintf(
		"not enough disk space to %s.\n\n"+
			"  Storage location: %s\n"+
			"  Available: %s\n"+
			"  Estimated operation growth: %s\n"+
			"  Free-space reserve: %s\n"+
			"  Required before starting: %s",
		action,
		surface.Location,
		available,
		FormatBytes(requirement.PeakBytes),
		FormatBytes(requirement.MinimumFreeBytes),
		FormatBytes(check.RequiredAvailableBytes),
	)
	if surface.Capacity.Known && check.DeficitBytes > 0 {
		message += fmt.Sprintf("\n  Additional space needed: %s", FormatBytes(check.DeficitBytes))
	}
	if cleanupCommand != "" {
		message += "\n\nReview reclaimable EPAR storage with:\n  " + cleanupCommand
	}
	return fmt.Errorf("%s", message)
}
