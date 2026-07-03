package pool

import (
	"testing"
	"time"
)

func TestRunnerNames(t *testing.T) {
	names := RunnerNames("epar", 2, time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC))
	want := []string{"epar-20260702-030405-001", "epar-20260702-030405-002"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("name %d = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestRunnerName(t *testing.T) {
	got := RunnerName("epar", 12, time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC))
	want := "epar-20260703-010203-012"
	if got != want {
		t.Fatalf("RunnerName() = %q, want %q", got, want)
	}
}

func TestHasPrefix(t *testing.T) {
	if !HasPrefix("epar-test-1", "epar-test") {
		t.Fatal("expected prefix match")
	}
	if HasPrefix("epar-testx-1", "epar-test") {
		t.Fatal("unexpected prefix match")
	}
}
