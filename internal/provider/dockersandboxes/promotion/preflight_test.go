package promotion

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	sandboxpolicy "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/policy"
)

func TestRunPreflightPassesEveryIndependentGateWithExactReadOnlyArgv(t *testing.T) {
	record, outputs := validPreflightFixture(t)
	var commands [][]string
	storageRoot := t.TempDir()
	result := RunPreflight(context.Background(), record, PreflightOptions{
		ProjectRoot:        t.TempDir(),
		StorageRoot:        storageRoot,
		NativeController:   true,
		ControllerRevision: record.EPARRevision,
		RunSBX: func(_ context.Context, args []string) ([]byte, error) {
			commands = append(commands, append([]string(nil), args...))
			output, ok := outputs[strings.Join(args, "\x00")]
			if !ok {
				return nil, fmt.Errorf("unexpected command %#v", args)
			}
			return append([]byte(nil), output...), nil
		},
		InspectTemplate: func(_ context.Context, reference string) (string, error) {
			if reference != record.Template {
				t.Fatalf("inspected template = %q, want %q", reference, record.Template)
			}
			return record.TemplateDigest, nil
		},
		HostSpace: func(path string) (HostSpace, error) {
			if path != storageRoot {
				t.Fatalf("capacity path = %q, want provider storage %q", path, storageRoot)
			}
			required, overflow := requiredHostFreeBytes(record, 500<<30)
			if overflow {
				t.Fatal("valid fixture resource total overflowed")
			}
			return HostSpace{AvailableBytes: required, TotalBytes: 500 << 30}, nil
		},
		CheckVirtualization: func() error { return nil },
	})
	if !result.Passed() {
		t.Fatalf("preflight failures = %+v", result.Failures)
	}
	want := [][]string{
		{"daemon", "status", "--json"},
		{"diagnose", "--output", "json"},
		{"template", "ls", "--json"},
		{"policy", "ls", "--include-inactive", "--json"},
	}
	if !slices.EqualFunc(commands, want, slices.Equal[[]string]) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	for _, args := range commands {
		if len(args) == 0 || args[0] == "tui" || args[0] == "reset" {
			t.Fatalf("unsafe Docker Sandboxes argv invoked: %#v", args)
		}
	}
}

