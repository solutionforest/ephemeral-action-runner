package storagepath

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeFileInfo struct {
	name string
	dir  bool
}

func TestDiscoverDockerDesktopUsesObservedMovedDiskDirectory(t *testing.T) {
	environment := Environment{GOOS: "windows", AppData: `C:\Users\runner\AppData\Roaming`, LocalAppData: `C:\Users\runner\AppData\Local`}
	settingsPath, _, err := desktopPaths(environment)
	if err != nil {
		t.Fatal(err)
	}
	movedDirectory := `D:\Docker Desktop\wsl`
	movedDisk := `D:\Docker Desktop\wsl\disk\docker_data.vhdx`
	storage, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: environment,
		Endpoint:    "npipe:////./pipe/dockerDesktopLinuxEngine",
		Info:        DockerInfo{OperatingSystem: "Docker Desktop"},
		ReadFile: func(path string) ([]byte, error) {
			if path != settingsPath {
				t.Fatalf("settings path = %q, want %q", path, settingsPath)
			}
			return []byte(`{"diskImageLocation":"D:\\Docker Desktop\\wsl"}`), nil
		},
		Stat: func(path string) (os.FileInfo, error) {
			if path == movedDirectory {
				return fakeFileInfo{name: "wsl", dir: true}, nil
			}
			if path == movedDisk {
				return fakeFileInfo{name: "docker_data.vhdx"}, nil
			}
			return nil, os.ErrNotExist
		},
		CapacityPath: func(path string) (string, error) {
			if path != movedDisk {
				t.Fatalf("capacity path input = %q, want %q", path, movedDisk)
			}
			return `D:\Docker Desktop\wsl\disk`, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := storage.Roots[0]
	if root.Path != movedDisk || root.CapacityPath != `D:\Docker Desktop\wsl\disk` || root.Provenance != ProvenanceDocumentedSettings {
		t.Fatalf("directory-backed observed root = %#v", root)
	}
}

func (info fakeFileInfo) Name() string       { return info.name }
func (info fakeFileInfo) Size() int64        { return 1 }
func (info fakeFileInfo) Mode() os.FileMode  { return 0 }
func (info fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (info fakeFileInfo) IsDir() bool        { return info.dir }
func (info fakeFileInfo) Sys() any           { return nil }

func TestClassifyDockerEndpoint(t *testing.T) {
	for _, endpoint := range []string{"unix:///var/run/docker.sock", "npipe:////./pipe/docker_engine", "fd://3"} {
		classification, err := ClassifyDockerEndpoint(endpoint)
		if err != nil || classification != DockerEndpointLocal {
			t.Errorf("endpoint %q classification = %q, err = %v", endpoint, classification, err)
		}
	}
	for _, endpoint := range []string{"ssh://builder.example", "tcp://10.0.0.2:2376", "https://engine.example"} {
		classification, err := ClassifyDockerEndpoint(endpoint)
		if err != nil || classification != DockerEndpointRemote {
			t.Errorf("endpoint %q classification = %q, err = %v", endpoint, classification, err)
		}
	}
}

func TestDiscoverDockerStorageRejectsRemoteContext(t *testing.T) {
	_, err := DiscoverDockerStorage(DockerDiscoveryOptions{Endpoint: "ssh://builder.example"})
	if err == nil || !errors.Is(err, ErrRemoteDockerEndpoint) {
		t.Fatalf("remote discovery error = %v", err)
	}
}

func TestClassifyDockerEndpointRejectsUnsupportedTransport(t *testing.T) {
	_, err := ClassifyDockerEndpoint("orb://local")
	if err == nil || !errors.Is(err, ErrUnsupportedDockerTransport) {
		t.Fatalf("unsupported transport error = %v", err)
	}
}

func TestDiscoverDockerDesktopUsesNestedObservedMovedDisk(t *testing.T) {
	environment := Environment{GOOS: "windows", AppData: `C:\Users\runner\AppData\Roaming`, LocalAppData: `C:\Users\runner\AppData\Local`}
	settingsPath, _, err := desktopPaths(environment)
	if err != nil {
		t.Fatal(err)
	}
	moved := `D:\Docker Desktop\data\docker_data.vhdx`
	storage, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: environment,
		Endpoint:    "npipe:////./pipe/dockerDesktopLinuxEngine",
		Info:        DockerInfo{OperatingSystem: "Docker Desktop"},
		ReadFile: func(path string) ([]byte, error) {
			if path != settingsPath {
				t.Fatalf("settings path = %q, want %q", path, settingsPath)
			}
			return []byte(`{"resources":{"advanced":{"disk":{"location":"D:\\Docker Desktop\\data\\docker_data.vhdx"}}}}`), nil
		},
		Stat: func(path string) (os.FileInfo, error) {
			if path == moved {
				return fakeFileInfo{name: "docker_data.vhdx"}, nil
			}
			return nil, os.ErrNotExist
		},
		CapacityPath: func(path string) (string, error) { return `D:\Docker Desktop\data`, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	root := storage.Roots[0]
	if root.Path != moved || root.CapacityPath != `D:\Docker Desktop\data` {
		t.Fatalf("observed root = %#v", root)
	}
	if root.Provenance != ProvenanceDocumentedSettings || root.Confidence != ConfidenceObserved {
		t.Fatalf("observed evidence = %#v", root)
	}
}

func TestDiscoverDockerDesktopRejectsAmbiguousObservedDisks(t *testing.T) {
	environment := Environment{GOOS: "linux", HomeDir: "/home/runner"}
	_, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: environment,
		Endpoint:    "unix:///home/runner/.docker/desktop/docker.sock",
		Info:        DockerInfo{OperatingSystem: "Docker Desktop"},
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{"one":"/mnt/one/Docker.raw","nested":{"two":"/mnt/two/Docker.raw"}}`), nil
		},
		Stat: func(path string) (os.FileInfo, error) { return fakeFileInfo{name: base("linux", path)}, nil },
	})
	if err == nil || !errors.Is(err, ErrInvalidDockerStorage) || !strings.Contains(err.Error(), "multiple existing backing disks") {
		t.Fatalf("ambiguous settings error = %v", err)
	}
}

func TestDiscoverDockerDesktopRejectsMalformedSettings(t *testing.T) {
	_, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: Environment{GOOS: "linux", HomeDir: "/home/runner"},
		Endpoint:    "unix:///home/runner/.docker/desktop/docker.sock",
		Info:        DockerInfo{OperatingSystem: "Docker Desktop"},
		ReadFile:    func(string) ([]byte, error) { return []byte(`{"unterminated":`), nil },
	})
	if err == nil || !errors.Is(err, ErrInvalidDockerStorage) || !strings.Contains(err.Error(), "decode Docker Desktop settings store") {
		t.Fatalf("malformed settings error = %v", err)
	}
}

func TestDiscoverDockerDesktopUsesObservedDocumentedDefault(t *testing.T) {
	environment := Environment{GOOS: "darwin", HomeDir: "/Users/runner"}
	settings, disk, err := desktopPaths(environment)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: environment,
		Endpoint:    "unix:///Users/runner/.docker/run/docker.sock",
		Info:        DockerInfo{Name: "docker-desktop"},
		ReadFile: func(path string) ([]byte, error) {
			if path == settings {
				return nil, os.ErrNotExist
			}
			return nil, errors.New("unexpected read")
		},
		Stat: func(path string) (os.FileInfo, error) {
			if path == disk {
				return fakeFileInfo{name: "Docker.raw"}, nil
			}
			return nil, os.ErrNotExist
		},
		CapacityPath: func(string) (string, error) {
			return "/Users/runner/Library/Containers/com.docker.docker/Data/vms/0/data", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if storage.Roots[0].Path != disk || storage.Roots[0].Provenance != ProvenanceDocumentedDefaultObserved {
		t.Fatalf("default root = %#v", storage.Roots[0])
	}
}

func TestWindowsDockerDesktopDocumentedDefaultUsesCurrentWSLDiskRoot(t *testing.T) {
	environment := Environment{GOOS: "windows", HomeDir: `C:\Users\runner`, AppData: `C:\Users\runner\AppData\Roaming`, LocalAppData: `C:\Users\runner\AppData\Local`}
	_, disk, err := desktopPaths(environment)
	if err != nil {
		t.Fatal(err)
	}
	if want := `C:\Users\runner\AppData\Local\Docker\wsl\disk\docker_data.vhdx`; disk != want {
		t.Fatalf("documented Windows Docker Desktop disk = %q, want %q", disk, want)
	}
}

func TestDiscoverDockerDesktopWarnsOnAssumedDefaultAncestor(t *testing.T) {
	environment := Environment{GOOS: "linux", HomeDir: "/home/runner"}
	_, disk, err := desktopPaths(environment)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment:  environment,
		Endpoint:     "unix:///home/runner/.docker/desktop/docker.sock",
		Info:         DockerInfo{OperatingSystem: "Docker Desktop"},
		ReadFile:     func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Stat:         func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		CapacityPath: func(string) (string, error) { return "/home/runner", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	root := storage.Roots[0]
	if root.Path != disk || root.CapacityPath != "/home/runner" || root.Provenance != ProvenanceDocumentedDefaultAssumed || root.Confidence != ConfidenceAssumed {
		t.Fatalf("assumed root = %#v", root)
	}
	if len(root.Warnings) != 1 || !strings.Contains(root.Warnings[0], "nearest existing ancestor") {
		t.Fatalf("warnings = %v", root.Warnings)
	}
}

func TestDiscoverNativeDockerUsesDockerInfoAndActiveContainerdRoot(t *testing.T) {
	storage, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: Environment{GOOS: "linux", HomeDir: "/home/runner"},
		Endpoint:    "unix:///var/run/docker.sock",
		Info: DockerInfo{
			DockerRootDir: "/mnt/docker",
			DriverStatus:  [][]string{{"driver-type", "io.containerd.snapshotter.v1"}},
		},
		ReadFile: func(path string) ([]byte, error) {
			if path != "/etc/containerd/config.toml" {
				t.Fatalf("containerd config path = %q", path)
			}
			return []byte("version = 2\nroot = \"/mnt/containerd\"\n[grpc]\n"), nil
		},
		CapacityPath: func(path string) (string, error) { return path + "-physical", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if storage.Desktop || len(storage.Roots) != 2 {
		t.Fatalf("native storage = %#v", storage)
	}
	if got := storage.Roots[0]; got.Path != "/mnt/docker" || got.CapacityPath != "/mnt/docker-physical" || got.Provenance != ProvenanceDockerInfo {
		t.Fatalf("Docker root = %#v", got)
	}
	if got := storage.Roots[1]; got.Path != "/mnt/containerd" || got.CapacityPath != "/mnt/containerd-physical" || got.Provenance != ProvenanceContainerdConfig {
		t.Fatalf("containerd root = %#v", got)
	}
}

func TestDiscoverNativeDockerKeepsGuestOnlyDockerRootAsUnavailable(t *testing.T) {
	discovered, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: Environment{GOOS: "linux", HomeDir: "/home/runner"},
		Endpoint:    "unix:///var/run/docker.sock",
		Info:        DockerInfo{DockerRootDir: "/var/lib/docker"},
		Stat:        func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered.Roots) != 1 {
		t.Fatalf("roots = %#v, want one unknown engine root", discovered.Roots)
	}
	root := discovered.Roots[0]
	if root.ID != "engine" || root.Path != "/var/lib/docker" || root.CapacityPath != "" || root.Confidence != ConfidenceUnavailable {
		t.Fatalf("unknown engine root = %#v", root)
	}
	if !strings.Contains(root.CapacityUnavailableReason, "resolve Docker Engine capacity path") {
		t.Fatalf("unavailable reason = %q", root.CapacityUnavailableReason)
	}
}

func TestDiscoverNativeDockerPreservesKnownEngineWhenContainerdCapacityIsUnavailable(t *testing.T) {
	discovered, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: Environment{GOOS: "linux", HomeDir: "/home/runner"},
		Endpoint:    "unix:///var/run/docker.sock",
		Info: DockerInfo{
			DockerRootDir: "/mnt/docker",
			DriverStatus:  [][]string{{"driver-type", "io.containerd.snapshotter.v1"}},
		},
		ReadFile: func(string) ([]byte, error) {
			return []byte("root = \"/mnt/containerd\"\n"), nil
		},
		CapacityPath: func(path string) (string, error) {
			if path == "/mnt/docker" {
				return "/physical/docker", nil
			}
			return "", errors.New("statfs denied")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered.Roots) != 2 {
		t.Fatalf("roots = %#v, want engine and containerd", discovered.Roots)
	}
	if engine := discovered.Roots[0]; engine.CapacityPath != "/physical/docker" || engine.CapacityUnavailableReason != "" {
		t.Fatalf("engine root = %#v", engine)
	}
	containerd := discovered.Roots[1]
	if containerd.Path != "/mnt/containerd" || containerd.CapacityPath != "" || containerd.Provenance != ProvenanceContainerdConfig || containerd.Confidence != ConfidenceUnavailable {
		t.Fatalf("containerd root = %#v", containerd)
	}
	if !strings.Contains(containerd.CapacityUnavailableReason, "statfs denied") {
		t.Fatalf("containerd unavailable reason = %q", containerd.CapacityUnavailableReason)
	}
}

func TestDiscoverDockerDesktopKeepsBackingDiskWhenCapacityResolutionFails(t *testing.T) {
	environment := Environment{GOOS: "darwin", HomeDir: "/Users/runner"}
	settings, disk, err := desktopPaths(environment)
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: environment,
		Endpoint:    "unix:///Users/runner/.docker/run/docker.sock",
		Info:        DockerInfo{Name: "docker-desktop"},
		ReadFile: func(path string) ([]byte, error) {
			if path != settings {
				t.Fatalf("settings path = %q", path)
			}
			return nil, os.ErrNotExist
		},
		Stat: func(path string) (os.FileInfo, error) {
			if path == disk {
				return fakeFileInfo{name: "Docker.raw"}, nil
			}
			return nil, os.ErrNotExist
		},
		CapacityPath: func(string) (string, error) { return "", errors.New("statfs denied") },
	})
	if err != nil {
		t.Fatal(err)
	}
	root := discovered.Roots[0]
	if root.Path != disk || root.CapacityPath != "" || root.Provenance != ProvenanceDocumentedDefaultObserved || root.Confidence != ConfidenceUnavailable {
		t.Fatalf("Docker Desktop root = %#v", root)
	}
	if !strings.Contains(root.CapacityUnavailableReason, "statfs denied") {
		t.Fatalf("unavailable reason = %q", root.CapacityUnavailableReason)
	}
}

func TestDiscoverNativeDockerRejectsInvalidRelativeRoot(t *testing.T) {
	_, err := DiscoverDockerStorage(DockerDiscoveryOptions{
		Environment: Environment{GOOS: "linux"},
		Endpoint:    "unix:///var/run/docker.sock",
		Info:        DockerInfo{DockerRootDir: "var/lib/docker"},
	})
	if err == nil || !errors.Is(err, ErrInvalidDockerStorage) {
		t.Fatalf("invalid root error = %v", err)
	}
}
