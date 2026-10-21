package image

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDockerSandboxesPackageLockObservation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the guest lock observer uses Unix device identities")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to execute the guest lock observer tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, filepath.Join("..", "..", "scripts", "test", "docker-sandboxes-package-locks.py"), "-v")
	command.Env = append(command.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("guest package lock checks failed: %v\n%s", err, output)
	}
}