func TestRunPreflightFailsClosedForEveryAdmissionGate(t *testing.T) {
	tests := []struct {
		name string
		gate string
		edit func(*Record, map[string][]byte, *PreflightOptions)
	}{
		{
			name: "unknown controller revision",
			gate: "controller revision",
			edit: func(_ *Record, _ map[string][]byte, opts *PreflightOptions) {
				opts.ControllerRevision = "unknown"
			},
		},
		{
			name: "stale controller revision",
			gate: "controller revision",
			edit: func(_ *Record, _ map[string][]byte, opts *PreflightOptions) {
				opts.ControllerRevision = "sha256:" + strings.Repeat("8", 64)
			},
		},
		{
			name: "native controller",
			gate: "native controller",
			edit: func(_ *Record, _ map[string][]byte, opts *PreflightOptions) { opts.NativeController = false },
		},
		{
			name: "virtualization",
			gate: "virtualization",
			edit: func(_ *Record, _ map[string][]byte, opts *PreflightOptions) {
				opts.CheckVirtualization = func() error { return errors.New("hardware virtualization unavailable") }
			},
		},
		{
			name: "resource availability",
			gate: "resource availability",
			edit: func(_ *Record, _ map[string][]byte, opts *PreflightOptions) {
				opts.HostSpace = func(string) (HostSpace, error) { return HostSpace{AvailableBytes: 1, TotalBytes: 500 << 30}, nil }
			},
		},
		{
			name: "provider storage path",
			gate: "resource availability",
			edit: func(_ *Record, _ map[string][]byte, opts *PreflightOptions) {
				opts.StorageRoot = ""
			},
		},
		{
			name: "daemon",
			gate: "daemon health",
			edit: func(_ *Record, outputs map[string][]byte, _ *PreflightOptions) {
				outputs["daemon\x00status\x00--json"] = []byte(`{"status":"stopped"}`)
			},
		},
		{
			name: "authentication",
			gate: "daemon diagnostics",
			edit: func(_ *Record, outputs map[string][]byte, _ *PreflightOptions) {
				outputs["diagnose\x00--output\x00json"] = []byte(diagnosticsFixture("Authentication", "fail"))
			},
		},
		{
			name: "diagnostics without a pass",
			gate: "daemon diagnostics",
			edit: func(_ *Record, outputs map[string][]byte, _ *PreflightOptions) {
				outputs["diagnose\x00--output\x00json"] = []byte(`{"version":"1.0","checks":[{"name":"Optional integration","status":"skip","message":"","detail":"","hint":""}],"summary":{"pass":0,"warn":0,"fail":0,"skip":1}}`)
			},
		},
		{
			name: "template",
			gate: "promoted template",
			edit: func(_ *Record, outputs map[string][]byte, _ *PreflightOptions) {
				outputs["template\x00ls\x00--json"] = []byte(`{"images":[{"id":"bbbbbbbbbbbb","repository":"docker.io/library/epar-template","tag":"promoted","flavor":"shell","created_at":"2026-07-23T00:00:00Z","size":1024}]}`)
			},
		},
		{
			name: "full template evidence",
			gate: "promoted template evidence",
			edit: func(_ *Record, _ map[string][]byte, opts *PreflightOptions) {
				opts.InspectTemplate = func(context.Context, string) (string, error) {
					return "sha256:" + strings.Repeat("9", 64), nil
				}
			},
		},
		{
			name: "policy",
			gate: "promoted policy",
			edit: func(record *Record, _ map[string][]byte, _ *PreflightOptions) {
				record.PolicyFingerprint = "sha256:" + strings.Repeat("9", 64)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, outputs := validPreflightFixture(t)
			required, overflow := requiredHostFreeBytes(record, 500<<30)
			if overflow {
				t.Fatal("valid fixture resource total overflowed")
			}
			opts := PreflightOptions{
				ProjectRoot:        t.TempDir(),
				StorageRoot:        t.TempDir(),
				NativeController:   true,
				ControllerRevision: record.EPARRevision,
				RunSBX:             fixtureCommandRunner(outputs),
				InspectTemplate: func(context.Context, string) (string, error) {
					return record.TemplateDigest, nil
				},
				HostSpace: func(string) (HostSpace, error) {
					return HostSpace{AvailableBytes: required, TotalBytes: 500 << 30}, nil
				},
				CheckVirtualization: func() error { return nil },
			}
			test.edit(&record, outputs, &opts)
			result := RunPreflight(context.Background(), record, opts)
			if result.Passed() {
				t.Fatal("preflight unexpectedly passed")
			}
			found := false
			for _, failure := range result.Failures {
				if failure.Gate == test.gate && failure.Detail != "" && failure.Resolution != "" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("preflight failures = %+v, want actionable %q failure", result.Failures, test.gate)
			}
		})
	}
}

func TestRunPreflightAcceptsDiagnosticWarningsAndSkips(t *testing.T) {
	for _, status := range []string{"warn", "skip"} {
		t.Run(status, func(t *testing.T) {
			record, outputs := validPreflightFixture(t)
			outputs["diagnose\x00--output\x00json"] = []byte(diagnosticsFixture("Storage directories", status))
			required, _ := requiredHostFreeBytes(record, 500<<30)
			result := RunPreflight(context.Background(), record, PreflightOptions{
				ProjectRoot:        t.TempDir(),
				StorageRoot:        t.TempDir(),
				NativeController:   true,
				ControllerRevision: record.EPARRevision,
				RunSBX:             fixtureCommandRunner(outputs),
				InspectTemplate: func(context.Context, string) (string, error) {
					return record.TemplateDigest, nil
				},
				HostSpace: func(string) (HostSpace, error) {
					return HostSpace{AvailableBytes: required, TotalBytes: 500 << 30}, nil
				},
				CheckVirtualization: func() error { return nil },
			})
			if !result.Passed() {
				t.Fatalf("preflight rejected diagnostic %s: %+v", status, result.Failures)
			}
		})
	}
}

