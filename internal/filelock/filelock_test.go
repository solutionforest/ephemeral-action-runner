package filelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAcquireExcludesAnotherProcess(t *testing.T) {
	if os.Getenv("EPAR_FILELOCK_HELPER") == "1" {
		lock, err := Acquire(os.Getenv("EPAR_FILELOCK_PATH"))
		if errors.Is(err, ErrLocked) {
			os.Exit(23)
		}
		if err != nil {
			os.Exit(24)
		}
		_ = lock.Close()
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "active.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lock.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestAcquireExcludesAnotherProcess$")
	command.Env = append(os.Environ(), "EPAR_FILELOCK_HELPER=1", "EPAR_FILELOCK_PATH="+path)
	err = command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("child result = %v, want ErrLocked exit code", err)
	}
}

func TestSharedLockCompatibilityAcrossProcesses(t *testing.T) {
	if mode := os.Getenv("EPAR_SHARED_LOCK_HELPER"); mode != "" {
		acquire := Acquire
		if mode == "shared" {
			acquire = AcquireShared
		}
		lock, err := acquire(os.Getenv("EPAR_SHARED_LOCK_PATH"))
		if errors.Is(err, ErrLocked) {
			os.Exit(23)
		}
		if err != nil {
			os.Exit(24)
		}
		_ = lock.Close()
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "shared.lock")
	check := func(mode string, want int) {
		t.Helper()
		child := exec.Command(os.Args[0], "-test.run=^TestSharedLockCompatibilityAcrossProcesses$")
		child.Env = append(os.Environ(), "EPAR_SHARED_LOCK_HELPER="+mode, "EPAR_SHARED_LOCK_PATH="+path)
		err := child.Run()
		got := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatal(err)
			}
			got = exitErr.ExitCode()
		}
		if got != want {
			t.Fatalf("%s exit = %d, want %d", mode, got, want)
		}
	}
	first, err := AcquireShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.ReplaceContent([]byte("unsafe")); err == nil {
		t.Fatal("shared lock allowed metadata replacement")
	}
	check("shared", 0)
	check("exclusive", 23)
	_ = first.Close()
	check("exclusive", 23)
	_ = second.Close()
	exclusive, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	check("shared", 23)
	_ = exclusive.Close()
	check("exclusive", 0)
}

func TestCloseAllowsReacquireAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first again: %v", err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire second: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close second: %v", err)
	}
}

func TestReplaceContentUsesHeldLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lock.Close()

	if err := lock.ReplaceContent([]byte("first payload that is longer\n")); err != nil {
		t.Fatalf("ReplaceContent first: %v", err)
	}
	if err := lock.ReplaceContent([]byte("short\n")); err != nil {
		t.Fatalf("ReplaceContent second: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got, want := string(content), "short\n"; got != want {
		t.Fatalf("lock metadata = %q, want %q", got, want)
	}
}
