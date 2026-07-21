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
