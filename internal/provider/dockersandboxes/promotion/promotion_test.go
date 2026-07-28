package promotion

import (
	"strings"
	"testing"
	"time"
)

func TestNoPlatformIsPromotedWithoutEmbeddedEvidence(t *testing.T) {
	for _, platform := range []Platform{WindowsAMD64, DarwinARM64, LinuxAMD64} {
		if _, promoted := Lookup(platform); promoted {
			t.Fatalf("%s unexpectedly has a Docker Sandboxes promotion record", platform)
		}
	}
}

func TestValidateCompleteRecord(t *testing.T) {
	record := validRecord()
	if err := Validate(record); err != nil {
		t.Fatalf("valid promotion record rejected: %v", err)
	}
}

func TestValidateRejectsEveryNonWaivableGate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Record)
	}{
		{name: "unknown platform", mutate: func(record *Record) { record.Platform = "plan9/amd64" }},
		{name: "wrong sbx", mutate: func(record *Record) { record.SBXVersion = "0.36.0" }},
		{name: "unknown EPAR revision", mutate: func(record *Record) { record.EPARRevision = "unknown" }},
		{name: "wrong template cache ID", mutate: func(record *Record) { record.TemplateCacheID = "bbbbbbbbbbbb" }},
		{name: "unverified", mutate: func(record *Record) { record.Verifier = "" }},
		{name: "weak Docker disk", mutate: func(record *Record) { record.DockerDiskBytes = 99 << 30 }},
		{name: "too few jobs", mutate: func(record *Record) { record.ReliabilityJobs = 24 }},
		{name: "short soak", mutate: func(record *Record) { record.ReliabilityDuration = 119 * time.Minute }},
		{name: "slow create", mutate: func(record *Record) { record.CachedCreateP95 = 61 * time.Second }},
		{name: "slow online", mutate: func(record *Record) { record.QueueToOnlineP95 = 181 * time.Second }},
		{name: "slow remove", mutate: func(record *Record) { record.ForceRemoveP95 = 121 * time.Second }},
		{name: "slow workload", mutate: func(record *Record) { record.BuildxComposeSlowdownPct = 25.01 }},
		{name: "missing artifact digest", mutate: func(record *Record) { record.SBOMDigest = "" }},
		{name: "failed recovery gate", mutate: func(record *Record) { record.Gates.Recovery = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := validRecord()
			test.mutate(&record)
			if err := Validate(record); err == nil {
				t.Fatal("invalid promotion record accepted")
			}
		})
	}
}

func validRecord() Record {
	digest := "sha256:" + strings.Repeat("a", 64)
	return Record{
		Platform:                WindowsAMD64,
		EPARRevision:            digest,
		SBXVersion:              "0.35.0",
		Template:                "epar-docker-sandboxes-catthehacker-full:version",
		TemplateDigest:          digest,
		TemplateCacheID:         strings.Repeat("a", 12),
		TemplateMetadataDigest:  digest,
		TemplateArchiveDigest:   digest,
		PolicyFingerprint:       digest,
		EvidenceDigest:          digest,
		SBOMDigest:              digest,
		ProvenanceDigest:        digest,
		SoftwareInventoryDigest: digest,
		VerifiedAt:              time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		Verifier:                "independent-security-verifier",
		Gates: GateResults{
			Local:                     true,
			Functional:                true,
			Recovery:                  true,
			Security:                  true,
			Policy:                    true,
			Cleanup:                   true,
			SecretScanning:            true,
			ConcurrentProvisioning:    true,
			IndependentSecurityReview: true,
		},
		RootDiskBytes:            120 << 30,
		DockerDiskBytes:          100 << 30,
		MinHostFreeSpaceBytes:    50 << 30,
		ReliabilityJobs:          25,
		ReliabilityDuration:      2 * time.Hour,
		CachedCreateP95:          60 * time.Second,
		QueueToOnlineP95:         180 * time.Second,
		ForceRemoveP95:           120 * time.Second,
		BuildxComposeSlowdownPct: 25,
	}
}