func TestRunPreflightDiagnosticFailureExplainsHowToInspectHints(t *testing.T) {
	record, outputs := validPreflightFixture(t)
	outputs["diagnose\x00--output\x00json"] = []byte(diagnosticsFixture("Daemon", "fail"))
	required, _ := requiredHostFreeBytes(record, 500<<30)
	result := RunPreflight(context.Background(), record, PreflightOptions{
		ProjectRoot:        t.TempDir(),
		StorageRoot:        t.TempDir(),
		NativeController:   true,
		ControllerRevision: record.EPARRevision,
		RunSBX:             fixtureCommandRunner(outputs),
		InspectTemplate: func(context.Context, string) (string, error) {
			return record.TemplateDigest, nil
		},
		HostSpace: func(string) (HostSpace, error) {
			return HostSpace{AvailableBytes: required, TotalBytes: 500 << 30}, nil
		},
		CheckVirtualization: func() error { return nil },
	})
	for _, failure := range result.Failures {
		if failure.Gate == "daemon diagnostics" && strings.Contains(failure.Resolution, "sbx diagnose --output json") && strings.Contains(failure.Resolution, "hints") {
			return
		}
	}
	t.Fatalf("preflight diagnostic failure omitted command and hint remediation: %+v", result.Failures)
}

func TestRunPreflightDoesNotInferVirtualizationFromDiagnostics(t *testing.T) {
	record, outputs := validPreflightFixture(t)
	required, _ := requiredHostFreeBytes(record, 500<<30)
	result := RunPreflight(context.Background(), record, PreflightOptions{
		ProjectRoot:        t.TempDir(),
		StorageRoot:        t.TempDir(),
		NativeController:   true,
		ControllerRevision: record.EPARRevision,
		RunSBX:             fixtureCommandRunner(outputs),
		InspectTemplate: func(context.Context, string) (string, error) {
			return record.TemplateDigest, nil
		},
		HostSpace: func(string) (HostSpace, error) {
			return HostSpace{AvailableBytes: required, TotalBytes: 500 << 30}, nil
		},
		CheckVirtualization: func() error { return errors.New("independent host virtualization proof failed") },
	})
	if result.Passed() {
		t.Fatal("preflight passed using diagnostics that contain no virtualization check")
	}
	for _, failure := range result.Failures {
		if failure.Gate == "virtualization" && strings.Contains(failure.Detail, "independent host") {
			return
		}
	}
	t.Fatalf("preflight failures = %+v, want independent virtualization failure", result.Failures)
}

func TestLocalPreflightRejectsCrossPlatformRecordAndKillSwitchBeforeCommands(t *testing.T) {
	record, _ := validPreflightFixture(t)
	if record.Platform == CurrentPlatform() {
		record.Platform = DarwinARM64
		if record.Platform == CurrentPlatform() {
			record.Platform = LinuxAMD64
		}
	}
	result := LocalPreflight(context.Background(), record, t.TempDir(), true, record.EPARRevision)
	if result.Passed() || len(result.Failures) != 1 || result.Failures[0].Gate != "promoted platform" {
		t.Fatalf("cross-platform preflight result = %+v", result)
	}

	record.Platform = CurrentPlatform()
	t.Setenv(DisableEnvironment, "1")
	result = LocalPreflight(context.Background(), record, t.TempDir(), true, record.EPARRevision)
	if result.Passed() || len(result.Failures) != 1 || result.Failures[0].Gate != "operator kill switch" {
		t.Fatalf("kill-switch preflight result = %+v", result)
	}
}

