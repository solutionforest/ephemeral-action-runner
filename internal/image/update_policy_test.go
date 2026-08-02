package image

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func TestNextImageUpdateAtIntervals(t *testing.T) {
	location := time.FixedZone("test", 8*60*60)
	last := time.Date(2026, time.July, 30, 13, 45, 0, 0, location)
	tests := []struct {
		frequency string
		want      time.Time
	}{
		{config.ImageUpdateFrequencyDaily, time.Date(2026, time.July, 31, 7, 0, 0, 0, location)},
		{config.ImageUpdateFrequencyWeekly, time.Date(2026, time.August, 6, 7, 0, 0, 0, location)},
		{config.ImageUpdateFrequencyBiweekly, time.Date(2026, time.August, 13, 7, 0, 0, 0, location)},
		{config.ImageUpdateFrequencyMonthly, time.Date(2026, time.August, 30, 7, 0, 0, 0, location)},
	}
	for _, test := range tests {
		got, err := NextImageUpdateAt(last, test.frequency, "07:00", location)
		if err != nil {
			t.Fatalf("%s: %v", test.frequency, err)
		}
		if !got.Equal(test.want) {
			t.Fatalf("%s next = %s, want %s", test.frequency, got, test.want)
		}
	}
}

func TestNextImageUpdateAtClampsCalendarMonth(t *testing.T) {
	location := time.UTC
	tests := []struct {
		last time.Time
		want time.Time
	}{
		{
			time.Date(2025, time.January, 31, 12, 0, 0, 0, location),
			time.Date(2025, time.February, 28, 7, 0, 0, 0, location),
		},
		{
			time.Date(2024, time.January, 31, 12, 0, 0, 0, location),
			time.Date(2024, time.February, 29, 7, 0, 0, 0, location),
		},
		{
			time.Date(2026, time.December, 31, 12, 0, 0, 0, location),
			time.Date(2027, time.January, 31, 7, 0, 0, 0, location),
		},
	}
	for _, test := range tests {
		got, err := NextImageUpdateAt(test.last, config.ImageUpdateFrequencyMonthly, "07:00", location)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(test.want) {
			t.Fatalf("next = %s, want %s", got, test.want)
		}
	}
}

func TestNextImageUpdateAtManual(t *testing.T) {
	got, err := NextImageUpdateAt(time.Now(), config.ImageUpdateFrequencyManual, "07:00", time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("manual next = %s, want zero", got)
	}
}

func TestUpdateFailureBackoffIsBounded(t *testing.T) {
	now := time.Date(2026, time.July, 30, 7, 0, 0, 0, time.UTC)
	state := UpdatePolicyState{}
	for attempt := 1; attempt <= 8; attempt++ {
		scheduleUpdateFailure(&state, now, errors.New("registry unavailable"))
		delay := state.NextRetryAt.Sub(now)
		if delay > 24*time.Hour {
			t.Fatalf("attempt %d delay = %s, want at most 24h", attempt, delay)
		}
	}
	if state.NextRetryAt.Sub(now) != 24*time.Hour {
		t.Fatalf("final delay = %s, want 24h", state.NextRetryAt.Sub(now))
	}
}

func TestUpdateCheckDueHonorsManualAndRetry(t *testing.T) {
	now := time.Date(2026, time.July, 30, 7, 0, 0, 0, time.UTC)
	image := config.Default().Image
	state := UpdatePolicyState{}
	if !updateCheckDue(state, image, now) {
		t.Fatal("new automatic policy should be due")
	}
	image.UpdateFrequency = config.ImageUpdateFrequencyManual
	if updateCheckDue(state, image, now) {
		t.Fatal("manual policy should not be due")
	}
	image.UpdateFrequency = config.ImageUpdateFrequencyWeekly
	state.NextRetryAt = now.Add(time.Hour)
	if updateCheckDue(state, image, now) {
		t.Fatal("retry backoff should defer check")
	}
}

