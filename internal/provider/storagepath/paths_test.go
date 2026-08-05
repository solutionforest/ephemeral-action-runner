package storagepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDockerSandboxesRootsUseDocumentedPlatformDefaults(t *testing.T) {
	tests := []struct {
		name        string
		environment Environment
		want        map[string]string
	}{
		{
			name:        "windows",
			environment: Environment{GOOS: "windows", LocalAppData: `C:\Users\runner\AppData\Local`},
			want:        map[string]string{"state": `C:\Users\runner\AppData\Local\DockerSandboxes`},
		},
		{
			name:        "macOS",
			environment: Environment{GOOS: "darwin", HomeDir: "/Users/runner"},
			want:        map[string]string{"state": "/Users/runner/Library/Application Support/com.docker.sandboxes"},
		},
		{
			name:        "Linux",
			environment: Environment{GOOS: "linux", HomeDir: "/home/runner"},
			want: map[string]string{
				"state":  "/home/runner/.local/state/sandboxes",
				"cache":  "/home/runner/.cache/sandboxes",
				"config": "/home/runner/.config/sandboxes",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			roots, err := DockerSandboxesRoots(test.environment)
			if err != nil {
				t.Fatal(err)
			}
			if len(roots) != len(test.want) {
				t.Fatalf("root count = %d, want %d", len(roots), len(test.want))
			}
			for _, root := range roots {
				if got, want := root.Path, test.want[root.ID]; got != want {
					t.Errorf("%s path = %q, want %q", root.ID, got, want)
				}
			}
		})
	}
}

func TestDockerSandboxesRootsHonorAllXDGOverrides(t *testing.T) {
	roots, err := DockerSandboxesRoots(Environment{
		GOOS:          "linux",
		HomeDir:       "/home/runner",
		XDGStateHome:  "/mnt/state",
		XDGCacheHome:  "/mnt/cache",
		XDGConfigHome: "/mnt/config",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"state": "/mnt/state/sandboxes", "cache": "/mnt/cache/sandboxes", "config": "/mnt/config/sandboxes"}
	for _, root := range roots {
		if root.Path != want[root.ID] {
			t.Errorf("%s path = %q, want %q", root.ID, root.Path, want[root.ID])
		}
		if root.Provenance != ProvenanceEnvironment {
			t.Errorf("%s provenance = %q, want environment", root.ID, root.Provenance)
		}
	}
}

func TestDockerSandboxesRootsRejectRelativeXDGOverride(t *testing.T) {
	_, err := DockerSandboxesRoots(Environment{GOOS: "linux", HomeDir: "/home/runner", XDGCacheHome: "relative"})
	if err == nil {
		t.Fatal("relative XDG_CACHE_HOME was accepted")
	}
}

func TestCurrentCapacityPathFollowsRedirectedRoot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(root, "redirect")
	if err := os.Symlink(target, redirect); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolved, err := currentCapacityPath(filepath.Join(redirect, "future", "data"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Clean(want) {
		t.Fatalf("capacity path = %q, want redirected root %q", resolved, want)
	}
}