func fixtureCommandRunner(outputs map[string][]byte) func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, args []string) ([]byte, error) {
		output, ok := outputs[strings.Join(args, "\x00")]
		if !ok {
			return nil, fmt.Errorf("unexpected command %#v", args)
		}
		return append([]byte(nil), output...), nil
	}
}

func validPreflightFixture(t *testing.T) (Record, map[string][]byte) {
	t.Helper()
	policyJSON := []byte(`{"rules":[{"id":"global-1","name":"automatic baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["api.github.com"],"origin":"local","status":"active","editable":true}]}`)
	rules, err := parseGlobalPolicy(policyJSON)
	if err != nil {
		t.Fatal(err)
	}
	policyFingerprint, err := sandboxpolicy.Fingerprint(rules)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	record := Record{
		Platform:                 WindowsAMD64,
		EPARRevision:             digest("1"),
		Template:                 "epar-template:promoted",
		TemplateDigest:           digest("a"),
		TemplateCacheID:          strings.Repeat("a", 12),
		TemplateMetadataDigest:   digest("b"),
		TemplateArchiveDigest:    digest("b"),
		PolicyFingerprint:        policyFingerprint,
		EvidenceDigest:           digest("c"),
		SBOMDigest:               digest("d"),
		ProvenanceDigest:         digest("e"),
		SoftwareInventoryDigest:  digest("f"),
		VerifiedAt:               time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		Verifier:                 "independent-test-verifier",
		Gates:                    GateResults{Local: true, Functional: true, Recovery: true, Security: true, Policy: true, Cleanup: true, SecretScanning: true, ConcurrentProvisioning: true, IndependentSecurityReview: true},
		RootDiskBytes:            120 << 30,
		DockerDiskBytes:          100 << 30,
		MinHostFreeSpaceBytes:    50 << 30,
		ReliabilityJobs:          25,
		ReliabilityDuration:      2 * time.Hour,
		CachedCreateP95:          30 * time.Second,
		QueueToOnlineP95:         90 * time.Second,
		ForceRemoveP95:           60 * time.Second,
		BuildxComposeSlowdownPct: 10,
	}
	outputs := map[string][]byte{
		"daemon\x00status\x00--json":                   []byte(`{"status":"running","socket":"test","logs":"test"}`),
		"diagnose\x00--output\x00json":                 []byte(diagnosticsFixture("", "")),
		"template\x00ls\x00--json":                     []byte(`{"images":[{"id":"aaaaaaaaaaaa","repository":"docker.io/library/epar-template","tag":"promoted","flavor":"shell","created_at":"2026-07-23T00:00:00Z","size":1024}]}`),
		"policy\x00ls\x00--include-inactive\x00--json": policyJSON,
	}
	return record, outputs
}

func diagnosticsFixture(changedCheck, changedStatus string) string {
	names := []string{"CLI binary", "CLI invocation", "Daemon", "Daemon diagnostics", "Runtime compatibility", "Storage directories", "Directory permissions", "Socket", "Authentication"}
	checks := make([]string, 0, len(names))
	passed := 0
	failed := 0
	warned := 0
	skipped := 0
	for _, name := range names {
		status := "pass"
		if name == changedCheck {
			status = changedStatus
		}
		switch status {
		case "pass":
			passed++
		case "fail":
			failed++
		case "warn":
			warned++
		case "skip":
			skipped++
		}
		checks = append(checks, fmt.Sprintf(`{"name":%q,"status":%q,"message":"ok","detail":"","hint":""}`, name, status))
	}
	return fmt.Sprintf(`{"version":"1.0","checks":[%s],"summary":{"pass":%d,"warn":%d,"fail":%d,"skip":%d}}`, strings.Join(checks, ","), passed, warned, failed, skipped)
}