func TestUpdateCheckDueSkipsImmutableRemoteInputs(t *testing.T) {
	now := time.Date(2026, time.July, 30, 7, 0, 0, 0, time.UTC)
	image := config.Default().Image
	state := UpdatePolicyState{
		LastSuccessfulCheckAt: now.Add(-8 * 24 * time.Hour),
		NextEligibleAt:        now.Add(-24 * time.Hour),
		LastResolvedManifest: &Manifest{
			SourceType:     config.ImageSourceDockerImage,
			SourceImage:    "ghcr.io/example/runner@sha256:" + strings.Repeat("a", 64),
			RunnerSelector: "2.332.0",
		},
	}
	if updateCheckDue(state, image, now) {
		t.Fatal("immutable source and pinned runner should not schedule a remote check")
	}
	state.LastResolvedManifest.RunnerSelector = "latest"
	if !updateCheckDue(state, image, now) {
		t.Fatal("runnerVersion latest should remain scheduled")
	}
}

func TestNextImageUpdateAtHandlesDSTGapAndRepeat(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	gap, err := NextImageUpdateAt(time.Date(2026, time.March, 7, 7, 0, 0, 0, location), config.ImageUpdateFrequencyDaily, "02:30", location)
	if err != nil {
		t.Fatal(err)
	}
	gapLocal := gap.In(location)
	if gapLocal.Year() != 2026 || gapLocal.Month() != time.March || gapLocal.Day() != 8 || gapLocal.Hour() != 3 || gapLocal.Minute() != 0 {
		t.Fatalf("DST gap next = %s, want first valid local instant at 03:00", gapLocal)
	}
	repeat, err := NextImageUpdateAt(time.Date(2026, time.October, 31, 7, 0, 0, 0, location), config.ImageUpdateFrequencyDaily, "01:30", location)
	if err != nil {
		t.Fatal(err)
	}
	repeatLocal := repeat.In(location)
	if repeatLocal.Year() != 2026 || repeatLocal.Month() != time.November || repeatLocal.Day() != 1 || repeatLocal.Hour() != 1 || repeatLocal.Minute() != 30 {
		t.Fatalf("DST repeat next = %s, want local 01:30", repeatLocal)
	}
}

func TestRecalculateScheduleForTimeZone(t *testing.T) {
	singapore := time.FixedZone("Asia/Singapore", 8*60*60)
	tokyo := time.FixedZone("Asia/Tokyo", 9*60*60)
	success := time.Date(2026, time.July, 30, 7, 0, 0, 0, singapore)
	state := UpdatePolicyState{
		LastSuccessfulCheckAt: success.UTC(),
		NextEligibleAt:        success.AddDate(0, 0, 7).UTC(),
		TimeZone:              singapore.String(),
		PolicyFrequency:       config.ImageUpdateFrequencyWeekly,
		PolicyTime:            "07:00",
	}
	image := config.Default().Image
	if !recalculateScheduleForTimeZone(&state, image, tokyo) {
		t.Fatal("timezone change did not recalculate the next check")
	}
	want := time.Date(2026, time.August, 6, 7, 0, 0, 0, tokyo)
	if !state.NextEligibleAt.Equal(want) {
		t.Fatalf("next after timezone change = %s, want %s", state.NextEligibleAt, want)
	}
}

func TestRecalculateScheduleAfterPolicyChange(t *testing.T) {
	location := time.FixedZone("local", 8*60*60)
	success := time.Date(2026, time.July, 30, 7, 0, 0, 0, location)
	state := UpdatePolicyState{
		LastSuccessfulCheckAt: success.UTC(),
		NextEligibleAt:        success.AddDate(0, 0, 7).UTC(),
		TimeZone:              location.String(),
		PolicyFrequency:       config.ImageUpdateFrequencyWeekly,
		PolicyTime:            "07:00",
	}
	image := config.Default().Image
	image.UpdateFrequency = config.ImageUpdateFrequencyDaily
	image.UpdateTime = "06:30"
	if !recalculateScheduleForTimeZone(&state, image, location) {
		t.Fatal("policy change did not recalculate the next check")
	}
	want := time.Date(2026, time.July, 31, 6, 30, 0, 0, location)
	if !state.NextEligibleAt.Equal(want) {
		t.Fatalf("next after policy change = %s, want %s", state.NextEligibleAt, want)
	}
	image.UpdateFrequency = config.ImageUpdateFrequencyManual
	if !recalculateScheduleForTimeZone(&state, image, location) || !state.NextEligibleAt.IsZero() {
		t.Fatalf("manual policy did not clear next automatic check: %+v", state)
	}
}

