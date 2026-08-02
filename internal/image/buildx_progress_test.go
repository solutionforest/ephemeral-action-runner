package image

import (
	"strings"
	"testing"
	"time"
)

func TestBuildxProgressMonitorFramesStreamsAndAggregatesLayers(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	var reports []BuildxProgressSnapshot
	monitor := newBuildxProgressMonitor(2*time.Second, func() time.Time { return now }, func(snapshot BuildxProgressSnapshot) {
		reports = append(reports, snapshot)
	})
	stdout := monitor.NewStream()
	stderr := monitor.NewStream()
	firstDigest := "sha256:" + strings.Repeat("a", 64)
	secondDigest := "sha256:" + strings.Repeat("b", 64)

	if _, err := stdout.Write([]byte("#12 " + firstDigest + " 1.00G")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("#11 [source 1/1] resolve image config\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.Write([]byte("B / 2.00GB 10.0s\n")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Second)
	if _, err := stderr.Write([]byte("#12 " + secondDigest + " 1.00GB / 1.00GB 13.0s done\n")); err != nil {
		t.Fatal(err)
	}
	stdout.Flush()
	stderr.Flush()

	if len(reports) != 2 {
		t.Fatalf("reports = %d, want 2: %#v", len(reports), reports)
	}
	snapshot := reports[1]
	if snapshot.CurrentBytes != 2_000_000_000 || snapshot.TotalBytes != 3_000_000_000 || snapshot.CompletedLayers != 1 || snapshot.KnownLayers != 2 || snapshot.ActiveStep != 12 {
		t.Fatalf("unexpected aggregate snapshot: %+v", snapshot)
	}
	if got, want := FormatBuildxProgress("Docker Sandboxes template build", snapshot), "Docker Sandboxes template build: 1.9 GiB/2.8 GiB (67%); 1/2 layer downloads complete; BuildKit step #12; elapsed 3s"; got != want {
		t.Fatalf("FormatBuildxProgress = %q, want %q", got, want)
	}
}

func TestBuildxProgressMonitorIgnoresUnrecognizedAndOverflowingOutput(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	reported := false
	monitor := newBuildxProgressMonitor(0, func() time.Time { return now }, func(BuildxProgressSnapshot) {
		reported = true
	})
	stream := monitor.NewStream()
	lines := []string{
		"ordinary command output",
		"#x malformed",
		"#12 sha256:" + strings.Repeat("c", 64) + " 999999999999999999999EB / 1.00GB 1.0s",
	}
	for _, line := range lines {
		if _, err := stream.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if reported {
		t.Fatal("unrecognized Buildx output produced a progress report")
	}
	if snapshot := monitor.Snapshot(); snapshot.ObservedProgress {
		t.Fatalf("unrecognized Buildx output changed snapshot: %+v", snapshot)
	}
}
