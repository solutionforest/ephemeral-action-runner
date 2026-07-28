// Package promotion records independently certified Docker Sandboxes
// platform decisions. Wizard default selection is based on local capabilities.
package promotion

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

type Platform string

const (
	WindowsAMD64 Platform = "windows/amd64"
	DarwinARM64  Platform = "darwin/arm64"
	LinuxAMD64   Platform = "linux/amd64"
)

type Record struct {
	Platform                 Platform
	EPARRevision             string
	SBXVersion               string
	Template                 string
	TemplateDigest           string
	TemplateCacheID          string
	TemplateMetadataDigest   string
	TemplateArchiveDigest    string
	PolicyFingerprint        string
	EvidenceDigest           string
	SBOMDigest               string
	ProvenanceDigest         string
	SoftwareInventoryDigest  string
	VerifiedAt               time.Time
	Verifier                 string
	Gates                    GateResults
	RootDiskBytes            uint64
	DockerDiskBytes          uint64
	MinHostFreeSpaceBytes    uint64
	ReliabilityJobs          int
	ReliabilityDuration      time.Duration
	CachedCreateP95          time.Duration
	QueueToOnlineP95         time.Duration
	ForceRemoveP95           time.Duration
	BuildxComposeSlowdownPct float64
}

// GateResults records the non-performance, non-soak promotion gates. Every
// field is non-waivable; the evidence digest binds the detailed transcripts.
type GateResults struct {
	Local                     bool
	Functional                bool
	Recovery                  bool
	Security                  bool
	Policy                    bool
	Cleanup                   bool
	SecretScanning            bool
	ConcurrentProvisioning    bool
	IndependentSecurityReview bool
}

// embeddedRecords deliberately starts empty. A record represents the stronger
// independently reviewed certification for one exact source and artifact
// identity; it is not required for operator-accepted first-run default status.
var embeddedRecords = map[Platform]Record{}

func CurrentPlatform() Platform {
	return Platform(runtime.GOOS + "/" + runtime.GOARCH)
}

func Lookup(platform Platform) (Record, bool) {
	record, ok := embeddedRecords[platform]
	return record, ok
}

func Validate(record Record) error {
	switch record.Platform {
	case WindowsAMD64, DarwinARM64, LinuxAMD64:
	default:
		return fmt.Errorf("unsupported Docker Sandboxes promotion platform %q", record.Platform)
	}
	for key, value := range map[string]string{
		"EPAR revision":      record.EPARRevision,
		"sbx version":        record.SBXVersion,
		"template":           record.Template,
		"template digest":    record.TemplateDigest,
		"template cache ID":  record.TemplateCacheID,
		"template metadata":  record.TemplateMetadataDigest,
		"template archive":   record.TemplateArchiveDigest,
		"policy fingerprint": record.PolicyFingerprint,
		"evidence digest":    record.EvidenceDigest,
		"SBOM digest":        record.SBOMDigest,
		"provenance digest":  record.ProvenanceDigest,
		"inventory digest":   record.SoftwareInventoryDigest,
		"verifier":           record.Verifier,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Docker Sandboxes promotion record %s is required", key)
		}
	}
	if record.SBXVersion != "0.35.0" {
		return fmt.Errorf("Docker Sandboxes promotion record requires sbx version 0.35.0")
	}
	if !validSHA256(record.EPARRevision) {
		return fmt.Errorf("Docker Sandboxes promotion record EPAR revision must be an exact clean source/build sha256 identity")
	}
	for key, digest := range map[string]string{
		"template digest":          record.TemplateDigest,
		"template metadata digest": record.TemplateMetadataDigest,
		"template archive digest":  record.TemplateArchiveDigest,
		"policy fingerprint":       record.PolicyFingerprint,
		"evidence digest":          record.EvidenceDigest,
		"SBOM digest":              record.SBOMDigest,
		"provenance digest":        record.ProvenanceDigest,
		"inventory digest":         record.SoftwareInventoryDigest,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("Docker Sandboxes promotion record %s must be sha256:<64 lowercase hex>", key)
		}
	}
	if !validTemplateCacheID(record.TemplateCacheID) {
		return fmt.Errorf("Docker Sandboxes promotion record template cache ID must be exactly 12 lowercase hexadecimal characters")
	}
	if record.TemplateCacheID != strings.TrimPrefix(record.TemplateDigest, "sha256:")[:12] {
		return fmt.Errorf("Docker Sandboxes promotion record template cache ID must match the first 12 hexadecimal characters of the full template identity")
	}
	if record.VerifiedAt.IsZero() {
		return fmt.Errorf("Docker Sandboxes promotion record verification time is required")
	}
	for gate, passed := range map[string]bool{
		"local":                       record.Gates.Local,
		"functional":                  record.Gates.Functional,
		"recovery":                    record.Gates.Recovery,
		"security":                    record.Gates.Security,
		"policy":                      record.Gates.Policy,
		"cleanup":                     record.Gates.Cleanup,
		"secret scanning":             record.Gates.SecretScanning,
		"concurrent provisioning":     record.Gates.ConcurrentProvisioning,
		"independent security review": record.Gates.IndependentSecurityReview,
	} {
		if !passed {
			return fmt.Errorf("Docker Sandboxes promotion record %s gate did not pass", gate)
		}
	}
	if record.RootDiskBytes == 0 || record.DockerDiskBytes < 100<<30 || record.MinHostFreeSpaceBytes < 50<<30 {
		return fmt.Errorf("Docker Sandboxes promotion record resource floors are incomplete")
	}
	if record.ReliabilityJobs < 25 || record.ReliabilityDuration < 2*time.Hour {
		return fmt.Errorf("Docker Sandboxes promotion record reliability gate is incomplete")
	}
	if record.CachedCreateP95 <= 0 || record.CachedCreateP95 > 60*time.Second {
		return fmt.Errorf("Docker Sandboxes promotion record cached-create p95 failed")
	}
	if record.QueueToOnlineP95 <= 0 || record.QueueToOnlineP95 > 180*time.Second {
		return fmt.Errorf("Docker Sandboxes promotion record queue-to-online p95 failed")
	}
	if record.ForceRemoveP95 <= 0 || record.ForceRemoveP95 > 120*time.Second {
		return fmt.Errorf("Docker Sandboxes promotion record force-remove p95 failed")
	}
	if record.BuildxComposeSlowdownPct < 0 || record.BuildxComposeSlowdownPct > 25 {
		return fmt.Errorf("Docker Sandboxes promotion record Buildx/Compose slowdown failed")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func validTemplateCacheID(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