func TestBootstrapUpdatePolicyStateFromMatchingVerifiedArtifact(t *testing.T) {
	location := time.FixedZone("local", 8*60*60)
	activated := time.Date(2026, time.July, 1, 7, 0, 0, 0, location)
	local := Manifest{
		SchemaVersion:  ManifestSchemaVersion,
		ProviderType:   "docker-sandboxes",
		SourceType:     config.ImageSourceDockerImage,
		SourceImage:    "ghcr.io/catthehacker/ubuntu:act-latest",
		RunnerSelector: "latest",
		EPARScripts:    []FileDigest{{Path: "runtime.sh", SHA256: "local"}},
	}
	resolved := local
	resolved.SourcePlatform = "linux/amd64"
	resolved.SourceDigest = "ghcr.io/catthehacker/ubuntu@sha256:" + strings.Repeat("a", 64)
	resolved.SourcePlatformDigest = "sha256:" + strings.Repeat("b", 64)
	resolved.RunnerVersion = "2.332.0"
	resolved.RunnerAssetName = "actions-runner-linux-x64-2.332.0.tar.gz"
	resolved.RunnerAssetURL = "https://example.invalid/runner.tar.gz"
	resolved.RunnerAssetDigest = "sha256:" + strings.Repeat("c", 64)
	source := ResolvedDockerSource{Reference: local.SourceImage, Platform: "linux/amd64", PlatformDigest: resolved.SourcePlatformDigest}
	state := UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}
	image := config.ImageConfig{UpdateFrequency: config.ImageUpdateFrequencyWeekly, UpdateTime: "07:00"}

	bootstrapped, err := bootstrapUpdatePolicyState(&state, image, local, resolved, &source, activated, location)
	if err != nil {
		t.Fatal(err)
	}
	if !bootstrapped || state.LastResolvedManifest == nil || state.LastResolvedSource == nil {
		t.Fatalf("bootstrap result = %t, state = %+v", bootstrapped, state)
	}
	if got := state.NextEligibleAt.In(location); !got.Equal(activated.AddDate(0, 0, 7)) {
		t.Fatalf("next eligible = %v, want %v", got, activated.AddDate(0, 0, 7))
	}

	changed := local
	changed.EPARScripts = []FileDigest{{Path: "runtime.sh", SHA256: "changed"}}
	rejected := UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}
	bootstrapped, err = bootstrapUpdatePolicyState(&rejected, image, changed, resolved, &source, activated, location)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapped || rejected.LastResolvedManifest != nil {
		t.Fatalf("changed local inputs bootstrapped stale artifact: %+v", rejected)
	}
}

func TestUpdatePolicyStateUsesPerConfigPathAndAtomicJSON(t *testing.T) {
	root := t.TempDir()
	configA := filepath.Join(root, ".local", "a.yml")
	configB := filepath.Join(root, ".local", "b.yml")
	if err := os.MkdirAll(filepath.Dir(configA), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configA, []byte("provider:\n  type: docker-container\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configB, []byte("provider:\n  type: wsl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathA, err := UpdatePolicyStatePath(root, configA)
	if err != nil {
		t.Fatal(err)
	}
	pathB, err := UpdatePolicyStatePath(root, configB)
	if err != nil {
		t.Fatal(err)
	}
	if pathA == pathB {
		t.Fatalf("different configs share update-policy path %q", pathA)
	}
	state := UpdatePolicyState{LocalInputHash: "local", LastError: "registry unavailable"}
	if err := writeUpdatePolicyState(root, configA, state); err != nil {
		t.Fatal(err)
	}
	got, err := ReadUpdatePolicyState(root, configA)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != updatePolicyStateSchemaVersion || got.LocalInputHash != "local" || got.LastError != "registry unavailable" {
		t.Fatalf("read state = %+v", got)
	}
}
